package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/pkg/models"
)

func TestSyncCmdRegisteredWithSubcommands(t *testing.T) {
	cmd := findFleetSubcommand(rootCmd, "sync")
	require.NotNil(t, cmd)
	assert.Nil(t, findFleetSubcommand(rootCmd, "fleet"))

	for _, name := range []string{"serve", "publish", "status", "forget"} {
		assert.NotNil(t, findFleetSubcommand(cmd, name), "missing sync %s subcommand", name)
	}
}

func TestFleetStatusPublishesBeforeRenderingAndContinuesOnPublishWarning(t *testing.T) {
	resetFleetCommandDeps(t)

	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HostID:  "host-a",
		HubURL:  "http://hub.example.test",
	}}
	state := fleet.FleetState{Rows: []fleet.FleetRow{{
		ProjectIdentity: "github.com/kenn-io/kwt",
		ProjectName:     "kwt",
		Kind:            "branch",
		Ref:             "feature/fleet",
		Branch:          "feature/fleet",
		Observations: []fleet.Observation{
			{HostID: "host-a", Head: "aaa", ObservedAt: now.Add(-time.Minute)},
			{HostID: "host-b", Head: "bbb", Status: fleet.ChangeStatus{Modified: 1}, ObservedAt: now.Add(-2 * time.Minute)},
		},
	}}}
	sequence := []string{}
	client := &stubFleetClient{state: state, sequence: &sequence}

	loadFleetConfig = func() (*models.Config, error) {
		return cfg, nil
	}
	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		sequence = append(sequence, "builder")
		return &stubFleetManifestBuilder{}
	}
	publishFleetBestEffort = func(ctx context.Context, gotCfg *models.Config, builder fleet.ManifestBuildProvider, warn *bytes.Buffer) error {
		sequence = append(sequence, "publish")
		assert.Same(t, cfg, gotCfg)
		assert.NotNil(t, builder)
		return errors.New("publish failed")
	}
	newFleetClientFromConfig = func(gotCfg *models.Config) (fleetHubClient, error) {
		sequence = append(sequence, "client")
		assert.Same(t, cfg, gotCfg)
		return client, nil
	}
	fleetNow = func() time.Time {
		return now
	}

	cmd, stdout, stderr := fleetTestCommand()
	err := runFleetStatus(cmd, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"builder", "publish", "client", "state"}, sequence)
	assert.Contains(t, stderr.String(), "warning: sync publish failed: publish failed")
	assert.Contains(t, stdout.String(), "kwt")
	assert.Contains(t, stdout.String(), "different: host-b")
	assert.NotContains(t, stdout.String(), "differs from")
}

func TestFleetStatusSurfacesHubWarnings(t *testing.T) {
	resetFleetCommandDeps(t)

	cfg := &models.Config{Fleet: models.FleetConfig{
		Enabled: true,
		HostID:  "host-a",
		HubURL:  "http://hub.example.test",
	}}
	state := fleet.FleetState{Warnings: []fleet.Warning{{
		Code:    "host_id_collision",
		HostID:  "same",
		Message: "multiple machines are publishing as host ID \"same\"",
	}}}
	client := &stubFleetClient{state: state}

	loadFleetConfig = func() (*models.Config, error) { return cfg, nil }
	publishFleetBestEffort = func(context.Context, *models.Config, fleet.ManifestBuildProvider, *bytes.Buffer) error {
		return nil
	}
	newFleetClientFromConfig = func(*models.Config) (fleetHubClient, error) { return client, nil }

	cmd, _, stderr := fleetTestCommand()
	err := runFleetStatus(cmd, nil)

	require.NoError(t, err)
	assert.Contains(t, stderr.String(),
		`warning: multiple machines are publishing as host ID "same" (host same)`)
}

func TestFleetForgetDeletesHost(t *testing.T) {
	resetFleetCommandDeps(t)

	client := &stubFleetClient{}
	loadFleetConfig = func() (*models.Config, error) {
		return &models.Config{Fleet: models.FleetConfig{
			Enabled: true,
			HubURL:  "http://hub.example.test",
		}}, nil
	}
	newFleetClientFromConfig = func(*models.Config) (fleetHubClient, error) {
		return client, nil
	}

	cmd, _, _ := fleetTestCommand()
	err := runFleetForget(cmd, []string{"host-b"})

	require.NoError(t, err)
	assert.Equal(t, []string{"host-b"}, client.forgottenHosts)
}

func TestFleetPublishErrorsWhenFleetDisabledOrTokenMissing(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		resetFleetCommandDeps(t)

		loadFleetConfig = func() (*models.Config, error) {
			return &models.Config{Fleet: models.FleetConfig{Enabled: false}}, nil
		}
		newFleetClientFromConfig = func(*models.Config) (fleetHubClient, error) {
			t.Fatal("publish must not create a fleet client when fleet is disabled")
			return nil, nil
		}

		cmd, _, _ := fleetTestCommand()
		err := runFleetPublish(cmd, nil)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "multi-machine sync is disabled")
	})

	t.Run("missing token", func(t *testing.T) {
		resetFleetCommandDeps(t)

		loadFleetConfig = func() (*models.Config, error) {
			return &models.Config{Fleet: models.FleetConfig{
				Enabled: true,
				HubURL:  "https://hub.example.test",
			}}, nil
		}
		newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
			t.Fatal("publish must validate the fleet client before building a manifest")
			return nil
		}

		cmd, _, _ := fleetTestCommand()
		err := runFleetPublish(cmd, nil)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "sync token is not configured")
	})
}

func TestDefaultNewFleetClientFromConfigRejectsPlaintextNonLoopbackHub(t *testing.T) {
	t.Setenv("KWT_FLEET_TOKEN", "secret")

	_, err := defaultNewFleetClientFromConfig(&models.Config{Fleet: models.FleetConfig{
		Enabled:  true,
		HubURL:   "http://192.0.2.10:8787",
		TokenEnv: "KWT_FLEET_TOKEN",
	}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext sync hub URL")
}

func TestFleetPublishBestEffortForCommandNoopsWhenFleetDisabled(t *testing.T) {
	resetFleetCommandDeps(t)

	newFleetManifestBuilder = func() fleet.ManifestBuildProvider {
		t.Fatal("disabled fleet publish must not build a manifest")
		return nil
	}
	publishFleetBestEffort = func(context.Context, *models.Config, fleet.ManifestBuildProvider, *bytes.Buffer) error {
		t.Fatal("disabled fleet publish must not call the publisher")
		return nil
	}

	cmd, _, stderr := fleetTestCommand()
	publishFleetBestEffortForCommand(cmd, &models.Config{Fleet: models.FleetConfig{Enabled: false}})

	assert.Empty(t, stderr.String())
}

func resetFleetCommandDeps(t *testing.T) {
	t.Helper()

	oldLoadFleetConfig := loadFleetConfig
	oldNewFleetManifestBuilder := newFleetManifestBuilder
	oldPublishFleetBestEffort := publishFleetBestEffort
	oldPublishFleetBestEffortForCommand := publishFleetBestEffortForCommand
	oldNewFleetClientFromConfig := newFleetClientFromConfig
	oldServeFleetHub := serveFleetHub
	oldFleetNow := fleetNow
	oldFleetHostname := fleetHostname

	t.Cleanup(func() {
		loadFleetConfig = oldLoadFleetConfig
		newFleetManifestBuilder = oldNewFleetManifestBuilder
		publishFleetBestEffort = oldPublishFleetBestEffort
		publishFleetBestEffortForCommand = oldPublishFleetBestEffortForCommand
		newFleetClientFromConfig = oldNewFleetClientFromConfig
		serveFleetHub = oldServeFleetHub
		fleetNow = oldFleetNow
		fleetHostname = oldFleetHostname
	})
}

func fleetTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}

func findFleetSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, cmd := range parent.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	return nil
}

type stubFleetManifestBuilder struct {
	manifest *fleet.Manifest
	err      error
}

func (b *stubFleetManifestBuilder) Build(context.Context, *models.Config) (*fleet.Manifest, error) {
	if b.manifest == nil || b.err != nil {
		return b.manifest, b.err
	}
	manifest := *b.manifest
	return &manifest, nil
}

type stubFleetClient struct {
	published      []fleet.Manifest
	forgottenHosts []string
	state          fleet.FleetState
	stateErr       error
	publishErr     error
	forgetErr      error
	sequence       *[]string
}

func (c *stubFleetClient) Publish(_ context.Context, manifest fleet.Manifest) error {
	c.published = append(c.published, manifest)
	return c.publishErr
}

func (c *stubFleetClient) State(context.Context, string) (fleet.FleetState, string, bool, error) {
	if c.sequence != nil {
		*c.sequence = append(*c.sequence, "state")
	}
	return c.state, "", false, c.stateErr
}

func (c *stubFleetClient) Forget(_ context.Context, hostID string) error {
	c.forgottenHosts = append(c.forgottenHosts, hostID)
	return c.forgetErr
}
