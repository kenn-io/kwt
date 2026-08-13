package pullrequest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	managedworktree "go.kenn.io/kit/git/managed"
	"go.kenn.io/kwt/internal/template"
	"go.kenn.io/kwt/internal/tmux"
	urlutil "go.kenn.io/kwt/internal/url"
)

func testProject() Project {
	return Project{
		Identity: "github.com/acme/widget",
		Name:     "widget",
		Path:     "/repos/widget",
	}
}

func testPR(number int, fork bool) PullRequest {
	sourceRepo := Repository{
		Provider: "github",
		Identity: "github.com/acme/widget",
		Host:     "github.com",
		Owner:    "acme",
		Name:     "widget",
		CloneURL: "https://github.com/acme/widget.git",
	}
	if fork {
		sourceRepo.Identity = "github.com/octocat/widget"
		sourceRepo.Owner = "octocat"
		sourceRepo.CloneURL = "https://github.com/octocat/widget.git"
	}
	return PullRequest{
		ID:       "github:github.com/acme/widget#" + itoa(number),
		Provider: "github",
		Repository: Repository{
			Provider: "github",
			Identity: "github.com/acme/widget",
			Host:     "github.com",
			Owner:    "acme",
			Name:     "widget",
			CloneURL: "https://github.com/acme/widget.git",
		},
		Number: number,
		URL:    "https://github.com/acme/widget/pull/" + itoa(number),
		Title:  "Improve widgets",
		Author: "octocat",
		Source: Branch{Repository: sourceRepo, Name: "feature/widgets"},
		Target: Branch{Repository: Repository{
			Provider: "github", Identity: "github.com/acme/widget", Host: "github.com",
			Owner: "acme", Name: "widget", CloneURL: "https://github.com/acme/widget.git",
		}, Name: "main"},
		State:   "open",
		HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	}
}

func TestImportBranchNameTruncatesAtUTF8Boundary(t *testing.T) {
	pr := testPR(42, false)
	pr.Source.Name = "a" + strings.Repeat("é", 40)

	branch := importBranchName(pr)
	slug := strings.TrimPrefix(branch, "pr-42-")

	assert.True(t, utf8.ValidString(branch))
	assert.LessOrEqual(t, len(slug), 80)
	assert.Equal(t, "a"+strings.Repeat("é", 39), slug)
}

func TestImportBranchNamePreservesCaseNeededForValidRef(t *testing.T) {
	pr := testPR(17, false)
	pr.Source.Name = "feature.LOCK"

	branch := importBranchName(pr)

	assert.Equal(t, "pr-17-feature.LOCK", branch)
	cmd := exec.Command("git", "check-ref-format", "--branch", branch)
	require.NoError(t, cmd.Run(), "generated branch %q must be a valid Git ref", branch)
}

func TestImportBranchNameTruncationDoesNotCreateLockSuffix(t *testing.T) {
	pr := testPR(18, false)
	pr.Source.Name = strings.Repeat("a", 75) + ".lockx"

	branch := importBranchName(pr)

	assert.False(t, strings.HasSuffix(strings.ToLower(branch), ".lock"))
	cmd := exec.Command("git", "check-ref-format", "--branch", branch)
	require.NoError(t, cmd.Run(), "generated branch %q must be a valid Git ref", branch)
}

type fakeProvider struct {
	prs      []PullRequest
	listErr  error
	getErr   error
	getCalls atomic.Int64
}

func (f *fakeProvider) List(context.Context, Repository, string) ([]PullRequest, error) {
	return append([]PullRequest(nil), f.prs...), f.listErr
}

func (f *fakeProvider) Get(_ context.Context, _ Repository, number int) (PullRequest, error) {
	f.getCalls.Add(1)
	if f.getErr != nil {
		return PullRequest{}, f.getErr
	}
	for _, pr := range f.prs {
		if pr.Number == number {
			return pr, nil
		}
	}
	return PullRequest{}, NewError(CodeNotFound, "pull request not found", false, nil)
}

type fakeWorkspaceBackend struct {
	mu                 sync.Mutex
	workspaces         []Workspace
	createCalls        int
	createErr          error
	createErrWorkspace Workspace
	rollbackErr        error
	importCalls        int
	importedPR         PullRequest
}

type guardedWorkspaceBackend struct {
	*fakeWorkspaceBackend
	guardCalls         int
	guardActive        bool
	rollbackUnderGuard bool
}

func (b *guardedWorkspaceBackend) Rollback(
	ctx context.Context,
	workspace Workspace,
) error {
	b.rollbackUnderGuard = b.guardActive
	return b.fakeWorkspaceBackend.Rollback(ctx, workspace)
}

func (b *guardedWorkspaceBackend) AcquireWorkspaceCreation(
	_ context.Context,
	_ string,
) (func() error, error) {
	b.guardCalls++
	b.guardActive = true
	return func() error {
		b.guardActive = false
		return nil
	}, nil
}

func newFakeBackend() *fakeWorkspaceBackend {
	return &fakeWorkspaceBackend{}
}

func (f *fakeWorkspaceBackend) ListWorkspaces(context.Context) ([]Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Workspace(nil), f.workspaces...), nil
}

func (f *fakeWorkspaceBackend) ImportPullRequest(
	_ context.Context, pr PullRequest, branch string,
) (Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importCalls++
	f.importedPR = pr
	f.createCalls++
	if f.createErr != nil {
		if f.createErrWorkspace.Path != "" {
			f.workspaces = append(f.workspaces, f.createErrWorkspace)
		}
		return f.createErrWorkspace, f.createErr
	}
	workspace := Workspace{
		ID:          "github.com/acme/widget:" + branch,
		Repository:  "github.com/acme/widget",
		Branch:      branch,
		Path:        "/worktrees/widget/" + branch,
		Generation:  "0123456789abcdef0123456789abcdef",
		State:       "ready",
		SessionName: "kwt-workspace-github-com-acme-widget-" + branch,
	}
	f.workspaces = append(f.workspaces, workspace)
	return workspace, nil
}

func (f *fakeWorkspaceBackend) Rollback(ctx context.Context, workspace Workspace) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.rollbackErr != nil {
		return f.rollbackErr
	}
	for i := range f.workspaces {
		if f.workspaces[i].Path == workspace.Path {
			f.workspaces = append(f.workspaces[:i], f.workspaces[i+1:]...)
			break
		}
	}
	return nil
}

type memoryStore struct {
	mu      sync.Mutex
	records map[string]Provenance
}

type guardObservingStore struct {
	*memoryStore
	guardActive         func() bool
	committedUnderGuard bool
}

func (s *guardObservingStore) Update(
	_ context.Context,
	fn func(map[string]Provenance) error,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := cloneRecords(s.records)
	if err := fn(copy); err != nil {
		return err
	}
	s.committedUnderGuard = s.guardActive()
	s.records = copy
	return nil
}

type commitFailStore struct {
	*memoryStore
}

func (s *commitFailStore) Update(ctx context.Context, fn func(map[string]Provenance) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := cloneRecords(s.records)
	if err := fn(copy); err != nil {
		return err
	}
	return errors.New("disk full")
}

func newMemoryStore() *memoryStore { return &memoryStore{records: make(map[string]Provenance)} }

func (s *memoryStore) View(_ context.Context, fn func(map[string]Provenance) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(cloneRecords(s.records))
}

func (s *memoryStore) Update(_ context.Context, fn func(map[string]Provenance) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := cloneRecords(s.records)
	if err := fn(copy); err != nil {
		return err
	}
	s.records = copy
	return nil
}

func cloneRecords(records map[string]Provenance) map[string]Provenance {
	copy := make(map[string]Provenance, len(records))
	for key, value := range records {
		copy[key] = value
	}
	return copy
}

func newTestService(provider Provider, backend WorkspaceBackend, store Store) *Service {
	return NewService(provider, backend, store)
}

func TestListReturnsSameRepoForkAndDraftDetails(t *testing.T) {
	same := testPR(11, false)
	fork := testPR(12, true)
	draft := testPR(13, false)
	draft.Draft = true
	service := newTestService(&fakeProvider{prs: []PullRequest{same, fork, draft}}, newFakeBackend(), newMemoryStore())

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "github.com/acme/widget", got[0].Repository.Identity)
	assert.False(t, got[0].Source.IsFork)
	assert.True(t, got[1].Source.IsFork)
	assert.Equal(t, "github.com/octocat/widget", got[1].Source.Repository.Identity)
	assert.True(t, got[2].Draft)
	assert.False(t, got[0].Imported)
}

func TestListMarksExistingImport(t *testing.T) {
	pr := testPR(21, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-21", Repository: testProject().Identity, Branch: "pr-21-feature-widgets", Path: "/worktrees/21", State: "ready", SessionName: "session-21"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: workspace, HeadSHA: pr.HeadSHA,
		SourceRepo: pr.Source.Repository.Identity, SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Imported)
	assert.Equal(t, &workspace, got[0].Workspace)
}

func TestImportPersistsWorkspaceGenerationInProvenance(t *testing.T) {
	pr := testPR(61, false)
	backend := newFakeBackend()
	backend.workspaces = nil
	store := newMemoryStore()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	result, err := service.Import(context.Background(), testProject(), "61")

	require.NoError(t, err)
	require.NotEmpty(t, result.Workspace.Generation)
	record := store.records[pr.ID]
	assert.Equal(t, result.Workspace.Generation, record.Workspace.Generation)
}

func TestImportHoldsWorkspaceCreationGuardThroughProvenanceCommit(t *testing.T) {
	pr := testPR(62, false)
	backend := &guardedWorkspaceBackend{fakeWorkspaceBackend: newFakeBackend()}
	store := &guardObservingStore{
		memoryStore: newMemoryStore(),
		guardActive: func() bool { return backend.guardActive },
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	result, err := service.Import(context.Background(), testProject(), "62")

	require.NoError(t, err)
	assert.NotEmpty(t, result.Workspace.Path)
	assert.Equal(t, 1, backend.guardCalls)
	assert.True(t, store.committedUnderGuard)
}

func TestImportHoldsWorkspaceCreationGuardThroughRollback(t *testing.T) {
	pr := testPR(63, false)
	backend := &guardedWorkspaceBackend{fakeWorkspaceBackend: newFakeBackend()}
	store := &commitFailStore{memoryStore: newMemoryStore()}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	_, err := service.Import(context.Background(), testProject(), "63")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.Equal(t, 1, backend.guardCalls)
	assert.True(t, backend.rollbackUnderGuard)
}

func TestListMatchesCanonicalProvenancePathThroughSymlink(t *testing.T) {
	pr := testPR(25, false)
	realBase := t.TempDir()
	workspacePath := filepath.Join(realBase, "workspace")
	require.NoError(t, os.Mkdir(workspacePath, 0o755))
	linkBase := filepath.Join(t.TempDir(), "linked-base")
	require.NoError(t, os.Symlink(realBase, linkBase))
	live := Workspace{ID: "ws-25", Repository: testProject().Identity, Branch: "pr-25-feature-widgets", Path: workspacePath, State: "ready"}
	recorded := live
	recorded.Path = filepath.Join(linkBase, "workspace")
	backend := newFakeBackend()
	backend.workspaces = []Workspace{live}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: recorded, HeadSHA: pr.HeadSHA,
		SourceRepo: pr.Source.Repository.Identity, SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, got[0].Imported)
	require.NotNil(t, got[0].Workspace)
	assert.Equal(t, workspacePath, got[0].Workspace.Path)
}

func TestListDoesNotMatchProvenanceWhenLiveBranchDiffers(t *testing.T) {
	pr := testPR(23, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-23", Repository: testProject().Identity, Branch: "different-branch", Path: "/worktrees/23", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	recorded := workspace
	recorded.Branch = "pr-23-feature-widgets"
	store.records[pr.ID] = Provenance{PullRequestID: pr.ID, Project: testProject(), Workspace: recorded, HeadSHA: pr.HeadSHA}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Imported)
	assert.Nil(t, got[0].Workspace)
}

func TestListDoesNotMatchProvenanceWhenLiveGenerationDiffers(t *testing.T) {
	pr := testPR(62, false)
	live := Workspace{
		ID: "ws-62", Repository: testProject().Identity,
		Branch: "pr-62-feature-widgets", Path: "/worktrees/62", State: "ready",
		Generation: "0123456789abcdef0123456789abcdef",
	}
	recorded := live
	recorded.Generation = "fedcba9876543210fedcba9876543210"
	backend := newFakeBackend()
	backend.workspaces = []Workspace{live}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: recorded,
		HeadSHA: pr.HeadSHA, SourceRepo: pr.Source.Repository.Identity,
		SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Imported)
}

func TestListDoesNotMarkImportAfterSourceBranchRename(t *testing.T) {
	pr := testPR(24, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-24", Repository: testProject().Identity, Branch: "pr-24-feature-old", Path: "/worktrees/24", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: workspace, HeadSHA: pr.HeadSHA,
		SourceRepo: pr.Source.Repository.Identity, SourceBranch: "feature/old",
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].Imported)
	assert.Nil(t, got[0].Workspace)
}

func TestListRecognizesLegacyCasedProvenance(t *testing.T) {
	pr := testPR(22, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-22", Repository: testProject().Identity, Branch: "pr-22-feature-widgets", Path: "/worktrees/22", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	legacyID := "github:github.com/Acme/Widget#22"
	store.records[legacyID] = Provenance{
		PullRequestID: legacyID, Provider: "github", Repository: "github.com/Acme/Widget", Number: 22,
		Project: Project{Identity: "github.com/Acme/Widget", Path: testProject().Path}, Workspace: workspace,
		SourceRepo: "github.com/Acme/Widget", SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Imported)
	assert.Equal(t, &workspace, got[0].Workspace)
}

func TestListRecognizesTransferredRepositoryProvenance(t *testing.T) {
	pr := testPR(26, false)
	backend := newFakeBackend()
	workspace := Workspace{
		ID: "ws-26", Repository: testProject().Identity,
		Branch: "pr-26-feature-widgets", Path: "/worktrees/26", State: "ready",
	}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	legacyIdentity := "github.com/legacy/widget"
	legacyID := OpaqueID(legacyIdentity, pr.Number)
	store.records[legacyID] = Provenance{
		PullRequestID: legacyID,
		Provider:      "github",
		Repository:    legacyIdentity,
		Number:        pr.Number,
		Project: Project{
			Identity: legacyIdentity,
			Path:     testProject().Path,
		},
		Workspace:    workspace,
		SourceRepo:   legacyIdentity,
		SourceBranch: pr.Source.Name,
	}
	service := NewService(
		&fakeProvider{prs: []PullRequest{pr}},
		backend,
		store,
		legacyIdentity,
		testProject().Identity,
	)

	got, err := service.List(context.Background(), testProject(), "open")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].Imported)
	assert.Equal(t, &workspace, got[0].Workspace)
}

func TestImportSameRepositoryUsesMatchingRemoteAndCanonicalName(t *testing.T) {
	pr := testPR(31, false)
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	result, err := service.Import(context.Background(), testProject(), "31")

	require.NoError(t, err)
	assert.Equal(t, ImportCreated, result.Status)
	assert.Equal(t, 1, backend.importCalls)
	assert.Equal(t, pr.ID, backend.importedPR.ID)
	assert.Equal(t, "pr-31-feature-widgets", result.Workspace.Branch)
	assert.Equal(t, testProject().Identity, result.Workspace.Repository)
}

func TestImportDelegatesGitLifecycleAsOneOperation(t *testing.T) {
	pr := testPR(35, true)
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	result, err := service.Import(context.Background(), testProject(), "35")

	require.NoError(t, err)
	assert.Equal(t, 1, backend.importCalls)
	assert.Equal(t, pr.ID, backend.importedPR.ID)
	assert.Equal(t, "pr-35-feature-widgets", result.Workspace.Branch)
}

func TestImportPropagatesUnsupportedGitFromSharedLifecycle(t *testing.T) {
	backend := newFakeBackend()
	backend.createErr = NewError(CodeUnsupportedGitVersion, "Git 2.20 or newer is required", false, nil)
	provider := &fakeProvider{prs: []PullRequest{testPR(30, false)}}
	service := newTestService(provider, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "30")

	assertErrorCode(t, err, CodeUnsupportedGitVersion)
	assert.Equal(t, 1, backend.importCalls)
	assert.Equal(t, int64(1), provider.getCalls.Load())
}

func TestGitHubRepositoryIdentityIsCaseInsensitive(t *testing.T) {
	project := testProject()
	project.Identity = "github.com/Acme/Widget"
	pr := testPR(33, false)
	pr.Repository.Identity = "github.com/ACME/WIDGET"
	pr.Source.Repository.Identity = "github.com/acme/widget"
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	result, err := service.Import(context.Background(), project, "https://github.com/aCmE/wIdGeT/pull/33")

	require.NoError(t, err)
	assert.False(t, result.PullRequest.Source.IsFork)
	assert.Equal(t, pr.ID, backend.importedPR.ID)
}

func TestParseSelectorAcceptsMixedCaseGitHubIdentity(t *testing.T) {
	for _, selector := range []string{
		"github:github.com/ACME/WIDGET#17",
		"https://github.com/AcMe/WiDgEt/pull/17",
	} {
		number, err := ParseSelector(selector, "github.com/acme/widget")
		require.NoError(t, err)
		assert.Equal(t, 17, number)
	}
}

func TestParseSelectorNumberValidatesShapeWithoutRepositoryIdentity(t *testing.T) {
	for _, selector := range []string{
		"17",
		"github:github.com/legacy/widget#17",
		"https://github.com/acme/widget/pull/17",
	} {
		number, err := ParseSelectorNumber(selector)
		require.NoError(t, err)
		assert.Equal(t, 17, number)
	}

	_, err := ParseSelectorNumber("https://github.com/acme/widget/issues/17")
	assertErrorCode(t, err, CodeInvalidSelector)
}

func TestImportForkCreatesAndUsesForkRemote(t *testing.T) {
	pr := testPR(32, true)
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	result, err := service.Import(context.Background(), testProject(), pr.URL)

	require.NoError(t, err)
	assert.Equal(t, ImportCreated, result.Status)
	assert.Equal(t, pr.Source.Repository.Identity, backend.importedPR.Source.Repository.Identity)
}

func TestImportIsIdempotent(t *testing.T) {
	pr := testPR(41, false)
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	first, err := service.Import(context.Background(), testProject(), pr.ID)
	require.NoError(t, err)
	second, err := service.Import(context.Background(), testProject(), pr.ID)
	require.NoError(t, err)

	assert.Equal(t, ImportCreated, first.Status)
	assert.Equal(t, ImportExisting, second.Status)
	assert.Equal(t, first.Workspace, second.Workspace)
	assert.Equal(t, 1, backend.createCalls)
}

func TestImportPreservesLegacyProtectedWorkspaceEndpoint(t *testing.T) {
	pr := testPR(41, false)
	branch := importBranchName(pr)
	livePath := "/worktrees/widget/" + branch
	recordedPath := "/worktrees/widget/alias/../" + branch
	info, ok := urlutil.CanonicalRepositoryInfo(testProject().Identity)
	require.True(t, ok)
	legacyName := "kwt-workspace-github-com-acme-widget-" + branch + "-" +
		template.ShortHash(recordedPath)
	legacySocket := tmux.ProtectedWorkspaceSocketName(legacyName, recordedPath)
	live := Workspace{
		ID:          "github.com/acme/widget:" + branch,
		Repository:  testProject().Identity,
		Branch:      branch,
		Path:        livePath,
		Generation:  "0123456789abcdef0123456789abcdef",
		State:       "ready",
		SessionName: tmux.WorkspaceSessionName(info, branch, livePath),
	}
	backend := newFakeBackend()
	backend.workspaces = []Workspace{live}
	store := newMemoryStore()
	recorded := live
	recorded.Path = recordedPath
	recorded.SessionName = legacyName
	recorded.TmuxSocketName = legacySocket
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID,
		Provider:      pr.Provider,
		Repository:    pr.Repository.Identity,
		Number:        pr.Number,
		URL:           pr.URL,
		HeadSHA:       pr.HeadSHA,
		SourceRepo:    pr.Source.Repository.Identity,
		SourceBranch:  pr.Source.Name,
		Project:       testProject(),
		Workspace:     recorded,
	}
	service := newTestService(
		&fakeProvider{prs: []PullRequest{pr}}, backend, store,
	)

	listed, err := service.List(context.Background(), testProject(), "open")
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NotNil(t, listed[0].Workspace)
	assert.Equal(t, recordedPath, listed[0].Workspace.Path)
	assert.Equal(t, legacyName, listed[0].Workspace.SessionName)
	assert.Equal(t, legacySocket, listed[0].Workspace.TmuxSocketName)

	result, err := service.Import(context.Background(), testProject(), pr.ID)
	require.NoError(t, err)
	assert.Equal(t, ImportExisting, result.Status)
	assert.Equal(t, recordedPath, result.Workspace.Path)
	assert.Equal(t, legacyName, result.Workspace.SessionName)
	assert.Equal(t, legacySocket, result.Workspace.TmuxSocketName)
	assert.Equal(t, recordedPath, store.records[pr.ID].Workspace.Path)
	assert.Equal(t, legacyName, store.records[pr.ID].Workspace.SessionName)
	assert.Equal(t, legacySocket, store.records[pr.ID].Workspace.TmuxSocketName)
	assert.Zero(t, backend.createCalls)
}

func TestImportDoesNotReturnExistingWorkspaceWhenBranchDiffers(t *testing.T) {
	pr := testPR(44, false)
	backend := newFakeBackend()
	live := Workspace{ID: "ws-other", Repository: testProject().Identity, Branch: "other-branch", Path: "/worktrees/44", State: "ready"}
	backend.workspaces = []Workspace{live}
	store := newMemoryStore()
	recorded := live
	recorded.Branch = "pr-44-feature-widgets"
	store.records[pr.ID] = Provenance{PullRequestID: pr.ID, Project: testProject(), Workspace: recorded, HeadSHA: pr.HeadSHA}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	result, err := service.Import(context.Background(), testProject(), "44")

	require.NoError(t, err)
	assert.Equal(t, ImportCreated, result.Status)
	assert.Equal(t, 1, backend.createCalls)
}

func TestImportRejectsExistingWorkspaceAfterSourceBranchRename(t *testing.T) {
	pr := testPR(45, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-45", Repository: testProject().Identity, Branch: "pr-45-feature-old", Path: "/worktrees/45", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: workspace, HeadSHA: pr.HeadSHA,
		SourceRepo: pr.Source.Repository.Identity, SourceBranch: "feature/old",
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	_, err := service.Import(context.Background(), testProject(), "45")

	assertErrorCode(t, err, CodeConflict)
	assert.ErrorContains(t, err, "source repository or branch changed")
	assert.Zero(t, backend.createCalls)
}

func TestImportRejectsExistingWorkspaceWithMissingSourceProvenance(t *testing.T) {
	pr := testPR(46, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-46", Repository: testProject().Identity, Branch: "pr-46-feature-widgets", Path: "/worktrees/46", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: workspace, HeadSHA: pr.HeadSHA,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	_, err := service.Import(context.Background(), testProject(), "46")

	assertErrorCode(t, err, CodeConflict)
	assert.ErrorContains(t, err, "source provenance")
	assert.Zero(t, backend.importCalls)
}

func TestImportExistingDoesNotReenterSharedLifecycle(t *testing.T) {
	pr := testPR(47, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-47", Repository: testProject().Identity, Branch: "pr-47-feature-widgets", Path: "/worktrees/47", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID, Project: testProject(), Workspace: workspace, HeadSHA: pr.HeadSHA,
		SourceRepo: pr.Source.Repository.Identity, SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	result, err := service.Import(context.Background(), testProject(), "47")

	require.NoError(t, err)
	assert.Equal(t, ImportExisting, result.Status)
	assert.Zero(t, backend.importCalls)
}

func TestImportRejectsProvenanceFromDifferentClone(t *testing.T) {
	pr := testPR(48, false)
	backend := newFakeBackend()
	store := newMemoryStore()
	otherClone := testProject()
	otherClone.Path = "/repos/other-widget-clone"
	store.records[pr.ID] = Provenance{
		PullRequestID: pr.ID,
		Project:       otherClone,
		Workspace: Workspace{
			Path:   "/worktrees/other-widget/pr-48-feature-widgets",
			Branch: "pr-48-feature-widgets",
		},
		HeadSHA:      pr.HeadSHA,
		SourceRepo:   pr.Source.Repository.Identity,
		SourceBranch: pr.Source.Name,
	}
	service := newTestService(
		&fakeProvider{prs: []PullRequest{pr}}, backend, store,
	)

	_, err := service.Import(t.Context(), testProject(), "48")

	assertErrorCode(t, err, CodeConflict)
	assert.Zero(t, backend.importCalls)
	assert.Equal(t, otherClone.Path, store.records[pr.ID].Project.Path)
}

func TestImportMigratesLegacyCasedProvenance(t *testing.T) {
	pr := testPR(43, false)
	backend := newFakeBackend()
	workspace := Workspace{ID: "ws-43", Repository: testProject().Identity, Branch: "pr-43-feature-widgets", Path: "/worktrees/43", State: "ready"}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	legacyID := "github:github.com/Acme/Widget#43"
	store.records[legacyID] = Provenance{
		PullRequestID: legacyID, Provider: "github", Repository: "github.com/Acme/Widget", Number: 43,
		Project: Project{Identity: "github.com/Acme/Widget", Path: testProject().Path}, Workspace: workspace,
		SourceRepo: "github.com/Acme/Widget", SourceBranch: pr.Source.Name,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	result, err := service.Import(context.Background(), testProject(), "43")

	require.NoError(t, err)
	assert.Equal(t, ImportExisting, result.Status)
	assert.Zero(t, backend.createCalls)
	assert.NotContains(t, store.records, legacyID)
	assert.Contains(t, store.records, pr.ID)
	assert.Equal(t, pr.ID, store.records[pr.ID].PullRequestID)
	assert.Equal(t, pr.Source.Repository.Identity, store.records[pr.ID].SourceRepo)
	assert.Equal(t, pr.Source.Name, store.records[pr.ID].SourceBranch)
}

func TestImportMigratesTransferredRepositoryProvenance(t *testing.T) {
	pr := testPR(49, false)
	backend := newFakeBackend()
	workspace := Workspace{
		ID: "ws-49", Repository: testProject().Identity,
		Branch: "pr-49-feature-widgets", Path: "/worktrees/49", State: "ready",
	}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	legacyIdentity := "github.com/legacy/widget"
	legacyID := OpaqueID(legacyIdentity, pr.Number)
	store.records[legacyID] = Provenance{
		PullRequestID: legacyID,
		Provider:      "github",
		Repository:    legacyIdentity,
		Number:        pr.Number,
		Project: Project{
			Identity: legacyIdentity,
			Path:     testProject().Path,
		},
		Workspace: Workspace{
			ID: "legacy-ws", Repository: legacyIdentity,
			Branch: workspace.Branch, Path: workspace.Path, State: "ready",
		},
		SourceRepo:   legacyIdentity,
		SourceBranch: pr.Source.Name,
	}
	service := NewService(
		&fakeProvider{prs: []PullRequest{pr}},
		backend,
		store,
		legacyIdentity,
		testProject().Identity,
	)

	result, err := service.Import(context.Background(), testProject(), "49")

	require.NoError(t, err)
	assert.Equal(t, ImportExisting, result.Status)
	assert.Zero(t, backend.createCalls)
	assert.NotContains(t, store.records, legacyID)
	require.Contains(t, store.records, pr.ID)
	migrated := store.records[pr.ID]
	assert.Equal(t, pr.ID, migrated.PullRequestID)
	assert.Equal(t, pr.Repository.Identity, migrated.Repository)
	assert.ElementsMatch(t, []string{
		legacyIdentity,
		testProject().Identity,
	}, migrated.RepositoryAliases)
	assert.Equal(t, testProject().Identity, migrated.Project.Identity)
	assert.Equal(t, pr.Source.Repository.Identity, migrated.SourceRepo)
	assert.Equal(t, workspace, migrated.Workspace)
}

func TestImportMigratesConsecutiveRepositoryTransfers(t *testing.T) {
	pr := testPR(54, false)
	backend := newFakeBackend()
	workspace := Workspace{
		ID: "ws-54", Repository: testProject().Identity,
		Branch: "pr-54-feature-widgets", Path: "/worktrees/54", State: "ready",
	}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	firstIdentity := "github.com/legacy/widget"
	intermediateIdentity := "github.com/middle/widget"
	intermediateID := OpaqueID(intermediateIdentity, pr.Number)
	store.records[intermediateID] = Provenance{
		PullRequestID: intermediateID,
		Provider:      "github",
		Repository:    intermediateIdentity,
		RepositoryAliases: []string{
			firstIdentity,
			intermediateIdentity,
		},
		Number: pr.Number,
		Project: Project{
			Identity: intermediateIdentity,
			Path:     testProject().Path,
		},
		Workspace: Workspace{
			ID: "middle-ws", Repository: intermediateIdentity,
			Branch: workspace.Branch, Path: workspace.Path, State: "ready",
		},
		SourceRepo:   intermediateIdentity,
		SourceBranch: pr.Source.Name,
	}
	service := NewService(
		&fakeProvider{prs: []PullRequest{pr}},
		backend,
		store,
		firstIdentity,
		testProject().Identity,
	)

	result, err := service.Import(context.Background(), testProject(), "54")

	require.NoError(t, err)
	assert.Equal(t, ImportExisting, result.Status)
	assert.Zero(t, backend.createCalls)
	assert.NotContains(t, store.records, intermediateID)
	require.Contains(t, store.records, pr.ID)
	migrated := store.records[pr.ID]
	assert.ElementsMatch(t, []string{
		firstIdentity,
		intermediateIdentity,
		testProject().Identity,
	}, migrated.RepositoryAliases)
	assert.Equal(t, pr.Repository.Identity, migrated.Repository)
	assert.Equal(t, testProject().Identity, migrated.Project.Identity)
	assert.Equal(t, pr.Source.Repository.Identity, migrated.SourceRepo)
}

func TestImportDoesNotTrustRepositoryOutsidePersistedAliasHistory(
	t *testing.T,
) {
	pr := testPR(57, false)
	backend := newFakeBackend()
	store := newMemoryStore()
	legacyIdentity := "github.com/legacy/widget"
	intermediateIdentity := "github.com/middle/widget"
	unlistedIdentity := "github.com/unlisted/widget"
	recordID := OpaqueID(unlistedIdentity, pr.Number)
	store.records[recordID] = Provenance{
		PullRequestID: recordID,
		Provider:      "github",
		Repository:    unlistedIdentity,
		RepositoryAliases: []string{
			legacyIdentity,
			intermediateIdentity,
		},
		Number: pr.Number,
		Project: Project{
			Identity: intermediateIdentity,
			Path:     testProject().Path,
		},
		Workspace: Workspace{
			ID: "unlisted-ws", Repository: intermediateIdentity,
			Branch: "pr-57-feature-widgets", Path: "/worktrees/57", State: "ready",
		},
		SourceRepo:   intermediateIdentity,
		SourceBranch: pr.Source.Name,
	}
	service := NewService(
		&fakeProvider{prs: []PullRequest{pr}},
		backend,
		store,
		legacyIdentity,
		testProject().Identity,
	)

	result, err := service.Import(context.Background(), testProject(), "57")

	require.NoError(t, err)
	assert.Equal(t, ImportCreated, result.Status)
	assert.Equal(t, 1, backend.createCalls)
	assert.Contains(t, store.records, recordID)
}

func TestImportDoesNotTreatProjectAliasAsForkSource(t *testing.T) {
	pr := testPR(50, true)
	backend := newFakeBackend()
	workspace := Workspace{
		ID: "ws-50", Repository: testProject().Identity,
		Branch: "pr-50-feature-widgets", Path: "/worktrees/50", State: "ready",
	}
	backend.workspaces = []Workspace{workspace}
	store := newMemoryStore()
	legacyIdentity := "github.com/legacy/widget"
	legacyID := OpaqueID(legacyIdentity, pr.Number)
	store.records[legacyID] = Provenance{
		PullRequestID: legacyID,
		Provider:      "github",
		Repository:    legacyIdentity,
		Number:        pr.Number,
		Project: Project{
			Identity: legacyIdentity,
			Path:     testProject().Path,
		},
		Workspace:    workspace,
		SourceRepo:   legacyIdentity,
		SourceBranch: pr.Source.Name,
	}
	service := NewService(
		&fakeProvider{prs: []PullRequest{pr}},
		backend,
		store,
		legacyIdentity,
		testProject().Identity,
	)

	_, err := service.Import(context.Background(), testProject(), "50")

	assertErrorCode(t, err, CodeConflict)
	assert.Zero(t, backend.createCalls)
	assert.Contains(t, store.records, legacyID)
}

func TestConcurrentImportConverges(t *testing.T) {
	pr := testPR(42, false)
	backend := newFakeBackend()
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	results := make(chan ImportResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Import(context.Background(), testProject(), "42")
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var statuses []ImportStatus
	for result := range results {
		statuses = append(statuses, result.Status)
	}
	assert.ElementsMatch(t, []ImportStatus{ImportCreated, ImportExisting}, statuses)
	assert.Equal(t, 1, backend.createCalls)
}

func TestImportReportsNamingConflict(t *testing.T) {
	pr := testPR(51, false)
	backend := newFakeBackend()
	backend.createErr = NewError(CodeNamingConflict, "branch is already in use", false, nil)
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "51")

	assertErrorCode(t, err, CodeNamingConflict)
}

func TestImportReportsUnavailableHead(t *testing.T) {
	pr := testPR(52, true)
	backend := newFakeBackend()
	backend.createErr = NewError(CodeInaccessibleHead, "head is unavailable", false, nil)
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "52")

	assertErrorCode(t, err, CodeInaccessibleHead)
	assert.Empty(t, backend.workspaces)
}

func TestImportRejectsRepositoryMismatch(t *testing.T) {
	pr := testPR(53, false)
	pr.Repository.Identity = "github.com/other/widget"
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, newFakeBackend(), newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "53")

	assertErrorCode(t, err, CodeRepositoryMismatch)
}

func TestRepositoryFromProjectRejectsNestedGitHubPath(t *testing.T) {
	_, err := RepositoryFromProject(Project{
		Identity: "github.com/acme/team/widget",
	})

	assertErrorCode(t, err, CodeUnsupportedProvider)
}

func TestImportPropagatesTypedProviderFailures(t *testing.T) {
	for _, code := range []ErrorCode{CodeAuthentication, CodeNetwork, CodeMalformedResponse, CodeNotFound} {
		t.Run(string(code), func(t *testing.T) {
			providerErr := NewError(code, "provider failed", code == CodeNetwork, errors.New("cause"))
			service := newTestService(&fakeProvider{getErr: providerErr}, newFakeBackend(), newMemoryStore())

			_, err := service.Import(context.Background(), testProject(), "99")

			assertErrorCode(t, err, code)
		})
	}
}

func TestImportWrapsCreationFailure(t *testing.T) {
	pr := testPR(55, false)
	backend := newFakeBackend()
	backend.createErr = errors.New("create failed")
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "55")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.Empty(t, backend.workspaces)
}

func TestImportReportsArtifactsPreservedBySharedLifecycle(t *testing.T) {
	pr := testPR(63, false)
	backend := newFakeBackend()
	backend.createErr = managedworktree.ErrWorktreeCleanupIncomplete
	backend.createErrWorkspace = Workspace{
		Path:                  "/worktrees/widget/pr-63-feature-widgets",
		Branch:                "pr-63-feature-widgets",
		preserveOnImportError: true,
	}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "63")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.ErrorContains(t, err, backend.createErrWorkspace.Path)
	assert.ErrorContains(t, err, backend.createErrWorkspace.Branch)
	assert.ErrorContains(t, err, "manual cleanup")
}

func TestImportRollsBackPostCreationFailure(t *testing.T) {
	pr := testPR(64, true)
	backend := newFakeBackend()
	backend.createErr = errors.New("configure fork push safety")
	backend.createErrWorkspace = Workspace{
		Path:   "/worktrees/widget/pr-64-feature-widgets",
		Branch: "pr-64-feature-widgets",
	}
	service := newTestService(
		&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore(),
	)

	_, err := service.Import(context.Background(), testProject(), "64")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.Empty(t, backend.workspaces)
	assert.NotContains(t, err.Error(), "manual cleanup")
}

func TestImportPreservesTypedPostCreationFailureAfterRollback(t *testing.T) {
	pr := testPR(65, true)
	backend := newFakeBackend()
	backend.createErr = NewError(
		CodeNetwork, "fork fetch failed", true, errors.New("connection reset"),
	)
	backend.createErrWorkspace = Workspace{
		Path:   "/worktrees/widget/pr-65-feature-widgets",
		Branch: "pr-65-feature-widgets",
	}
	service := newTestService(
		&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore(),
	)

	_, err := service.Import(t.Context(), testProject(), "65")

	var typed *Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, CodeNetwork, typed.Code)
	assert.True(t, typed.Retryable)
	assert.Contains(t, typed.Message, "fork fetch failed")
	assert.Contains(t, typed.Message, "rolled it back")
	assert.Empty(t, backend.workspaces)
}

func TestImportPreservesTypedNamingFailureFromWorkspaceCreation(t *testing.T) {
	pr := testPR(56, false)
	backend := newFakeBackend()
	backend.createErr = NewError(CodeNamingConflict, "workspace path already exists", false, nil)
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, newMemoryStore())

	_, err := service.Import(context.Background(), testProject(), "56")

	assertErrorCode(t, err, CodeNamingConflict)
}

func TestImportRollsBackWhenProvenanceCannotBeCommitted(t *testing.T) {
	pr := testPR(57, false)
	backend := newFakeBackend()
	store := &commitFailStore{memoryStore: newMemoryStore()}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	_, err := service.Import(context.Background(), testProject(), "57")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.Empty(t, backend.workspaces)
}

func TestImportReportsRollbackFailureAndPreservesWorkspace(t *testing.T) {
	pr := testPR(60, false)
	backend := newFakeBackend()
	backend.rollbackErr = errors.New("remove failed")
	store := &commitFailStore{memoryStore: newMemoryStore()}
	service := newTestService(&fakeProvider{prs: []PullRequest{pr}}, backend, store)

	_, err := service.Import(context.Background(), testProject(), "60")

	assertErrorCode(t, err, CodeWorkspaceCreation)
	assert.ErrorContains(t, err, "rollback failed")
	require.Len(t, backend.workspaces, 1)
	assert.Contains(t, err.Error(), backend.workspaces[0].Path)
	assert.Contains(t, err.Error(), "branch \""+backend.workspaces[0].Branch+"\"")
}

func assertErrorCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	var typed *Error
	require.ErrorAs(t, err, &typed)
	assert.Equal(t, want, typed.Code)
}

func itoa(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = digits[value%10]
		value /= 10
	}
	return string(buf[i:])
}
