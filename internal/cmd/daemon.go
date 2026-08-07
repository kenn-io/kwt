package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
)

type daemonController interface {
	Status(context.Context) (kwtdaemon.Observation, error)
	Start(context.Context) (kwtdaemon.Observation, error)
	Stop(context.Context) error
	Restart(context.Context) (kwtdaemon.Observation, error)
}

var newDaemonController = func() (daemonController, error) {
	home, err := config.CanonicalHome()
	if err != nil {
		return nil, err
	}
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return nil, err
	}
	build := currentBuildInfo()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return kwtdaemon.NewController(kwtdaemon.ControllerOptions{
		Home:        home,
		Build:       kwtdaemon.Build{Version: build.Version, Revision: build.Revision},
		Config:      snapshot.Config.Daemon,
		Executable:  executable,
		Environment: os.Environ(),
		Progress:    os.Stderr,
	}), nil
}

var daemonCmd = &cobra.Command{
	Use:               "daemon",
	Short:             "Manage the local kwt daemon",
	Args:              cobra.NoArgs,
	PersistentPreRunE: globalOnlyPreRun,
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the local kwt daemon",
	Args:  cobra.NoArgs,
	RunE:  withGracefulSignals(runDaemonStart),
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the local kwt daemon",
	Args:  cobra.NoArgs,
	RunE:  withGracefulSignals(runDaemonStop),
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the local kwt daemon",
	Args:  cobra.NoArgs,
	RunE:  withGracefulSignals(runDaemonRestart),
}

var daemonStatusJSON bool

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show local kwt daemon status",
	Args:  cobra.NoArgs,
	RunE:  runDaemonStatus,
}

func init() {
	rootCmd.AddCommand(daemonCmd)
	daemonCmd.AddCommand(
		daemonStartCmd,
		daemonStopCmd,
		daemonRestartCmd,
		daemonStatusCmd,
	)
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false, "Output JSON")
}

func runDaemonStart(cmd *cobra.Command, _ []string) error {
	controller, err := newDaemonController()
	if err != nil {
		return err
	}
	observation, err := controller.Start(cmd.Context())
	if err != nil {
		return err
	}
	return renderDaemonStatus(cmd, observation, false)
}

func runDaemonStop(cmd *cobra.Command, _ []string) error {
	controller, err := newDaemonController()
	if err != nil {
		return err
	}
	return controller.Stop(cmd.Context())
}

func runDaemonRestart(cmd *cobra.Command, _ []string) error {
	controller, err := newDaemonController()
	if err != nil {
		return err
	}
	observation, err := controller.Restart(cmd.Context())
	if err != nil {
		return err
	}
	return renderDaemonStatus(cmd, observation, false)
}

func runDaemonStatus(cmd *cobra.Command, _ []string) error {
	controller, err := newDaemonController()
	if err != nil {
		return err
	}
	observation, err := controller.Status(cmd.Context())
	if err != nil {
		return err
	}
	return renderDaemonStatus(cmd, observation, daemonStatusJSON)
}

func renderDaemonStatus(
	cmd *cobra.Command,
	observation kwtdaemon.Observation,
	jsonOutput bool,
) error {
	if jsonOutput {
		switch observation.State {
		case kwtdaemon.RuntimeAbsent:
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				State string `json:"state"`
			}{State: "stopped"})
		case kwtdaemon.RuntimeUnresponsive:
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				State    string `json:"state"`
				PID      int    `json:"pid"`
				Version  string `json:"version"`
				Endpoint string `json:"endpoint"`
			}{
				State:    "unresponsive",
				PID:      observation.Record.PID,
				Version:  observation.Record.Version,
				Endpoint: observation.Record.Endpoint().Address,
			})
		}
		status := observation.Status
		status.State = kwtdaemon.State(daemonStateName(observation))
		return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
	}
	state := daemonStateName(observation)
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "state: %s\n", state); err != nil {
		return err
	}
	if observation.State == kwtdaemon.RuntimeAbsent {
		return nil
	}
	if observation.State == kwtdaemon.RuntimeUnresponsive {
		_, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"pid: %d\nversion: %s\nendpoint: %s\n",
			observation.Record.PID,
			observation.Record.Version,
			observation.Record.Endpoint().Address,
		)
		return err
	}
	status := observation.Status
	_, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"pid: %d\nversion: %s\nendpoint: %s\nschema: %d (%s)\n"+
			"uptime: %s\nactive work: %d\nactive leases: %d\n",
		status.PID,
		status.Version,
		status.Endpoint,
		status.SchemaMajor,
		status.SchemaVersion,
		time.Duration(status.UptimeSeconds)*time.Second,
		status.ActiveWork,
		status.ActiveLeases,
	)
	if err != nil {
		return err
	}
	if status.DrainDeadline != nil {
		if _, err := fmt.Fprintf(
			cmd.OutOrStdout(),
			"drain deadline: %s\n",
			status.DrainDeadline.Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	if status.LastError != nil {
		_, err = fmt.Fprintf(
			cmd.OutOrStdout(),
			"last error: %s (%s)\n",
			status.LastError.Message,
			status.LastError.At.Format(time.RFC3339),
		)
	}
	return err
}

func daemonStateName(observation kwtdaemon.Observation) string {
	switch observation.State {
	case kwtdaemon.RuntimeAbsent:
		return "stopped"
	case kwtdaemon.RuntimeDraining:
		return "draining"
	case kwtdaemon.RuntimeIncompatible:
		return "incompatible"
	case kwtdaemon.RuntimeUnresponsive:
		return "unresponsive"
	}
	if observation.Status.State != "" {
		return string(observation.Status.State)
	}
	if observation.State == kwtdaemon.RuntimeReady {
		return "ready"
	}
	return "unknown"
}
