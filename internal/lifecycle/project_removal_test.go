package lifecycle

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/service"
)

const testProjectGeneration = "0123456789abcdef0123456789abcdef"

type projectRemovalProbe struct {
	mu       sync.Mutex
	state    tmux.ProtectedSessionState
	err      error
	sockets  []string
	names    [][]string
	tempDirs []string
}

func (p *projectRemovalProbe) probe(
	_ context.Context,
	socket string,
	_ string,
	names []string,
	tempDir string,
) (tmux.ProtectedSessionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sockets = append(p.sockets, socket)
	p.names = append(p.names, append([]string(nil), names...))
	p.tempDirs = append(p.tempDirs, tempDir)
	return p.state, p.err
}

func (p *projectRemovalProbe) setState(state tmux.ProtectedSessionState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
}

type projectRemovalFixture struct {
	t          *testing.T
	home       string
	path       string
	identity   string
	expansion  ExpansionContext
	probe      *projectRemovalProbe
	service    ProjectRemover
	provenance *pullrequest.FileStore
}

func newProjectRemovalFixture(t *testing.T, path, identity string) *projectRemovalFixture {
	t.Helper()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = '"+identity+"'\nname = 'widget'\npath = '"+path+"'\nlast_touched = 'before'\n",
	), 0o600))
	probe := &projectRemovalProbe{state: tmux.ProtectedSessionAbsent}
	return &projectRemovalFixture{
		t: t, home: home, path: path, identity: identity,
		expansion: testExpansion(t), probe: probe,
		service:    newProjectRemovalService(home, probe.probe),
		provenance: pullrequest.NewFileStore(filepath.Join(home, "pull-requests.json")),
	}
}

func (f *projectRemovalFixture) writeProvenance() {
	f.t.Helper()
	err := f.provenance.Update(context.Background(), func(records map[string]pullrequest.Provenance) error {
		records["github:acme/widget#1"] = pullrequest.Provenance{
			Repository: f.identity,
			Project:    pullrequest.Project{Identity: f.identity, Name: "widget", Path: f.path},
			Workspace: pullrequest.Workspace{
				Path:        filepath.Join(filepath.Dir(f.path), "worktree"),
				Generation:  testProjectGeneration,
				SessionName: "widget-pr-1",
			},
		}
		return nil
	})
	require.NoError(f.t, err)
}

func (f *projectRemovalFixture) request() ProjectRemovalRequest {
	return ProjectRemovalRequest{
		Path: f.path, ExpectedRepository: f.identity, Expansion: f.expansion,
	}
}

func TestProjectRemovalMissingCheckoutWithoutProtectedSessionSucceeds(t *testing.T) {
	fixture := newProjectRemovalFixture(t, filepath.Join(t.TempDir(), "repo "), "github.com/acme/widget")

	result, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	require.NoError(t, err)
	assert.Equal(t, fixture.path, result.Project.Path)
	assert.Equal(t, fixture.identity, result.Project.Repository)
	remaining, err := configSnapshot(fixture.home, fixture.expansion)
	require.NoError(t, err)
	assert.Empty(t, remaining.Projects)
}

func TestProjectRemovalRequiresExactPathAndRepository(t *testing.T) {
	fixture := newProjectRemovalFixture(t, "/repo ", "github.com/acme/widget")
	request := fixture.request()
	request.Path = "/repo"
	_, err := fixture.service.RemoveProject(context.Background(), request)
	assert.True(t, service.IsCode(err, service.ProjectNotFound))

	request = fixture.request()
	request.ExpectedRepository = "github.com/acme/other"
	_, err = fixture.service.RemoveProject(context.Background(), request)
	assert.True(t, service.IsCode(err, service.RegistrationChanged))
}

func TestProjectRemovalAcceptsEquivalentRepositoryCase(t *testing.T) {
	fixture := newProjectRemovalFixture(t, "/repo ", "github.com/Acme/Widget")
	request := fixture.request()
	request.ExpectedRepository = "github.com/acme/widget"

	result, err := fixture.service.RemoveProject(context.Background(), request)

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", result.Project.Repository)
}

func TestProjectRemovalAcceptsPublishedLiveIdentityForLegacyRegistration(t *testing.T) {
	home := t.TempDir()
	repository := t.TempDir()
	command := exec.Command("git", "init", "-b", "main", repository)
	require.NoError(t, command.Run())
	command = exec.Command(
		"git", "-C", repository, "remote", "add", "origin",
		"git@github.com:acme/widget.git",
	)
	require.NoError(t, command.Run())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[[projects]]\nrepository = 'legacy identity'\nname = 'widget'\npath = '"+repository+"'\n",
	), 0o600))
	expansion := testExpansion(t)
	inventory, err := NewSource(SourceOptions{Home: home}).Load(
		context.Background(),
		Request{
			View: ViewProjects, Expansion: expansion,
			UntrustedConfig: IgnoreUntrustedConfig,
		},
	)
	require.NoError(t, err)
	require.Len(t, inventory.Snapshot.Projects, 1)
	assert.Equal(t, "github.com/acme/widget", inventory.Snapshot.Projects[0].Repository)
	probe := &projectRemovalProbe{state: tmux.ProtectedSessionAbsent}
	remover := newProjectRemovalService(home, probe.probe)

	result, err := remover.RemoveProject(context.Background(), ProjectRemovalRequest{
		Path: repository, ExpectedRepository: inventory.Snapshot.Projects[0].Repository,
		Expansion: expansion,
	})

	require.NoError(t, err)
	assert.Equal(t, "github.com/acme/widget", result.Project.Repository)
}

func TestProjectRemovalRejectsLiveProtectedSession(t *testing.T) {
	fixture := newProjectRemovalFixture(t, filepath.Join(t.TempDir(), "missing"), "github.com/acme/widget")
	fixture.writeProvenance()
	fixture.probe.setState(tmux.ProtectedSessionLive)

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	typed := service.AsError(err)
	assert.Equal(t, service.ProtectedSessionLive, typed.Code)
	assert.False(t, typed.Retryable)
	assert.Equal(t, "widget-pr-1", typed.Details["session_name"])
	require.Len(t, fixture.probe.sockets, 1)
	remaining, loadErr := configSnapshot(fixture.home, fixture.expansion)
	require.NoError(t, loadErr)
	require.Len(t, remaining.Projects, 1)
}

func TestProjectRemovalProbeStripsConfiguredCredential(t *testing.T) {
	fixture := newProjectRemovalFixture(
		t, filepath.Join(t.TempDir(), "missing"), "github.com/acme/widget",
	)
	require.NoError(t, os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte(
		"[fleet]\ntoken_env = 'GHOSTHUB_AUTH'\n"+
			"[[projects]]\nrepository = '"+fixture.identity+"'\nname = 'widget'\npath = '"+fixture.path+"'\n",
	), 0o600))
	fixture.writeProvenance()

	request := fixture.request()
	request.Expansion.Environment["TMUX_TMPDIR"] = "/tmp/legacy-tmux"
	_, err := fixture.service.RemoveProject(context.Background(), request)

	require.NoError(t, err)
	require.Len(t, fixture.probe.names, 1)
	assert.Contains(t, fixture.probe.names[0], "GHOSTHUB_AUTH")
	assert.Equal(t, []string{"/tmp/legacy-tmux"}, fixture.probe.tempDirs)
}

func TestProjectRemovalRejectsTransferredAliasProtectedEndpoint(t *testing.T) {
	fixture := newProjectRemovalFixture(
		t,
		filepath.Join(t.TempDir(), "registered"),
		"github.com/legacy/widget",
	)
	workspacePath := filepath.Join(t.TempDir(), "moved-clone", "pr-7")
	require.NoError(t, fixture.provenance.Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["github:current/widget#7"] = pullrequest.Provenance{
				Repository: "github.com/current/widget",
				RepositoryAliases: []string{
					"github.com/legacy/widget",
					"github.com/current/widget",
				},
				Project: pullrequest.Project{
					Identity: "github.com/current/widget",
					Path:     filepath.Join(t.TempDir(), "moved-clone"),
				},
				Workspace: pullrequest.Workspace{
					Path: workspacePath, Generation: testProjectGeneration,
					SessionName: "widget-pr-7",
				},
			}
			return nil
		},
	))
	fixture.probe.setState(tmux.ProtectedSessionLive)

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	assert.True(t, service.IsCode(err, service.ProtectedSessionLive))
	require.Len(t, fixture.probe.sockets, 1)
	assert.Equal(
		t,
		tmux.ProtectedWorkspaceSocketName("widget-pr-7", workspacePath),
		fixture.probe.sockets[0],
	)
}

func TestProjectRemovalRejectsDisconnectedSamePathProvenance(t *testing.T) {
	fixture := newProjectRemovalFixture(
		t,
		filepath.Join(t.TempDir(), "registered"),
		"github.com/acme/widget",
	)
	require.NoError(t, fixture.provenance.Update(
		context.Background(),
		func(records map[string]pullrequest.Provenance) error {
			records["github:other/widget#8"] = pullrequest.Provenance{
				Repository: "github.com/other/widget",
				Project: pullrequest.Project{
					Identity: "github.com/other/widget",
					Path:     fixture.path,
				},
				Workspace: pullrequest.Workspace{
					Path:       filepath.Join(t.TempDir(), "pr-8"),
					Generation: testProjectGeneration, SessionName: "widget-pr-8",
				},
			}
			return nil
		},
	))

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	assert.True(t, service.IsCode(err, service.ProtectedEndpointInventoryIncomplete))
	remaining, loadErr := configSnapshot(fixture.home, fixture.expansion)
	require.NoError(t, loadErr)
	require.Len(t, remaining.Projects, 1)
}

func TestProtectedEstablishmentWinsBeforeProjectRemoval(t *testing.T) {
	fixture := newProjectRemovalFixture(
		t,
		filepath.Join(t.TempDir(), "registered"),
		"github.com/acme/widget",
	)
	fixture.writeProvenance()
	claim, err := ObserveProjectClaim(
		context.Background(), fixture.home, fixture.path, fixture.expansion,
	)
	require.NoError(t, err)
	operationStarted := make(chan struct{})
	finishEstablishment := make(chan struct{})
	operationDone := make(chan error, 1)
	go func() {
		release, acquireErr := AcquireRequiredProjectClaim(
			context.Background(), fixture.home, claim,
		)
		if acquireErr != nil {
			operationDone <- acquireErr
			return
		}
		close(operationStarted)
		<-finishEstablishment
		fixture.probe.setState(tmux.ProtectedSessionLive)
		operationDone <- release()
	}()
	<-operationStarted
	removalDone := make(chan error, 1)
	go func() {
		_, removeErr := fixture.service.RemoveProject(
			context.Background(), fixture.request(),
		)
		removalDone <- removeErr
	}()
	select {
	case err := <-removalDone:
		t.Fatalf("removal completed before protected establishment: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishEstablishment)
	require.NoError(t, <-operationDone)

	err = <-removalDone

	assert.True(t, service.IsCode(err, service.ProtectedSessionLive))
	remaining, loadErr := configSnapshot(fixture.home, fixture.expansion)
	require.NoError(t, loadErr)
	require.Len(t, remaining.Projects, 1)
}

func TestProjectRemovalRejectsIncompleteProvenance(t *testing.T) {
	fixture := newProjectRemovalFixture(t, filepath.Join(t.TempDir(), "missing"), "github.com/acme/widget")
	require.NoError(t, os.WriteFile(filepath.Join(fixture.home, "pull-requests.json"), []byte("{"), 0o600))

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	typed := service.AsError(err)
	assert.Equal(t, service.ProtectedEndpointInventoryIncomplete, typed.Code)
	assert.False(t, typed.Retryable)
}

func TestProjectRemovalCASLossPreservesReplacement(t *testing.T) {
	fixture := newProjectRemovalFixture(t, "/repo ", "github.com/acme/widget")
	serviceImpl := fixture.service.(*projectRemovalService)
	serviceImpl.beforeCAS = func() {
		replacement := []byte(
			"[[projects]]\nrepository = 'github.com/acme/replacement'\nname = 'replacement'\npath = '/repo '\n",
		)
		require.NoError(t, os.WriteFile(filepath.Join(fixture.home, "config.toml"), replacement, 0o600))
	}

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	assert.True(t, service.IsCode(err, service.RegistrationChanged))
	snapshot, loadErr := configSnapshot(fixture.home, fixture.expansion)
	require.NoError(t, loadErr)
	require.Len(t, snapshot.Projects, 1)
	assert.Equal(t, "github.com/acme/replacement", snapshot.Projects[0].Persisted.Repository)
}

func TestProjectRemovalDuplicateExactPathFailsClosed(t *testing.T) {
	fixture := newProjectRemovalFixture(t, "/repo ", "github.com/acme/widget")
	duplicate := []byte(
		"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'one'\npath = '/repo '\n" +
			"[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'two'\npath = '/repo '\n",
	)
	require.NoError(t, os.WriteFile(filepath.Join(fixture.home, "config.toml"), duplicate, 0o600))

	_, err := fixture.service.RemoveProject(context.Background(), fixture.request())

	typed := service.AsError(err)
	assert.Equal(t, service.UnregistrationFailed, typed.Code)
	assert.False(t, typed.Retryable)
}

func configSnapshot(home string, expansion ExpansionContext) (*config.GlobalSnapshot, error) {
	return config.LoadGlobalSnapshotAtWithExpansion(home, expansion.expandPath)
}
