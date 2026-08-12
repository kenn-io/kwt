package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

var (
	sshResolveJSON bool
	sshResolveUser string
	sshResolvePort int

	resolveSSHThroughDaemon = resolveSSHViaDaemon
)

var sshCmd = &cobra.Command{
	Use:               "ssh",
	Short:             "Inspect and manage SSH connectivity",
	Args:              cobra.NoArgs,
	PersistentPreRunE: globalOnlyPreRun,
}

var sshResolveCmd = &cobra.Command{
	Use:   "resolve <hostname>",
	Short: "Resolve the effective OpenSSH route",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return nil
		}
		return writeSSHResolveFailure(cmd, service.NewError(
			service.InvalidRequest,
			fmt.Sprintf("expected one SSH hostname, received %d", len(args)),
			false,
			nil,
			nil,
		))
	},
	RunE: withGracefulSignals(runSSHResolve),
}

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.AddCommand(sshResolveCmd)
	sshResolveCmd.Flags().StringVar(&sshResolveUser, "user", "", "Override the SSH user")
	sshResolveCmd.Flags().IntVar(&sshResolvePort, "port", 0, "Override the SSH port")
	sshResolveCmd.Flags().BoolVar(&sshResolveJSON, "json", false, "Output a machine-readable route snapshot")
	sshResolveCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeSSHResolveFailure(cmd, service.NewError(
			service.InvalidRequest, err.Error(), false, nil, err,
		))
	})
}

func runSSHResolve(cmd *cobra.Command, args []string) error {
	snapshot, err := resolveSSHThroughDaemon(cmd.Context(), kwt.SSHResolveRequest{
		Target: kwt.SSHTarget{
			Hostname: args[0], User: sshResolveUser, Port: sshResolvePort,
		},
	})
	if err != nil {
		return writeSSHResolveFailure(cmd, err)
	}
	if sshResolveJSON {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(snapshot)
	}
	for _, target := range snapshot.Targets {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), target.DisplayTarget); err != nil {
			return err
		}
	}
	return nil
}

func writeSSHResolveFailure(cmd *cobra.Command, err error) error {
	typed := service.AsError(err)
	exitCode := 1
	if typed.Code == service.InvalidRequest || typed.Code == service.SSHInvalidTarget {
		exitCode = 2
	}
	return writeCommandFailure(
		cmd,
		typed.Descriptor,
		exitCode,
		sshResolveJSON,
		"ssh resolve",
	)
}
