// Package cmd provides CLI commands for the kwt application.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"golang.org/x/term"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var (
	mergeCwdLocal = config.MergeCwdLocal
	configInitErr error

	stdinIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	stdoutIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdout.Fd()))
	}
	runRootTUI = runTUI
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "kwt",
	Short: "Git worktree manager",
	Long: `kwt is a CLI tool for efficiently managing Git worktrees.

Like how 'ghq' manages repository clones, kwt provides intuitive 
operations for creating, switching, and deleting worktrees using 
a fuzzy finder interface.`,
	Version: getVersionString(),
	Args:    cobra.NoArgs,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := requireConfigInitialization(); err != nil {
			return err
		}
		if cmd == cmd.Root() {
			return nil
		}
		return mergeCwdLocal()
	},
	RunE: runRoot,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	err := rootCmd.Execute()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			exitCode = coded.ExitCode()
		}
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

type signalExitError struct {
	code  int
	cause error
}

func (e *signalExitError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "command interrupted"
}

func (e *signalExitError) Unwrap() error { return e.cause }
func (e *signalExitError) ExitCode() int { return e.code }

func withGracefulSignals(
	run func(*cobra.Command, []string) error,
) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		parent := cmd.Context()
		ctx, cancel := context.WithCancel(parent)
		cmd.SetContext(ctx)

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		done := make(chan struct{})
		handlerDone := make(chan struct{})
		var signalExitCode atomic.Int32
		go func() {
			defer close(handlerDone)
			select {
			case received := <-signals:
				switch received {
				case os.Interrupt:
					signalExitCode.Store(130)
				case syscall.SIGTERM:
					signalExitCode.Store(143)
				}
				signal.Stop(signals)
				cancel()
			case <-done:
			}
		}()

		err := run(cmd, args)
		signal.Stop(signals)
		close(done)
		<-handlerDone
		cancel()
		cmd.SetContext(parent)
		if code := signalExitCode.Load(); code != 0 {
			return &signalExitError{code: int(code), cause: err}
		}
		return err
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.CompletionOptions.DisableDefaultCmd = true
}

func runRoot(cmd *cobra.Command, args []string) error {
	if stdinIsTerminal() && stdoutIsTerminal() {
		return runRootTUI(cmd, args)
	}
	return cmd.Help()
}

// initConfig reads in config file and ENV variables if set. Cobra's
// initializer callback cannot return an error, so command pre-run hooks
// propagate the stored failure without terminating the process directly.
func initConfig() {
	configInitErr = config.Init()
}

func requireConfigInitialization() error {
	if configInitErr != nil {
		return fmt.Errorf("initialize configuration: %w", configInitErr)
	}
	return nil
}

func globalOnlyPreRun(*cobra.Command, []string) error {
	return requireConfigInitialization()
}

type buildInfo struct {
	Version  string
	Revision string
	Date     string
	Display  string
}

func currentBuildInfo() buildInfo {
	buildVersion, buildRevision, buildDate := version, commit, date
	info, ok := debug.ReadBuildInfo()
	if ok {
		if buildVersion == "dev" &&
			info.Main.Version != "" && info.Main.Version != "(devel)" {
			buildVersion = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if buildRevision == "none" && setting.Value != "" {
					buildRevision = setting.Value
				}
			case "vcs.time":
				if buildDate == "unknown" && setting.Value != "" {
					buildDate = setting.Value
				}
			}
		}
	}
	displayRevision := buildRevision
	if len(displayRevision) > 7 {
		displayRevision = displayRevision[:7]
	}
	return buildInfo{
		Version:  buildVersion,
		Revision: buildRevision,
		Date:     buildDate,
		Display: fmt.Sprintf(
			"%s (commit: %s, built: %s)",
			buildVersion,
			displayRevision,
			buildDate,
		),
	}
}

// getVersionString returns a formatted version string using build info.
func getVersionString() string { return currentBuildInfo().Display }
