package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	"go.kenn.io/kwt/service"
)

const testProjectGeneration = "0123456789abcdef0123456789abcdef"

type projectRemovalProbe struct {
	state   tmux.ProtectedSessionState
	err     error
	sockets []string
}

func (p *projectRemovalProbe) probe(
	_ context.Context,
	socket string,
	_ string,
) (tmux.ProtectedSessionState, error) {
	p.sockets = append(p.sockets, socket)
	return p.state, p.err
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
			Project: pullrequest.Project{Identity: f.identity, Name: "widget", Path: f.path},
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

func TestProjectRemovalRejectsLiveProtectedSession(t *testing.T) {
	fixture := newProjectRemovalFixture(t, filepath.Join(t.TempDir(), "missing"), "github.com/acme/widget")
	fixture.writeProvenance()
	fixture.probe.state = tmux.ProtectedSessionLive

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
