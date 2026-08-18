package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

func TestSSHResolveCommandPassesStructuredTargetAndPrintsJSON(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON, oldUser, oldPort := sshResolveJSON, sshResolveUser, sshResolvePort
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON, sshResolveUser, sshResolvePort = oldJSON, oldUser, oldPort
	})
	sshResolveJSON, sshResolveUser, sshResolvePort = true, "deploy", 2200
	var got kwt.SSHResolveRequest
	resolveSSHThroughDaemon = func(_ context.Context, request kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		got = request
		return kwt.SSHRouteSnapshot{
			LogicalTarget: request.Target, RouteIdentity: "route-identity",
			ProjectionPolicy: kwt.SSHProjectionPolicyV1,
			ObservedAt:       time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC),
		}, nil
	}
	command, stdout, stderr := sshResolveTestCommand()

	err := runSSHResolve(command, []string{"build.example.test"})
	require.NoError(t, err)
	assert.Equal(t, kwt.SSHResolveRequest{Target: kwt.SSHTarget{
		Hostname: "build.example.test", User: "deploy", Port: 2200,
	}}, got)
	var snapshot kwt.SSHRouteSnapshot
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &snapshot))
	assert.Equal(t, "route-identity", snapshot.RouteIdentity)
	assert.Empty(t, stderr.String())
}

func TestSSHResolveCommandPrintsCredentialFreeHumanRoute(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON := sshResolveJSON
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON = oldJSON
	})
	sshResolveJSON = false
	resolveSSHThroughDaemon = func(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{Targets: []kwt.SSHResolvedTarget{{
			DisplayTarget: "relay@[2001:db8::42]:2200",
		}, {
			DisplayTarget: "deploy@build.internal:22",
		}}}, nil
	}
	command, stdout, _ := sshResolveTestCommand()
	require.NoError(t, runSSHResolve(command, []string{"build.example.test"}))
	assert.Equal(t, "relay@[2001:db8::42]:2200\ndeploy@build.internal:22\n", stdout.String())
}

func TestSSHResolveCommandPreservesStableJSONErrors(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON := sshResolveJSON
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshResolveJSON = oldJSON
	})
	sshResolveJSON = true
	resolveSSHThroughDaemon = func(context.Context, kwt.SSHResolveRequest) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{}, service.NewError(
			service.SSHInvalidTarget, "invalid SSH target", false, nil, errors.New("private"),
		)
	}
	command, stdout, stderr := sshResolveTestCommand()
	err := runSSHResolve(command, []string{"bad"})
	require.Error(t, err)
	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 2, coded.ExitCode())
	assert.NotEqual(t, 255, coded.ExitCode())
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.SSHInvalidTarget, envelope.Error.Code)
	assert.NotContains(t, stdout.String(), "private")
	assert.Contains(t, stderr.String(), "kwt ssh resolve: ssh_invalid_target")
}

func TestRequireSSHResolveCapabilityFailsClosed(t *testing.T) {
	err := requireSSHResolveCapability(kwtdaemon.Observation{})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.DaemonIncompatible))
}

type fakeSSHLeaseControl struct {
	touches    atomic.Int32
	holds      atomic.Int32
	releases   atomic.Int32
	touchErr   error
	holdErr    error
	releaseErr error
}

type failingSSHOutput struct{ err error }

func (w failingSSHOutput) Write([]byte) (int, error) { return 0, w.err }

func (c *fakeSSHLeaseControl) Touch(context.Context, string) error {
	c.touches.Add(1)
	return c.touchErr
}

func (c *fakeSSHLeaseControl) Hold(ctx context.Context, _ string) (io.ReadCloser, error) {
	if c.holdErr != nil {
		return nil, c.holdErr
	}
	c.holds.Add(1)
	reader, writer := io.Pipe()
	go func() {
		<-ctx.Done()
		_ = writer.CloseWithError(ctx.Err())
	}()
	return reader, nil
}

func (c *fakeSSHLeaseControl) Release(context.Context, string) error {
	c.releases.Add(1)
	return c.releaseErr
}

func stubShortSSHLease(t *testing.T, control sshLeaseControl) {
	t.Helper()
	oldResolve := resolveSSHThroughDaemon
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldCapture := captureSSHClientProcess
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		acquireSSHLeaseThroughDaemon = oldAcquire
		captureSSHClientProcess = oldCapture
	})
	captureSSHClientProcess = func() (string, []string, error) {
		return "/work", []string{"PATH=/usr/bin"}, nil
	}
	resolveSSHThroughDaemon = func(
		_ context.Context,
		request kwt.SSHResolveRequest,
	) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{
			LogicalTarget: request.Target,
			Targets:       []kwt.SSHResolvedTarget{{EffectiveTarget: request.Target}},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}, nil
	}
	acquireSSHLeaseThroughDaemon = func(
		context.Context,
		kwt.SSHLeaseRequest,
		kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		return kwtdaemon.SSHLeaseResult{
			LeaseID: "lease-one", Arguments: []string{"-S", "/private/control"},
		}, control, nil
	}
}

func TestSSHLeaseCommandStreamsPromptsAndReleasesOnEOF(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = "route-one"
	control := &fakeSSHLeaseControl{}
	acquireSSHLeaseThroughDaemon = func(
		ctx context.Context,
		request kwt.SSHLeaseRequest,
		callbacks kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		assert.Equal(t, "route-one", request.Snapshot.RouteIdentity)
		prompt := service.OperationPrompt{
			ID: "prompt-1", Kind: "password", Message: "Password:\x1b]52;c;ignored\x07",
			Sensitive: true,
		}
		require.NoError(t, callbacks.Event(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventPrompt, Prompt: &prompt,
		}))
		value, err := callbacks.Prompt(ctx, prompt)
		require.NoError(t, err)
		assert.Equal(t, "secret", value)
		result := kwtdaemon.SSHLeaseResult{
			LeaseID: "lease-1", RouteIdentity: "route-one", Generation: 9,
			Mode: kwt.SSHLeaseModeMultiplexed, Arguments: []string{"-S", "/private/control"},
		}
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, callbacks.Event(service.OperationEvent{
			OperationID: "operation-1", Sequence: 2,
			Kind: service.OperationEventComplete, Result: encoded,
		}))
		return result, control, nil
	}
	command, stdout, stderr := sshResolveTestCommand()
	command.SetIn(strings.NewReader(`{"prompt_id":"prompt-1","value":"secret"}` + "\n"))

	require.NoError(t, runSSHLease(command, []string{"build.example.test"}))
	assert.Equal(t, int32(1), control.releases.Load())
	assert.Equal(t, int32(0), control.touches.Load())
	assert.Empty(t, stderr.String())
	decoder := json.NewDecoder(stdout)
	var promptEvent, completeEvent service.OperationEvent
	require.NoError(t, decoder.Decode(&promptEvent))
	require.NoError(t, decoder.Decode(&completeEvent))
	assert.Equal(t, service.OperationEventPrompt, promptEvent.Kind)
	require.NotNil(t, promptEvent.Prompt)
	assert.Equal(t, "Password:\x1b]52;c;ignored\x07", promptEvent.Prompt.Message)
	assert.Equal(t, service.OperationEventComplete, completeEvent.Kind)
	_, err := decoder.Token()
	assert.ErrorIs(t, err, io.EOF)
}

func TestSSHLeaseResolvesRouteWhenIdentityIsOmitted(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	oldPolicy := sshLeaseProjectionPolicy
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
		sshLeaseProjectionPolicy = oldPolicy
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = ""
	sshLeaseProjectionPolicy = kwt.SSHProjectionPolicyV1
	resolveSSHThroughDaemon = func(
		_ context.Context,
		request kwt.SSHResolveRequest,
	) (kwt.SSHRouteSnapshot, error) {
		assert.Equal(t, "build.example.test", request.Target.Hostname)
		return kwt.SSHRouteSnapshot{
			LogicalTarget:    request.Target,
			RouteIdentity:    "resolved-route",
			ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}, nil
	}
	control := &fakeSSHLeaseControl{}
	acquireSSHLeaseThroughDaemon = func(
		_ context.Context,
		request kwt.SSHLeaseRequest,
		_ kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		assert.Equal(t, "resolved-route", request.Snapshot.RouteIdentity)
		return kwtdaemon.SSHLeaseResult{LeaseID: "lease-1"}, control, nil
	}
	command, _, _ := sshResolveTestCommand()
	command.SetIn(strings.NewReader(""))

	require.NoError(t, runSSHLease(command, []string{"build.example.test"}))
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHExecResolvesAcquiresAndRunsThroughLease(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldRun := runSSHClientProcess
	oldCapture := captureSSHClientProcess
	oldUser, oldPort := sshExecUser, sshExecPort
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		acquireSSHLeaseThroughDaemon = oldAcquire
		runSSHClientProcess = oldRun
		captureSSHClientProcess = oldCapture
		sshExecUser, sshExecPort = oldUser, oldPort
	})
	sshExecUser, sshExecPort = "deploy", 2200
	captureSSHClientProcess = func() (string, []string, error) {
		return "/work", []string{"PATH=/usr/bin"}, nil
	}
	resolveSSHThroughDaemon = func(
		_ context.Context,
		request kwt.SSHResolveRequest,
	) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{
			LogicalTarget: request.Target,
			Targets: []kwt.SSHResolvedTarget{{
				EffectiveTarget: kwt.SSHTarget{Hostname: "10.0.0.8", User: "runner", Port: 2222},
			}},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}, nil
	}
	control := &fakeSSHLeaseControl{}
	acquireSSHLeaseThroughDaemon = func(
		_ context.Context,
		request kwt.SSHLeaseRequest,
		_ kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		assert.Equal(t, kwt.SSHHostKeyPolicyStrict, request.HostKeyPolicy)
		assert.Equal(t, "route-one", request.Snapshot.RouteIdentity)
		return kwtdaemon.SSHLeaseResult{
			LeaseID: "lease-one", Arguments: []string{"-S", "/private/control"},
		}, control, nil
	}
	var gotExecutable string
	var gotArguments []string
	runSSHClientProcess = func(
		_ context.Context,
		executable string,
		arguments []string,
		_ string,
		_ []string,
		_ io.Reader,
		stdout io.Writer,
		_ io.Writer,
	) (int, bool, error) {
		gotExecutable = executable
		gotArguments = append([]string(nil), arguments...)
		_, _ = io.WriteString(stdout, "remote output\n")
		return 0, true, nil
	}
	command, stdout, _ := sshResolveTestCommand()
	command.SetIn(strings.NewReader("input"))

	require.NoError(t, runSSHExec(command, []string{
		"build.example.test", "printf 'ready\\n'",
	}))
	assert.Equal(t, "ssh", gotExecutable)
	assert.Equal(t, []string{
		"-S", "/private/control", "runner@10.0.0.8", "printf 'ready\\n'",
	}, gotArguments)
	assert.Equal(t, "remote output\n", stdout.String())
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHExecHoldsLeaseWhileClientRuns(t *testing.T) {
	oldRun := runSSHClientProcess
	t.Cleanup(func() {
		runSSHClientProcess = oldRun
	})
	control := &fakeSSHLeaseControl{}
	stubShortSSHLease(t, control)
	runSSHClientProcess = func(
		ctx context.Context,
		_ string,
		_ []string,
		_ string,
		_ []string,
		_ io.Reader,
		_ io.Writer,
		_ io.Writer,
	) (int, bool, error) {
		for control.holds.Load() == 0 {
			select {
			case <-ctx.Done():
				return -1, true, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return 0, true, nil
	}
	command, _, _ := sshResolveTestCommand()

	require.NoError(t, runSSHExec(command, []string{"build.example.test", "true"}))
	assert.Equal(t, int32(1), control.holds.Load())
	assert.Equal(t, int32(0), control.touches.Load())
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHExecDoesNotStartClientWhenLeaseHoldFails(t *testing.T) {
	oldRun := runSSHClientProcess
	t.Cleanup(func() {
		runSSHClientProcess = oldRun
	})
	control := &fakeSSHLeaseControl{holdErr: service.NewError(
		service.SSHConnectionChanged, "SSH connection changed", false, nil, nil,
	)}
	stubShortSSHLease(t, control)
	started := false
	runSSHClientProcess = func(
		_ context.Context,
		_ string,
		_ []string,
		_ string,
		_ []string,
		_ io.Reader,
		_ io.Writer,
		_ io.Writer,
	) (int, bool, error) {
		started = true
		return 0, true, nil
	}
	command, _, stderr := sshResolveTestCommand()

	err := runSSHExec(command, []string{"build.example.test", "true"})
	require.Error(t, err)
	assert.True(t, service.IsCode(err, service.SSHConnectionChanged))
	assert.Contains(t, stderr.String(), "ssh_connection_changed")
	assert.False(t, started)
	assert.Equal(t, int32(0), control.touches.Load())
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHCopyUsesStructuredSFTPBatch(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldRun := runSSHClientProcess
	oldCapture := captureSSHClientProcess
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		acquireSSHLeaseThroughDaemon = oldAcquire
		runSSHClientProcess = oldRun
		captureSSHClientProcess = oldCapture
	})
	workingDirectory := filepath.Join(t.TempDir(), "work")
	captureSSHClientProcess = func() (string, []string, error) {
		return workingDirectory, []string{"PATH=/usr/bin"}, nil
	}
	resolveSSHThroughDaemon = func(
		_ context.Context,
		request kwt.SSHResolveRequest,
	) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{
			LogicalTarget: request.Target,
			Targets: []kwt.SSHResolvedTarget{{
				EffectiveTarget: kwt.SSHTarget{Hostname: "2001:db8::8", User: "runner", Port: 2222},
			}},
			RouteIdentity: "route-one", ProjectionPolicy: kwt.SSHProjectionPolicyV1,
		}, nil
	}
	control := &fakeSSHLeaseControl{}
	acquireSSHLeaseThroughDaemon = func(
		context.Context,
		kwt.SSHLeaseRequest,
		kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		return kwtdaemon.SSHLeaseResult{
			LeaseID: "lease-one",
			Arguments: []string{
				"-F", "/dev/null", "-S", `C:\kwt control`, "-p", "2222",
			},
		}, control, nil
	}
	var got []string
	runSSHClientProcess = func(
		_ context.Context,
		executable string,
		arguments []string,
		_ string,
		_ []string,
		stdin io.Reader,
		_ io.Writer,
		_ io.Writer,
	) (int, bool, error) {
		assert.Equal(t, "sftp", executable)
		got = append([]string(nil), arguments...)
		batch, err := io.ReadAll(stdin)
		require.NoError(t, err)
		expectedSource := strings.NewReplacer(
			`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`,
		).Replace(filepath.Join(workingDirectory, `-kwt *[build]?`))
		assert.Equal(t, "put \""+expectedSource+"\" \"C:/staging/kwt.exe; touch /tmp/pwn\"\n", string(batch))
		return 0, true, nil
	}
	command, _, _ := sshResolveTestCommand()

	require.NoError(t, runSSHCopy(command, []string{
		"build.example.test", `-kwt *[build]?`, "C:/staging/kwt.exe; touch /tmp/pwn",
	}))
	assert.Equal(t, []string{
		"-F", "/dev/null", "-o", `ControlPath="C:\\kwt control"`, "-P", "2222",
		"-b", "-", "runner@[2001:db8::8]",
	}, got)
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHExecJSONKeepsRemoteOutputUnmodifiedWhenReleaseFails(t *testing.T) {
	oldRun := runSSHClientProcess
	oldJSON := sshExecJSON
	t.Cleanup(func() {
		runSSHClientProcess = oldRun
		sshExecJSON = oldJSON
	})
	sshExecJSON = true
	control := &fakeSSHLeaseControl{releaseErr: service.NewError(
		service.SSHCleanupFailed, "SSH cleanup failed", true, nil, nil,
	)}
	stubShortSSHLease(t, control)
	runSSHClientProcess = func(
		_ context.Context,
		_ string,
		_ []string,
		_ string,
		_ []string,
		_ io.Reader,
		stdout io.Writer,
		_ io.Writer,
	) (int, bool, error) {
		_, _ = io.WriteString(stdout, "remote output\n")
		return 0, true, nil
	}
	command, stdout, stderr := sshResolveTestCommand()

	err := runSSHExec(command, []string{"build.example.test", "true"})
	require.Error(t, err)
	assert.Equal(t, "remote output\n", stdout.String())
	assert.NotContains(t, stdout.String(), `"error"`)
	assert.Contains(t, stderr.String(), "ssh_cleanup_failed")
}

func TestSSHExecJSONPreservesTypedPreExecutionFailure(t *testing.T) {
	oldResolve := resolveSSHThroughDaemon
	oldJSON := sshExecJSON
	t.Cleanup(func() {
		resolveSSHThroughDaemon = oldResolve
		sshExecJSON = oldJSON
	})
	sshExecJSON = true
	resolveSSHThroughDaemon = func(
		context.Context,
		kwt.SSHResolveRequest,
	) (kwt.SSHRouteSnapshot, error) {
		return kwt.SSHRouteSnapshot{}, service.NewError(
			service.SSHInteractionRequired,
			"SSH interaction is required",
			false,
			nil,
			nil,
		)
	}
	command, stdout, _ := sshResolveTestCommand()

	err := runSSHExec(command, []string{"build.example.test", "true"})
	require.Error(t, err)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.SSHInteractionRequired, envelope.Error.Code)
}

func TestSSHExecJSONReportsClientStartFailure(t *testing.T) {
	oldRun := runSSHClientProcess
	oldJSON := sshExecJSON
	t.Cleanup(func() {
		runSSHClientProcess = oldRun
		sshExecJSON = oldJSON
	})
	sshExecJSON = true
	control := &fakeSSHLeaseControl{}
	stubShortSSHLease(t, control)
	runSSHClientProcess = func(
		context.Context,
		string,
		[]string,
		string,
		[]string,
		io.Reader,
		io.Writer,
		io.Writer,
	) (int, bool, error) {
		return -1, false, errors.New("SSH executable is unavailable")
	}
	command, stdout, _ := sshResolveTestCommand()

	err := runSSHExec(command, []string{"build.example.test", "true"})
	require.Error(t, err)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.Internal, envelope.Error.Code)
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHLeaseHumanPromptEscapesTerminalControls(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
	})
	sshLeaseJSON = false
	sshLeaseRouteIdentity = "route-one"
	message := "Password:\x1b]52;c;ignored\x07\r\nNext\x7f\u0085"
	acquireSSHLeaseThroughDaemon = func(
		ctx context.Context,
		_ kwt.SSHLeaseRequest,
		callbacks kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		value, err := callbacks.Prompt(ctx, service.OperationPrompt{
			ID: "prompt-1", Message: message, Sensitive: true,
		})
		require.NoError(t, err)
		assert.Equal(t, "secret", value)
		return kwtdaemon.SSHLeaseResult{}, nil, service.NewError(
			service.SSHPromptRejected, "SSH prompt rejected", false, nil, nil,
		)
	}
	command, _, stderr := sshResolveTestCommand()
	command.SetIn(strings.NewReader("secret\n"))

	err := runSSHLease(command, []string{"build.example.test"})
	require.Error(t, err)
	assert.Contains(t, stderr.String(),
		`Password:\x1b]52;c;ignored\x07\x0d\x0aNext\x7f\x85 `)
	assert.NotContains(t, stderr.String(), "\x1b")
	assert.NotContains(t, stderr.String(), "\x07")
	assert.NotContains(t, stderr.String(), "\r")
	assert.NotContains(t, stderr.String(), "\x7f")
	assert.NotContains(t, stderr.String(), "\u0085")
}

func TestSSHLeasePromptReadStopsWhenContextIsCanceled(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonMode), func(t *testing.T) {
			oldAcquire := acquireSSHLeaseThroughDaemon
			oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
			t.Cleanup(func() {
				acquireSSHLeaseThroughDaemon = oldAcquire
				sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
			})
			sshLeaseJSON = jsonMode
			sshLeaseRouteIdentity = "route-one"
			promptStarted := make(chan struct{})
			acquireSSHLeaseThroughDaemon = func(
				ctx context.Context,
				_ kwt.SSHLeaseRequest,
				callbacks kwtdaemon.OperationCallbacks,
			) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
				close(promptStarted)
				_, err := callbacks.Prompt(ctx, service.OperationPrompt{
					ID: "prompt-1", Message: "Password:", Sensitive: true,
				})
				return kwtdaemon.SSHLeaseResult{}, nil, err
			}
			reader, writer := io.Pipe()
			t.Cleanup(func() {
				_ = reader.Close()
				_ = writer.Close()
			})
			ctx, cancel := context.WithCancel(context.Background())
			command, _, _ := sshResolveTestCommand()
			command.SetContext(ctx)
			command.SetIn(reader)
			done := make(chan error, 1)
			go func() { done <- runSSHLease(command, []string{"build.example.test"}) }()
			<-promptStarted
			cancel()

			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("SSH prompt read did not stop after cancellation")
			}
		})
	}
}

func TestSSHLeaseJSONWritesOneCompactFailureRecord(t *testing.T) {
	for _, terminalEvent := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminalEvent), func(t *testing.T) {
			oldAcquire := acquireSSHLeaseThroughDaemon
			oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
			t.Cleanup(func() {
				acquireSSHLeaseThroughDaemon = oldAcquire
				sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
			})
			sshLeaseJSON = true
			sshLeaseRouteIdentity = "route-one"
			failure := service.Descriptor{
				Code: service.SSHConnectionFailed, Message: "connection failed",
			}
			acquireSSHLeaseThroughDaemon = func(
				_ context.Context,
				_ kwt.SSHLeaseRequest,
				callbacks kwtdaemon.OperationCallbacks,
			) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
				if terminalEvent {
					require.NoError(t, callbacks.Event(service.OperationEvent{
						OperationID: "operation-1", Sequence: 1,
						Kind: service.OperationEventComplete, Failure: &failure,
					}))
				}
				return kwtdaemon.SSHLeaseResult{}, nil, service.NewDescriptorError(failure, nil)
			}
			command, stdout, _ := sshResolveTestCommand()

			err := runSSHLease(command, []string{"build.example.test"})
			require.Error(t, err)
			assert.Equal(t, 1, strings.Count(stdout.String(), "\n"))
			assert.NotContains(t, stdout.String(), "\n  ")
			if terminalEvent {
				var event service.OperationEvent
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &event))
				assert.Equal(t, service.OperationEventComplete, event.Kind)
			} else {
				var envelope jsonErrorEnvelope
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
				assert.Equal(t, service.SSHConnectionFailed, envelope.Error.Code)
			}
		})
	}
}

func TestSSHLeaseReleasesAcquiredLeaseWhenTerminalOutputFails(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = "route-one"
	control := &fakeSSHLeaseControl{}
	writeErr := errors.New("output closed")
	acquireSSHLeaseThroughDaemon = func(
		_ context.Context,
		_ kwt.SSHLeaseRequest,
		callbacks kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		result := kwtdaemon.SSHLeaseResult{LeaseID: "lease-1"}
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		err = callbacks.Event(service.OperationEvent{
			OperationID: "operation-1", Sequence: 1,
			Kind: service.OperationEventComplete, Result: encoded,
		})
		return result, control, err
	}
	command, _, _ := sshResolveTestCommand()
	command.SetOut(failingSSHOutput{err: writeErr})

	err := runSSHLease(command, []string{"build.example.test"})
	require.Error(t, err)
	assert.Equal(t, int32(1), control.releases.Load())
	assert.Equal(t, int32(0), control.touches.Load())
}

func TestSSHLeaseJSONWritesHeartbeatFailure(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	oldHeartbeat := sshLeaseHeartbeatEvery
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
		sshLeaseHeartbeatEvery = oldHeartbeat
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = "route-one"
	sshLeaseHeartbeatEvery = time.Millisecond
	control := &fakeSSHLeaseControl{touchErr: service.NewError(
		service.SSHConnectionChanged, "SSH connection changed", false, nil, nil,
	)}
	acquireSSHLeaseThroughDaemon = func(
		context.Context,
		kwt.SSHLeaseRequest,
		kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		return kwtdaemon.SSHLeaseResult{LeaseID: "lease-1"}, control, nil
	}
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	command, stdout, _ := sshResolveTestCommand()
	command.SetIn(reader)

	err := runSSHLease(command, []string{"build.example.test"})
	require.Error(t, err)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.SSHConnectionChanged, envelope.Error.Code)
	assert.Equal(t, int32(1), control.touches.Load())
	assert.Equal(t, int32(1), control.releases.Load())
}

func TestSSHLeaseJSONWritesReleaseFailure(t *testing.T) {
	oldAcquire := acquireSSHLeaseThroughDaemon
	oldJSON, oldIdentity := sshLeaseJSON, sshLeaseRouteIdentity
	t.Cleanup(func() {
		acquireSSHLeaseThroughDaemon = oldAcquire
		sshLeaseJSON, sshLeaseRouteIdentity = oldJSON, oldIdentity
	})
	sshLeaseJSON = true
	sshLeaseRouteIdentity = "route-one"
	control := &fakeSSHLeaseControl{releaseErr: service.NewError(
		service.SSHCleanupFailed, "SSH cleanup failed", true, nil, nil,
	)}
	acquireSSHLeaseThroughDaemon = func(
		context.Context,
		kwt.SSHLeaseRequest,
		kwtdaemon.OperationCallbacks,
	) (kwtdaemon.SSHLeaseResult, sshLeaseControl, error) {
		return kwtdaemon.SSHLeaseResult{LeaseID: "lease-1"}, control, nil
	}
	command, stdout, _ := sshResolveTestCommand()
	command.SetIn(strings.NewReader(""))

	err := runSSHLease(command, []string{"build.example.test"})
	require.Error(t, err)
	var envelope jsonErrorEnvelope
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &envelope))
	assert.Equal(t, service.SSHCleanupFailed, envelope.Error.Code)
	assert.Equal(t, int32(0), control.touches.Load())
	assert.Equal(t, int32(1), control.releases.Load())
}

func sshResolveTestCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	root := &cobra.Command{Use: "kwt"}
	command := &cobra.Command{Use: "resolve"}
	root.AddCommand(command)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	return command, &stdout, &stderr
}
