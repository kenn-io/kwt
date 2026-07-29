package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/pkg/models"
)

const syncServiceName = "kwt-sync"

type fleetHubClient interface {
	Publish(context.Context, fleet.Manifest) error
	State(context.Context, string) (fleet.FleetState, string, bool, error)
	Forget(context.Context, string) error
}

var (
	loadFleetConfig         = config.Load
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		return fleet.NewManifestBuilder(fleet.ManifestBuilderOptions{})
	}
	publishFleetBestEffort = func(ctx context.Context, cfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		return fleet.PublishBestEffort(ctx, cfg, builder, warn)
	}
	publishFleetBestEffortForCommand = defaultPublishFleetBestEffortForCommand
	newFleetClientFromConfig         = defaultNewFleetClientFromConfig
	serveFleetHub                    = defaultServeFleetHub
	fleetNow                         = time.Now
	fleetHostname                    = os.Hostname
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync worktree metadata across machines",
	Args:  cobra.NoArgs,
}

var syncServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the sync hub in the foreground",
	Args:  cobra.NoArgs,
	RunE:  runFleetServe,
}

var syncPublishCmd = &cobra.Command{
	Use:   "publish",
	Short: "Publish this host's sync manifest",
	Args:  cobra.NoArgs,
	RunE:  runFleetPublish,
}

var syncStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show multi-machine sync status",
	Args:  cobra.NoArgs,
	RunE:  runFleetStatus,
}

var syncForgetCmd = &cobra.Command{
	Use:   "forget <host-id>",
	Short: "Remove a host from the sync hub",
	Args:  cobra.ExactArgs(1),
	RunE:  runFleetForget,
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.AddCommand(syncServeCmd, syncPublishCmd, syncStatusCmd, syncForgetCmd)
}

func runFleetServe(cmd *cobra.Command, args []string) error {
	cfg, err := loadFleetConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireFleetEnabled(cfg); err != nil {
		return err
	}
	return serveFleetHub(cmd.Context(), cfg)
}

func runFleetPublish(cmd *cobra.Command, args []string) error {
	cfg, err := loadFleetConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireFleetEnabled(cfg); err != nil {
		return err
	}

	client, err := newFleetClientFromConfig(cfg)
	if err != nil {
		return err
	}
	builder := newFleetManifestBuilder()
	if builder == nil {
		return errors.New("sync manifest builder is not configured")
	}
	manifest, err := builder.Build(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if manifest == nil {
		return errors.New("sync manifest builder returned nil manifest")
	}
	return client.Publish(cmd.Context(), *manifest)
}

func runFleetStatus(cmd *cobra.Command, args []string) error {
	cfg, err := loadFleetConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireFleetEnabled(cfg); err != nil {
		return err
	}

	publishFleetBestEffortForCommand(cmd, cfg)

	client, err := newFleetClientFromConfig(cfg)
	if err != nil {
		return err
	}
	state, _, notModified, err := client.State(cmd.Context(), "")
	if err != nil {
		return err
	}
	if notModified {
		return nil
	}
	currentHost, err := currentFleetHostID(cfg)
	if err != nil {
		return err
	}
	for _, warning := range state.Warnings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	rows := fleet.BuildStatusRows(state, currentHost, fleetNow())
	return fleet.RenderStatusTable(cmd.OutOrStdout(), rows)
}

func runFleetForget(cmd *cobra.Command, args []string) error {
	cfg, err := loadFleetConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireFleetEnabled(cfg); err != nil {
		return err
	}
	client, err := newFleetClientFromConfig(cfg)
	if err != nil {
		return err
	}
	return client.Forget(cmd.Context(), args[0])
}

func requireFleetEnabled(cfg *models.Config) error {
	if cfg == nil || !cfg.Fleet.Enabled {
		return errors.New("multi-machine sync is disabled")
	}
	return nil
}

func defaultPublishFleetBestEffortForCommand(cmd *cobra.Command, cfg *models.Config) {
	if cfg == nil || !cfg.Fleet.Enabled {
		return
	}
	builder := newFleetManifestBuilder()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var publishWarning bytes.Buffer
	if publishErr := publishFleetBestEffort(ctx, cfg, builder, &publishWarning); publishErr != nil {
		_, _ = fmt.Fprintf(&publishWarning, "warning: sync publish failed: %v\n", publishErr)
	}
	if publishWarning.Len() > 0 {
		_, _ = publishWarning.WriteTo(cmd.ErrOrStderr())
	}
}

func defaultNewFleetClientFromConfig(cfg *models.Config) (fleetHubClient, error) {
	if cfg == nil {
		return nil, errors.New("sync config is not loaded")
	}
	hubURL := fleet.EffectiveHubURL(cfg.Fleet)
	if strings.TrimSpace(hubURL) == "" {
		return nil, errors.New("sync hub URL is not configured")
	}
	if err := fleet.ValidateHubURL(hubURL); err != nil {
		return nil, err
	}
	token, err := fleet.LoadToken(cfg.Fleet)
	if err != nil {
		return nil, err
	}
	return fleet.NewClient(fleet.ClientOptions{HubURL: hubURL, Token: token}), nil
}

func currentFleetHostID(cfg *models.Config) (string, error) {
	if cfg != nil {
		if hostID := strings.TrimSpace(cfg.Fleet.HostID); hostID != "" {
			return fleet.NormalizeHostID(hostID)
		}
	}
	return fleet.DefaultHostID(fleetHostname)
}

func defaultServeFleetHub(ctx context.Context, cfg *models.Config) error {
	if cfg == nil {
		return errors.New("sync config is not loaded")
	}
	listenAddr := strings.TrimSpace(cfg.Fleet.Hub.ListenAddr)
	if listenAddr == "" {
		return errors.New("sync hub listen_addr is not configured")
	}
	storePath := strings.TrimSpace(cfg.Fleet.Hub.StorePath)
	if storePath == "" {
		return errors.New("sync hub store_path is not configured")
	}
	token, err := fleet.LoadToken(cfg.Fleet)
	if err != nil {
		return err
	}
	endpoint, err := fleet.ParseHubEndpoint(listenAddr)
	if err != nil {
		return err
	}

	runtimeStore := daemon.RuntimeStore{
		Dir:    filepath.Join(filepath.Dir(storePath), "runtime"),
		Prefix: syncServiceName,
	}
	listener, err := daemon.Listen(ctx, endpoint, daemon.WithRuntimeStore(runtimeStore))
	if err != nil {
		return fmt.Errorf("listen sync hub: %w", err)
	}
	defer func() { _ = listener.Close() }()

	record := daemon.NewRuntimeRecord(syncServiceName, version, endpoint)
	record.Address = endpoint.ConfigAddress()
	runtimePath, err := runtimeStore.Write(record)
	if err != nil {
		return fmt.Errorf("write sync runtime record: %w", err)
	}
	defer func() { _ = os.Remove(runtimePath) }()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-done:
		}
	}()

	server := fleet.NewServer(fleet.ServerOptions{
		Store:   fleet.NewFileStore(storePath),
		Token:   token,
		Service: syncServiceName,
		Version: version,
	})
	if err := http.Serve(listener, server); err != nil {
		if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
			return ctx.Err()
		}
		return err
	}
	return nil
}
