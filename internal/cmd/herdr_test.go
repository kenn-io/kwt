package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/herdrcore"
)

type fakeHerdrClient struct {
	sessions []herdrcore.Terminal
	opts     herdrcore.RunOptions
	attached string
	killed   []string
	err      error
}

func (f *fakeHerdrClient) Run(ctx context.Context, opts herdrcore.RunOptions) (*herdrcore.Terminal, error) {
	f.opts = opts
	return &herdrcore.Terminal{Address: "t1", State: "live"}, f.err
}
func (f *fakeHerdrClient) List(context.Context, bool) ([]herdrcore.Terminal, error) {
	return f.sessions, f.err
}
func (f *fakeHerdrClient) Attach(ctx context.Context, address string) error {
	f.attached = address
	return f.err
}
func (f *fakeHerdrClient) Kill(ctx context.Context, address string) error {
	f.killed = append(f.killed, address)
	return f.err
}

func TestHerdrRunWorktreeAndDetach(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	directory := t.TempDir()
	f := &fakeHerdrClient{}
	cmd := newHerdrCommand(f)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"run", "-w", directory, "--id", "tests", "--context", "build", "--no-detach", "--", "printf hi"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, directory, f.opts.Directory)
	assert.Equal(t, "printf hi", f.opts.Command)
	assert.Equal(t, "tests", f.opts.Identifier)
	assert.Contains(t, f.opts.ProtectedNames, "KWT_GITHUB_TOKEN")
	assert.NotEmpty(t, f.opts.Executable)
	assert.Equal(t, "t1", f.attached)
	assert.Contains(t, out.String(), "t1")
}

func TestHerdrListJSONIsEmptyArray(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	f := &fakeHerdrClient{}
	cmd := newHerdrCommand(f)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"list", "--json"})
	require.NoError(t, cmd.Execute())
	assert.JSONEq(t, `[]`, out.String())
}

func TestHerdrKillConfirmationAndAmbiguity(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	for _, tc := range []struct {
		name      string
		args      []string
		input     string
		wantErr   bool
		wantKills int
	}{
		{"cancel", []string{"kill", "t1"}, "n\n", false, 0},
		{"confirm", []string{"kill", "t1"}, "yes\n", false, 1},
		{"ambiguous", []string{"kill", "auth", "--force"}, "", true, 0},
		{"all", []string{"kill", "--all", "--force"}, "", false, 2},
		{"missing", []string{"kill", "--force"}, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHerdrClient{sessions: []herdrcore.Terminal{{Address: "t1", Labels: map[string]string{"kwt.identifier": "auth"}}, {Address: "t2", Labels: map[string]string{"kwt.identifier": "auth-test"}}}}
			cmd := newHerdrCommand(f)
			cmd.SetContext(t.Context())
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetIn(strings.NewReader(tc.input))
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Len(t, f.killed, tc.wantKills)
		})
	}
}

func TestHerdrAttachPropagatesFailure(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	f := &fakeHerdrClient{err: errors.New("core unavailable")}
	cmd := newHerdrCommand(f)
	cmd.SetContext(t.Context())
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"attach", "t1"})
	require.ErrorContains(t, cmd.Execute(), "core unavailable")
	assert.Empty(t, f.attached)
}

func TestHerdrHelperStripsBeforeShell(t *testing.T) {
	t.Setenv("KWT_GITHUB_TOKEN", "fixture-token")
	t.Setenv("CuStOm_ToKeN", "fixture-custom")
	command := herdrShellCommand(t.Context(), "echo fixture", []string{"KWT_GITHUB_TOKEN", "CUSTOM_TOKEN"})
	for _, entry := range command.Env {
		name, _, _ := strings.Cut(entry, "=")
		assert.NotEqual(t, "KWT_GITHUB_TOKEN", strings.ToUpper(name))
		assert.NotEqual(t, "CUSTOM_TOKEN", strings.ToUpper(name))
	}
	output, err := command.Output()
	require.NoError(t, err)
	assert.Equal(t, "fixture", strings.TrimSpace(string(output)))
}

func TestHerdrHelperBypassesConfiguration(t *testing.T) {
	cmd := newHerdrCommand(&fakeHerdrClient{})
	helper, _, err := cmd.Find([]string{"_run-command"})
	require.NoError(t, err)
	require.True(t, helper.Hidden)
	require.NotNil(t, helper.PersistentPreRunE)
	require.NoError(t, helper.PersistentPreRunE(helper, nil))
}

func TestHerdrListRejectsConflictingFormats(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	cmd := newHerdrCommand(&fakeHerdrClient{})
	cmd.SetContext(t.Context())
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"list", "--json", "--csv"})
	require.Error(t, cmd.Execute())
}

func TestHerdrWatchStopsWhenCanceled(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := newHerdrCommand(&fakeHerdrClient{})
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"list", "--watch"})
	require.ErrorIs(t, cmd.Execute(), context.Canceled)
}

func TestHerdrRunDoesNotAttachOnUnknownOutcome(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	f := &fakeHerdrClient{err: errors.New("outcome unknown")}
	cmd := newHerdrCommand(f)
	cmd.SetContext(t.Context())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"run", "--no-detach", "echo hi"})
	require.ErrorContains(t, cmd.Execute(), "outcome unknown")
	assert.Empty(t, f.attached)
}

func TestHerdrAttachRefusesProtectedWorkspace(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	f := &fakeHerdrClient{sessions: []herdrcore.Terminal{{Address: "t91", State: "live", Labels: map[string]string{"kwt.managed": "true", "kwt.context": "protected-workspace"}}}}
	cmd := newHerdrCommand(f)
	cmd.SetContext(t.Context())
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"attach", "t91"})
	require.ErrorContains(t, cmd.Execute(), "pr attach")
	assert.Empty(t, f.attached)
}
