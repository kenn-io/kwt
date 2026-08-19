package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	internalssh "go.kenn.io/kwt/internal/ssh"
	"go.kenn.io/kwt/service"
	"golang.org/x/term"
)

var (
	sshResolveJSON           bool
	sshResolveUser           string
	sshResolvePort           int
	sshLeaseJSON             bool
	sshLeaseUser             string
	sshLeasePort             int
	sshLeaseRouteIdentity    string
	sshLeaseProjectionPolicy string
	sshLeaseHostKeyPolicy    string
	sshExecUser              string
	sshExecPort              int
	sshExecRouteIdentity     string
	sshExecHostKeyPolicy     string
	sshExecQuiet             bool
	sshExecJSON              bool
	sshCopyUser              string
	sshCopyPort              int
	sshCopyHostKeyPolicy     string
	sshCopyQuiet             bool
	sshCopyJSON              bool

	resolveSSHThroughDaemon      = resolveSSHViaDaemon
	acquireSSHLeaseThroughDaemon = acquireSSHLeaseViaDaemon
	runSSHClientProcess          = internalssh.RunClientProcess
	captureSSHClientProcess      = sshClientProcessContext
)

const sshLeaseHeartbeatInterval = 10 * time.Second

var sshLeaseHeartbeatEvery = sshLeaseHeartbeatInterval

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

var sshLeaseCmd = &cobra.Command{
	Use:   "lease <hostname>",
	Short: "Hold a daemon-owned SSH connection lease",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return nil
		}
		return writeSSHLeaseFailure(cmd, service.NewError(
			service.InvalidRequest,
			fmt.Sprintf("expected one SSH hostname, received %d", len(args)),
			false, nil, nil,
		))
	},
	RunE: withGracefulSignals(runSSHLease),
}

var sshExecCmd = &cobra.Command{
	Use:   "exec <hostname> <command> [arguments...]",
	Short: "Run a command through a daemon-owned SSH connection",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) >= 2 {
			return nil
		}
		return writeSSHClientFailure(cmd, "ssh exec", sshExecJSON, service.NewError(
			service.InvalidRequest,
			fmt.Sprintf("expected an SSH hostname and command, received %d arguments", len(args)),
			false, nil, nil,
		))
	},
	RunE: withGracefulSignals(runSSHExec),
}

var sshCopyCmd = &cobra.Command{
	Use:   "copy <hostname> <source> <destination>",
	Short: "Copy a file through a daemon-owned SSH connection",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) == 3 {
			return nil
		}
		return writeSSHClientFailure(cmd, "ssh copy", sshCopyJSON, service.NewError(
			service.InvalidRequest,
			fmt.Sprintf("expected an SSH hostname, source, and destination, received %d arguments", len(args)),
			false, nil, nil,
		))
	},
	RunE: withGracefulSignals(runSSHCopy),
}

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.AddCommand(sshResolveCmd)
	sshCmd.AddCommand(sshLeaseCmd)
	sshCmd.AddCommand(sshExecCmd)
	sshCmd.AddCommand(sshCopyCmd)
	sshResolveCmd.Flags().StringVar(&sshResolveUser, "user", "", "Override the SSH user")
	sshResolveCmd.Flags().IntVar(&sshResolvePort, "port", 0, "Override the SSH port")
	sshResolveCmd.Flags().BoolVar(&sshResolveJSON, "json", false, "Output a machine-readable route snapshot")
	sshResolveCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeSSHResolveFailure(cmd, service.NewError(
			service.InvalidRequest, err.Error(), false, nil, err,
		))
	})
	sshLeaseCmd.Flags().StringVar(&sshLeaseUser, "user", "", "Override the SSH user")
	sshLeaseCmd.Flags().IntVar(&sshLeasePort, "port", 0, "Override the SSH port")
	sshLeaseCmd.Flags().StringVar(
		&sshLeaseRouteIdentity, "route-identity", "", "Require this resolved SSH route identity",
	)
	sshLeaseCmd.Flags().StringVar(
		&sshLeaseProjectionPolicy, "projection-policy", kwt.SSHProjectionPolicyV1,
		"Require this SSH execution projection policy",
	)
	sshLeaseCmd.Flags().StringVar(
		&sshLeaseHostKeyPolicy, "host-key-policy", string(kwt.SSHHostKeyPolicyReview),
		"Host-key handling: review or strict",
	)
	sshLeaseCmd.Flags().BoolVar(
		&sshLeaseJSON, "json", false, "Stream machine-readable lifecycle events",
	)
	sshLeaseCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeSSHLeaseFailure(cmd, service.NewError(
			service.InvalidRequest, err.Error(), false, nil, err,
		))
	})
	sshExecCmd.Flags().StringVar(&sshExecUser, "user", "", "Override the SSH user")
	sshExecCmd.Flags().IntVar(&sshExecPort, "port", 0, "Override the SSH port")
	sshExecCmd.Flags().StringVar(
		&sshExecRouteIdentity, "route-identity", "", "Require this resolved SSH route identity",
	)
	sshExecCmd.Flags().StringVar(
		&sshExecHostKeyPolicy, "host-key-policy", string(kwt.SSHHostKeyPolicyStrict),
		"Host-key handling: review or strict",
	)
	sshExecCmd.Flags().BoolVar(&sshExecQuiet, "quiet", false, "Suppress connection status output")
	sshExecCmd.Flags().BoolVar(&sshExecJSON, "json", false, "Emit kwt failures as JSON on stdout")
	sshExecCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeSSHClientFailure(cmd, "ssh exec", sshExecJSON, service.NewError(
			service.InvalidRequest, err.Error(), false, nil, err,
		))
	})
	sshCopyCmd.Flags().StringVar(&sshCopyUser, "user", "", "Override the SSH user")
	sshCopyCmd.Flags().IntVar(&sshCopyPort, "port", 0, "Override the SSH port")
	sshCopyCmd.Flags().StringVar(
		&sshCopyHostKeyPolicy, "host-key-policy", string(kwt.SSHHostKeyPolicyStrict),
		"Host-key handling: review or strict",
	)
	sshCopyCmd.Flags().BoolVar(&sshCopyQuiet, "quiet", false, "Suppress connection status output")
	sshCopyCmd.Flags().BoolVar(&sshCopyJSON, "json", false, "Emit kwt failures as JSON on stdout")
	sshCopyCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeSSHClientFailure(cmd, "ssh copy", sshCopyJSON, service.NewError(
			service.InvalidRequest, err.Error(), false, nil, err,
		))
	})
}

type sshClientExitError struct{ code int }

func (e *sshClientExitError) Error() string {
	return fmt.Sprintf("SSH client exited with status %d", e.code)
}
func (e *sshClientExitError) ExitCode() int { return e.code }

func runSSHExec(cmd *cobra.Command, args []string) (returnErr error) {
	ctx := commandContext(cmd)
	clientStarted := false
	target := kwt.SSHTarget{Hostname: args[0], User: sshExecUser, Port: sshExecPort}
	snapshot, result, control, err := acquireShortSSHLease(
		cmd, target, kwt.SSHHostKeyPolicy(sshExecHostKeyPolicy),
		sshExecRouteIdentity, sshExecQuiet,
	)
	if err != nil {
		return writeSSHClientFailure(cmd, "ssh exec", sshExecJSON, err)
	}
	defer func() {
		if returnErr != nil && !isSSHClientExit(returnErr) {
			returnErr = writeSSHClientFailure(
				cmd, "ssh exec", sshExecJSON && !clientStarted, returnErr,
			)
		}
		if releaseErr := releaseShortSSHLease(ctx, control, result.LeaseID); releaseErr != nil {
			returnErr = errors.Join(
				returnErr,
				writeSSHClientFailure(cmd, "ssh exec", false, releaseErr),
			)
		}
	}()
	workingDirectory, environment, err := captureSSHClientProcess()
	if err != nil {
		return err
	}
	arguments := append([]string(nil), result.Arguments...)
	arguments = append(arguments, sshExecutionDestination(snapshot))
	arguments = append(arguments, args[1:]...)
	exitCode, started, err := runShortSSHClient(
		ctx, control, result.LeaseID,
		func(processContext context.Context) (int, bool, error) {
			return runSSHClientProcess(
				processContext, "ssh", arguments, workingDirectory, environment,
				cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
			)
		},
	)
	clientStarted = started
	return sshClientProcessResult(cmd, exitCode, err)
}

func runSSHCopy(cmd *cobra.Command, args []string) (returnErr error) {
	ctx := commandContext(cmd)
	clientStarted := false
	target := kwt.SSHTarget{Hostname: args[0], User: sshCopyUser, Port: sshCopyPort}
	snapshot, result, control, err := acquireShortSSHLease(
		cmd, target, kwt.SSHHostKeyPolicy(sshCopyHostKeyPolicy), "", sshCopyQuiet,
	)
	if err != nil {
		return writeSSHClientFailure(cmd, "ssh copy", sshCopyJSON, err)
	}
	defer func() {
		if returnErr != nil && !isSSHClientExit(returnErr) {
			returnErr = writeSSHClientFailure(
				cmd, "ssh copy", sshCopyJSON && !clientStarted, returnErr,
			)
		}
		if releaseErr := releaseShortSSHLease(ctx, control, result.LeaseID); releaseErr != nil {
			returnErr = errors.Join(
				returnErr,
				writeSSHClientFailure(cmd, "ssh copy", false, releaseErr),
			)
		}
	}()
	workingDirectory, environment, err := captureSSHClientProcess()
	if err != nil {
		return err
	}
	source := args[1]
	if !filepath.IsAbs(source) {
		source = filepath.Join(workingDirectory, source)
	}
	batch, err := sftpPutBatch(filepath.Clean(source), args[2])
	if err != nil {
		return service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	arguments := sftpLeaseArguments(result.Arguments)
	arguments = append(arguments, "-b", "-", sftpDestination(snapshot))
	exitCode, started, err := runShortSSHClient(
		ctx, control, result.LeaseID,
		func(processContext context.Context) (int, bool, error) {
			return runSSHClientProcess(
				processContext, "sftp", arguments, workingDirectory, environment,
				strings.NewReader(batch), cmd.OutOrStdout(), cmd.ErrOrStderr(),
			)
		},
	)
	clientStarted = started
	return sshClientProcessResult(cmd, exitCode, err)
}

func acquireShortSSHLease(
	cmd *cobra.Command,
	target kwt.SSHTarget,
	policy kwt.SSHHostKeyPolicy,
	expectedRouteIdentity string,
	quiet bool,
) (kwt.SSHRouteSnapshot, kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
	ctx := commandContext(cmd)
	snapshot, err := resolveSSHThroughDaemon(ctx, kwt.SSHResolveRequest{Target: target})
	if err != nil {
		return kwt.SSHRouteSnapshot{}, kwtdaemon.SSHLeaseResult{}, nil, err
	}
	if expectedRouteIdentity != "" && snapshot.RouteIdentity != expectedRouteIdentity {
		return kwt.SSHRouteSnapshot{}, kwtdaemon.SSHLeaseResult{}, nil, service.NewError(
			service.SSHConfigurationChanged,
			"SSH configuration changed",
			true,
			nil,
			nil,
		)
	}
	callbacks := kwtdaemon.OperationCallbacks{Event: func(event service.OperationEvent) error {
		if quiet {
			return nil
		}
		if event.Kind != service.OperationEventProgress && event.Kind != service.OperationEventWarning {
			return nil
		}
		_, err := fmt.Fprintln(cmd.ErrOrStderr(), event.Message)
		return err
	}}
	input := cmd.InOrStdin()
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		reader := bufio.NewReader(input)
		callbacks.Prompt = func(ctx context.Context, prompt service.OperationPrompt) (string, error) {
			if _, err := fmt.Fprint(
				cmd.ErrOrStderr(), terminalSafeSSHPrompt(prompt.Message)+" ",
			); err != nil {
				return "", err
			}
			if prompt.Sensitive {
				value, err := readTerminalPassword(ctx, file)
				_, _ = fmt.Fprintln(cmd.ErrOrStderr())
				return value, err
			}
			value, err := readSSHPrompt(ctx, func() (string, error) {
				return reader.ReadString('\n')
			})
			return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), err
		}
	}
	result, control, err := acquireSSHLeaseThroughDaemon(ctx, kwt.SSHLeaseRequest{
		Snapshot: snapshot, HostKeyPolicy: policy,
	}, callbacks)
	return snapshot, result, control, err
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func sshClientProcessContext() (string, []string, error) {
	snapshot, err := config.LoadGlobalSnapshot()
	if err != nil {
		return "", nil, err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	workingDirectory, err = filepath.Abs(workingDirectory)
	if err != nil {
		return "", nil, err
	}
	environment := credentials.StripEnvironment(
		os.Environ(), credentials.ProtectedNames(snapshot.Config),
	)
	return workingDirectory, environment, nil
}

func sshExecutionDestination(snapshot kwt.SSHRouteSnapshot) string {
	if len(snapshot.Targets) == 0 {
		return snapshot.LogicalTarget.Display()
	}
	destination, _ := snapshot.Targets[len(snapshot.Targets)-1].EffectiveTarget.CommandDestination()
	return destination
}

func sftpDestination(snapshot kwt.SSHRouteSnapshot) string {
	target := snapshot.LogicalTarget
	if len(snapshot.Targets) > 0 {
		target = snapshot.Targets[len(snapshot.Targets)-1].EffectiveTarget
	}
	hostname := target.Hostname
	if strings.Contains(hostname, ":") && !strings.HasPrefix(hostname, "[") {
		hostname = "[" + hostname + "]"
	}
	if target.User != "" {
		hostname = target.User + "@" + hostname
	}
	return hostname
}

func sftpLeaseArguments(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if index+1 < len(arguments) && argument == "-S" {
			result = append(result, "-o", "ControlPath="+sshConfigQuotedValue(arguments[index+1]))
			index++
			continue
		}
		if index+1 < len(arguments) && argument == "-p" {
			result = append(result, "-P", arguments[index+1])
			index++
			continue
		}
		result = append(result, argument)
	}
	return result
}

func sftpPutBatch(source, destination string) (string, error) {
	source, err := sftpBatchPath(source)
	if err != nil {
		return "", fmt.Errorf("invalid source path: %w", err)
	}
	destination, err = sftpBatchPath(destination)
	if err != nil {
		return "", fmt.Errorf("invalid destination path: %w", err)
	}
	return "put " + source + " " + destination + "\n", nil
}

func sftpBatchPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("path contains a line or null character")
	}
	escaped := strings.NewReplacer(
		`\`, `\\`, `"`, `\"`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`,
	).Replace(path)
	return `"` + escaped + `"`, nil
}

func sshConfigQuotedValue(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return `"` + escaped + `"`
}

func runShortSSHClient(
	ctx context.Context,
	control sshLeaseControl,
	leaseID string,
	run func(context.Context) (int, bool, error),
) (int, bool, error) {
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	if control == nil || leaseID == "" {
		return run(processContext)
	}
	holdContext, releaseHold := context.WithCancel(processContext)
	hold, err := control.Hold(holdContext, leaseID)
	if err != nil {
		releaseHold()
		return -1, false, err
	}
	holdDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(io.Discard, hold)
		if holdContext.Err() != nil {
			holdDone <- nil
			return
		}
		cancel()
		if readErr == nil {
			readErr = io.ErrUnexpectedEOF
		}
		holdDone <- service.NewError(
			service.SSHConnectionChanged,
			"SSH connection changed",
			false,
			nil,
			readErr,
		)
	}()
	exitCode, started, processErr := run(processContext)
	releaseHold()
	_ = hold.Close()
	holdErr := <-holdDone
	if holdErr != nil {
		return -1, started, errors.Join(holdErr, processErr)
	}
	return exitCode, started, processErr
}

func releaseShortSSHLease(ctx context.Context, control sshLeaseControl, leaseID string) error {
	if control == nil || leaseID == "" {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return control.Release(releaseCtx, leaseID)
}

func sshClientProcessResult(cmd *cobra.Command, exitCode int, err error) error {
	if err == nil {
		return nil
	}
	if exitCode >= 0 {
		cmd.Root().SilenceUsage = true
		cmd.Root().SilenceErrors = true
		return &sshClientExitError{code: exitCode}
	}
	return err
}

func isSSHClientExit(err error) bool {
	var exitErr *sshClientExitError
	return errors.As(err, &exitErr)
}

func writeSSHClientFailure(cmd *cobra.Command, prefix string, jsonRequested bool, err error) error {
	typed := service.AsError(err)
	exitCode := 1
	if typed.Code == service.InvalidRequest || typed.Code == service.SSHInvalidTarget {
		exitCode = 2
	}
	return writeCommandFailure(cmd, typed.Descriptor, exitCode, jsonRequested, prefix)
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

func runSSHLease(cmd *cobra.Command, args []string) (returnErr error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	target := kwt.SSHTarget{
		Hostname: args[0], User: sshLeaseUser, Port: sshLeasePort,
	}
	snapshot := kwt.SSHRouteSnapshot{
		LogicalTarget: target, RouteIdentity: sshLeaseRouteIdentity,
		ProjectionPolicy: sshLeaseProjectionPolicy,
	}
	if sshLeaseRouteIdentity == "" {
		var err error
		snapshot, err = resolveSSHThroughDaemon(ctx, kwt.SSHResolveRequest{Target: target})
		if err != nil {
			return writeSSHLeaseFailure(cmd, err)
		}
	}
	input := cmd.InOrStdin()
	inputReader := bufio.NewReader(input)
	decoder := json.NewDecoder(inputReader)
	encoder := json.NewEncoder(cmd.OutOrStdout())
	terminalEventWritten := false
	callbacks := kwtdaemon.OperationCallbacks{
		Event: func(event service.OperationEvent) error {
			if sshLeaseJSON {
				if err := encoder.Encode(event); err != nil {
					return err
				}
				if event.Kind == service.OperationEventComplete {
					terminalEventWritten = true
				}
				return nil
			}
			if event.Kind == service.OperationEventProgress ||
				event.Kind == service.OperationEventWarning {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), event.Message)
				return err
			}
			return nil
		},
		Prompt: func(ctx context.Context, prompt service.OperationPrompt) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if sshLeaseJSON {
				var response service.OperationResponse
				if _, err := readSSHPrompt(ctx, func() (string, error) {
					return "", decoder.Decode(&response)
				}); err != nil {
					return "", err
				}
				if response.PromptID != prompt.ID {
					return "", service.NewError(
						service.InvalidRequest,
						"SSH prompt response does not match the active prompt",
						false, nil, nil,
					)
				}
				return response.Value, nil
			}
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), terminalSafeSSHPrompt(prompt.Message)+" "); err != nil {
				return "", err
			}
			if prompt.Sensitive {
				if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
					value, err := readTerminalPassword(ctx, file)
					_, _ = fmt.Fprintln(cmd.ErrOrStderr())
					return value, err
				}
			}
			value, err := readSSHPrompt(ctx, func() (string, error) {
				return inputReader.ReadString('\n')
			})
			return strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r"), err
		},
	}
	result, control, err := acquireSSHLeaseThroughDaemon(
		ctx,
		kwt.SSHLeaseRequest{
			HostKeyPolicy: kwt.SSHHostKeyPolicy(sshLeaseHostKeyPolicy),
			Snapshot:      snapshot,
		},
		callbacks,
	)
	if err != nil {
		if control != nil && result.LeaseID != "" {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			err = errors.Join(err, control.Release(releaseCtx, result.LeaseID))
			cancel()
		}
		return writeSSHLeaseFailureRecord(cmd, err, sshLeaseJSON && !terminalEventWritten)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		returnErr = errors.Join(returnErr, control.Release(releaseCtx, result.LeaseID))
		if returnErr != nil {
			returnErr = writeSSHLeaseFailureRecord(cmd, returnErr, sshLeaseJSON)
		}
	}()
	if !sshLeaseJSON {
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	stdinDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, io.MultiReader(decoder.Buffered(), inputReader))
		stdinDone <- err
	}()
	ticker := time.NewTicker(sshLeaseHeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stdinDone:
			return err
		case <-ticker.C:
			if err := control.Touch(ctx, result.LeaseID); err != nil {
				return err
			}
		}
	}
}

func terminalSafeSSHPrompt(message string) string {
	const hexadecimal = "0123456789abcdef"
	var safe strings.Builder
	for _, character := range message {
		if character > 0x1f && (character < 0x7f || character > 0x9f) {
			safe.WriteRune(character)
			continue
		}
		safe.WriteString(`\x`)
		safe.WriteByte(hexadecimal[byte(character)>>4])
		safe.WriteByte(hexadecimal[byte(character)&0x0f])
	}
	return safe.String()
}

type sshPromptRead struct {
	value string
	err   error
}

func readSSHPrompt(ctx context.Context, read func() (string, error)) (string, error) {
	result := make(chan sshPromptRead, 1)
	go func() {
		value, err := read()
		result <- sshPromptRead{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case response := <-result:
		return response.value, response.err
	}
}

func writeSSHLeaseFailure(cmd *cobra.Command, err error) error {
	return writeSSHLeaseFailureRecord(cmd, err, sshLeaseJSON)
}

func writeSSHLeaseFailureRecord(cmd *cobra.Command, err error, emitJSON bool) error {
	typed := service.AsError(err)
	exitCode := 1
	if typed.Code == service.InvalidRequest || typed.Code == service.SSHInvalidTarget {
		exitCode = 2
	}
	cmd.Root().SilenceUsage = true
	cmd.Root().SilenceErrors = true
	if emitJSON {
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(jsonErrorEnvelope{Error: typed.Descriptor})
	}
	_, _ = fmt.Fprintf(
		cmd.ErrOrStderr(),
		"kwt ssh lease: %s: %s\n",
		typed.Code,
		typed.Message,
	)
	return errors.Join(&commandFailure{descriptor: typed.Descriptor, exitCode: exitCode}, err)
}
