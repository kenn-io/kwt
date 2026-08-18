package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
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

	resolveSSHThroughDaemon      = resolveSSHViaDaemon
	acquireSSHLeaseThroughDaemon = acquireSSHLeaseViaDaemon
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

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.AddCommand(sshResolveCmd)
	sshCmd.AddCommand(sshLeaseCmd)
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
	if sshLeaseRouteIdentity == "" {
		return writeSSHLeaseFailure(cmd, service.NewError(
			service.InvalidRequest, "--route-identity is required", false, nil, nil,
		))
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
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
			Snapshot: kwt.SSHRouteSnapshot{
				LogicalTarget: kwt.SSHTarget{
					Hostname: args[0], User: sshLeaseUser, Port: sshLeasePort,
				},
				RouteIdentity:    sshLeaseRouteIdentity,
				ProjectionPolicy: sshLeaseProjectionPolicy,
			},
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
