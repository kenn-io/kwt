package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
)

var serveDaemonChild bool
var serveDaemonHost = kwtdaemon.Serve
var serveFlagErr error

var loadServeOptions = func() (kwtdaemon.ServeOptions, error) {
	home, err := config.CanonicalHome()
	if err != nil {
		return kwtdaemon.ServeOptions{}, err
	}
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return kwtdaemon.ServeOptions{}, err
	}
	build := currentBuildInfo()
	return kwtdaemon.ServeOptions{
		Home: home,
		Build: kwtdaemon.Build{
			Version:      build.Version,
			Revision:     build.Revision,
			RevisionTime: build.RevisionTime,
		},
		Config: snapshot.Config.Daemon,
	}, nil
}

var serveCmd = &cobra.Command{
	Use:               "serve",
	Short:             "Run the kwt service host in the foreground",
	Args:              cobra.NoArgs,
	PersistentPreRunE: globalOnlyPreRun,
	RunE:              withGracefulSignals(runServe),
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().BoolVar(&serveDaemonChild, "daemon-child", false, "")
	serveFlagErr = serveCmd.Flags().MarkHidden("daemon-child")
}

func runServe(cmd *cobra.Command, _ []string) error {
	if serveFlagErr != nil {
		return serveFlagErr
	}
	options, err := loadServeOptions()
	if err != nil {
		return err
	}
	options.Foreground = !serveDaemonChild
	options.Stdout = cmd.OutOrStdout()
	options.Stderr = cmd.ErrOrStderr()
	return serveDaemonHost(cmd.Context(), options)
}
