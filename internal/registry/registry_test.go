package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorktreeEntry_IsExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt *time.Time
		want      bool
	}{
		{
			name:      "nil expiration",
			expiresAt: nil,
			want:      false,
		},
		{
			name:      "expired",
			expiresAt: new(time.Now().Add(-time.Hour)),
			want:      true,
		},
		{
			name:      "not expired",
			expiresAt: new(time.Now().Add(time.Hour)),
			want:      false,
		},
		{
			name:      "just expired",
			expiresAt: new(time.Now().Add(-time.Second)),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &WorktreeEntry{
				ExpiresAt: tt.expiresAt,
			}
			if got := e.IsExpired(); got != tt.want {
				t.Errorf("WorktreeEntry.IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegistry_ListExpired(t *testing.T) {
	// Create a temporary registry
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	registryPath := filepath.Join(tmpDir, "registry.json")

	pastTime := time.Now().Add(-time.Hour)
	futureTime := time.Now().Add(time.Hour)

	entries := []*WorktreeEntry{
		{
			Path:         "/path/to/expired1",
			Branch:       "expired-branch-1",
			RegisteredAt: time.Now(),
			ExpiresAt:    &pastTime,
		},
		{
			Path:         "/path/to/not-expired",
			Branch:       "not-expired-branch",
			RegisteredAt: time.Now(),
			ExpiresAt:    &futureTime,
		},
		{
			Path:         "/path/to/expired2",
			Branch:       "expired-branch-2",
			RegisteredAt: time.Now(),
			ExpiresAt:    &pastTime,
		},
		{
			Path:         "/path/to/no-expiration",
			Branch:       "no-expiration-branch",
			RegisteredAt: time.Now(),
			ExpiresAt:    nil,
		},
	}

	// Write entries to registry file
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal entries: %v", err)
	}
	if err := os.WriteFile(registryPath, data, 0644); err != nil {
		t.Fatalf("Failed to write registry: %v", err)
	}

	// Create registry and load
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}
	if err := r.load(); err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}

	// Test ListExpired
	expired := r.ListExpired()
	if len(expired) != 2 {
		t.Errorf("ListExpired() returned %d entries, want 2", len(expired))
	}

	// Check that only expired entries are returned
	expiredPaths := make(map[string]bool)
	for _, e := range expired {
		expiredPaths[e.Path] = true
	}

	if !expiredPaths["/path/to/expired1"] {
		t.Error("ListExpired() missing /path/to/expired1")
	}
	if !expiredPaths["/path/to/expired2"] {
		t.Error("ListExpired() missing /path/to/expired2")
	}
	if expiredPaths["/path/to/not-expired"] {
		t.Error("ListExpired() should not include /path/to/not-expired")
	}
	if expiredPaths["/path/to/no-expiration"] {
		t.Error("ListExpired() should not include /path/to/no-expiration")
	}
}

func TestRegistryAcknowledgesUnreviewedRemoteSourceWithoutLosingExpiration(
	t *testing.T,
) {
	future := time.Now().Add(time.Hour)
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	if err := r.Register(&WorktreeEntry{
		Path:                   "/worktrees/remote",
		Branch:                 "feature/remote",
		ExpiresAt:              &future,
		UnreviewedRemoteSource: true,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if !r.IsUnreviewedRemoteSource("/worktrees/remote") {
		t.Fatal("remote source was not marked unreviewed")
	}
	if err := r.AcknowledgeRemoteSource("/worktrees/remote"); err != nil {
		t.Fatalf("AcknowledgeRemoteSource() error = %v", err)
	}
	if r.IsUnreviewedRemoteSource("/worktrees/remote") {
		t.Fatal("remote source remained unreviewed after acknowledgement")
	}
	entry, ok := r.Get("/worktrees/remote")
	if !ok || entry.ExpiresAt == nil {
		t.Fatal("acknowledgement discarded expiration metadata")
	}
	if entry.ExpiresAt.Sub(future) > time.Second ||
		future.Sub(*entry.ExpiresAt) > time.Second {
		t.Fatalf("expiration = %v, want %v", entry.ExpiresAt, future)
	}
}

func TestRegistryMutationsTreatSymlinkAliasesAsOneWorktree(t *testing.T) {
	root := t.TempDir()
	worktreePath := filepath.Join(root, "worktree")
	if err := os.MkdirAll(worktreePath, 0755); err != nil {
		t.Fatalf("create worktree path: %v", err)
	}
	aliasPath := filepath.Join(root, "worktree-alias")
	if err := os.Symlink(worktreePath, aliasPath); err != nil {
		t.Skipf("symbolic links are not supported or allowed: %v", err)
	}
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(root, "registry.json"),
	}
	if err := r.Register(&WorktreeEntry{
		Path:                   worktreePath,
		Branch:                 "first",
		UnreviewedRemoteSource: true,
	}); err != nil {
		t.Fatalf("Register(real path) error = %v", err)
	}
	if err := r.Register(&WorktreeEntry{
		Path:                   aliasPath,
		Branch:                 "latest",
		UnreviewedRemoteSource: true,
	}); err != nil {
		t.Fatalf("Register(alias path) error = %v", err)
	}

	if got := r.List(); len(got) != 1 || got[0].Branch != "latest" {
		t.Fatalf("List() = %+v, want one latest alias registration", got)
	}
	if err := r.AcknowledgeRemoteSource(worktreePath); err != nil {
		t.Fatalf("AcknowledgeRemoteSource(real path) error = %v", err)
	}
	if r.IsUnreviewedRemoteSource(aliasPath) {
		t.Fatal("alias remained unreviewed after canonical path acknowledgement")
	}
	if err := r.Unregister(worktreePath); err != nil {
		t.Fatalf("Unregister(real path) error = %v", err)
	}
	if got := r.List(); len(got) != 0 {
		t.Fatalf("List() after unregister = %+v, want empty registry", got)
	}
}

func TestRegistryRewriteKeepsFilePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permission bits")
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	r := &Registry{entries: make(map[string]*WorktreeEntry), path: path}

	if err := r.Register(&WorktreeEntry{
		Path:       "/worktrees/private",
		Repository: "https://user:secret@example.com/repo.git",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat registry: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("new registry mode = %o, want 600", got)
	}

	if err := os.Chmod(path, 0644); err != nil {
		t.Fatalf("make legacy registry readable: %v", err)
	}
	if err := r.Register(&WorktreeEntry{Path: "/worktrees/second"}); err != nil {
		t.Fatalf("rewrite legacy registry: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat rewritten registry: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("rewritten legacy registry mode = %o, want 600", got)
	}

	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("restrict registry: %v", err)
	}
	if err := r.Register(&WorktreeEntry{Path: "/worktrees/third"}); err != nil {
		t.Fatalf("rewrite restricted registry: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat restricted registry: %v", err)
	}
	if got := info.Mode().Perm(); got != 0400 {
		t.Fatalf("restricted registry mode = %o, want 400", got)
	}
}

func TestRegistryConcurrentRegistrationsPreserveBothMarkers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	left := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	right := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	var done sync.WaitGroup
	done.Add(2)

	register := func(r *Registry, worktreePath string) {
		defer done.Done()
		ready.Done()
		<-start
		errs <- r.Register(&WorktreeEntry{
			Path:                   worktreePath,
			UnreviewedRemoteSource: true,
		})
	}
	go register(left, "/worktrees/left")
	go register(right, "/worktrees/right")
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}

	reloaded := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	if err := reloaded.load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if !reloaded.IsUnreviewedRemoteSource("/worktrees/left") {
		t.Error("left registration was lost")
	}
	if !reloaded.IsUnreviewedRemoteSource("/worktrees/right") {
		t.Error("right registration was lost")
	}
}

func TestRegistryConcurrentAcknowledgementPreservesOtherMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	seed := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	if err := seed.Register(&WorktreeEntry{
		Path:                   "/worktrees/reviewed",
		UnreviewedRemoteSource: true,
	}); err != nil {
		t.Fatalf("seed Register() error = %v", err)
	}
	acknowledger := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	registrar := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	if err := acknowledger.load(); err != nil {
		t.Fatalf("load acknowledger: %v", err)
	}
	if err := registrar.load(); err != nil {
		t.Fatalf("load registrar: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		ready.Done()
		<-start
		errs <- acknowledger.AcknowledgeRemoteSource("/worktrees/reviewed")
	}()
	go func() {
		defer done.Done()
		ready.Done()
		<-start
		errs <- registrar.Register(&WorktreeEntry{
			Path:                   "/worktrees/new",
			UnreviewedRemoteSource: true,
		})
	}()
	ready.Wait()
	close(start)
	done.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("registry mutation error = %v", err)
		}
	}

	reloaded := &Registry{entries: make(map[string]*WorktreeEntry), path: path}
	if err := reloaded.load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if reloaded.IsUnreviewedRemoteSource("/worktrees/reviewed") {
		t.Error("acknowledgement was overwritten")
	}
	if !reloaded.IsUnreviewedRemoteSource("/worktrees/new") {
		t.Error("concurrent registration was lost")
	}
}

func TestRegistryConditionalUnregisterPreservesReplacementGeneration(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "reused-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:       path,
		Generation: "0123456789abcdef0123456789abcdef",
	}))
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:       path,
		Generation: "fedcba9876543210fedcba9876543210",
	}))

	removed, err := r.UnregisterIfGeneration(
		path,
		"0123456789abcdef0123456789abcdef",
	)

	require.NoError(t, err)
	assert.False(t, removed)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Equal(t, "fedcba9876543210fedcba9876543210", entry.Generation)
}

func TestRegistryCreationFinalizationPreservesReplacementOwner(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "reused-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:          path,
		Branch:        "feature/original",
		CreationToken: "original-creation",
	}))
	observed, ok := r.Get(path)
	require.True(t, ok)
	replacementExpiry := time.Now().Add(time.Hour)
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:       path,
		Branch:     "feature/replacement",
		Generation: "fedcba9876543210fedcba9876543210",
		ExpiresAt:  &replacementExpiry,
	}))

	finalized, err := r.CompareAndSwap(
		path,
		observed,
		&WorktreeEntry{
			Path:       path,
			Branch:     "feature/original",
			Generation: "0123456789abcdef0123456789abcdef",
		},
	)

	require.NoError(t, err)
	assert.False(t, finalized)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Equal(t, "feature/replacement", entry.Branch)
	assert.Equal(t, "fedcba9876543210fedcba9876543210", entry.Generation)
	require.NotNil(t, entry.ExpiresAt)
	assert.True(t, entry.ExpiresAt.Equal(replacementExpiry))
}

func TestRegistryCreationLockDistinguishesActiveAndAbandonedClaims(
	t *testing.T,
) {
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	first := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}
	second := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}
	worktreePath := filepath.Join(t.TempDir(), "creating-worktree")

	releaseFirst, acquired, err := first.AcquireCreation(worktreePath)
	require.NoError(t, err)
	require.True(t, acquired)

	_, acquired, err = second.AcquireCreation(worktreePath)
	require.NoError(t, err)
	assert.False(t, acquired)

	require.NoError(t, releaseFirst())
	releaseSecond, acquired, err := second.AcquireCreation(worktreePath)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, releaseSecond())
}

func TestRegistryCreationLockKeepsIdentityAfterDestinationAppears(
	t *testing.T,
) {
	registryPath := filepath.Join(t.TempDir(), "registry.json")
	first := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}
	second := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}
	canonicalParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(canonicalParent, aliasParent))
	aliasPath := filepath.Join(aliasParent, "creating-worktree")
	canonicalPath := filepath.Join(canonicalParent, "creating-worktree")

	release, acquired, err := first.AcquireCreation(aliasPath)
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, os.Mkdir(canonicalPath, 0755))

	_, acquired, err = second.AcquireCreation(canonicalPath)

	require.NoError(t, err)
	assert.False(t, acquired)
	require.NoError(t, release())
}

func TestRegistryReclaimsAliasedAbandonedCreation(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	canonicalParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(canonicalParent, aliasParent))
	aliasPath := filepath.Join(aliasParent, "creating-worktree")
	canonicalPath := filepath.Join(canonicalParent, "creating-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:                   aliasPath,
		Branch:                 "feature/abandoned",
		CreationToken:          "abandoned-creation",
		UnreviewedRemoteSource: true,
	}))
	replacement := &WorktreeEntry{
		Path:                   canonicalPath,
		Branch:                 "feature/recovered",
		CreationToken:          "replacement-creation",
		UnreviewedRemoteSource: true,
	}

	reclaimed, err := r.ReclaimCreation(
		canonicalPath,
		"abandoned-creation",
		replacement,
	)

	require.NoError(t, err)
	require.True(t, reclaimed)
	assert.Len(t, r.List(), 1)
	require.NoError(t, os.Mkdir(canonicalPath, 0755))
	completed, err := r.CompleteCreation(
		canonicalPath,
		"replacement-creation",
		"0123456789abcdef0123456789abcdef",
	)
	require.NoError(t, err)
	require.True(t, completed)
	assert.Len(t, r.List(), 1)
	entry, ok := r.Get(aliasPath)
	require.True(t, ok)
	assert.Equal(t, canonicalPath, entry.Path)
	assert.Empty(t, entry.CreationToken)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", entry.Generation)
}

func TestRegistryCreationFinalizationPreservesAcknowledgement(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "created-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:                   path,
		Branch:                 "feature/reviewed",
		CreationToken:          "creation-owner",
		UnreviewedRemoteSource: true,
	}))
	require.NoError(t, r.AcknowledgeRemoteSource(path))

	finalized, err := r.CompleteCreation(
		path,
		"creation-owner",
		"0123456789abcdef0123456789abcdef",
	)

	require.NoError(t, err)
	require.True(t, finalized)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Empty(t, entry.CreationToken)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", entry.Generation)
	assert.False(t, entry.UnreviewedRemoteSource)
}

func TestRegistryCreationAbortPreservesReviewedState(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "reviewed-worktree")
	previous := &WorktreeEntry{
		Path:                   path,
		Branch:                 "feature/reviewed",
		UnreviewedRemoteSource: false,
	}
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:                   path,
		Branch:                 "feature/replacement",
		CreationToken:          "creation-owner",
		UnreviewedRemoteSource: true,
	}))

	aborted, err := r.AbortCreation(path, "creation-owner", previous)

	require.NoError(t, err)
	require.True(t, aborted)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Equal(t, "feature/reviewed", entry.Branch)
	assert.False(t, entry.UnreviewedRemoteSource)
}

func TestRegistryExpirationUpdatePreservesAcknowledgement(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "expiring-worktree")
	generation := "0123456789abcdef0123456789abcdef"
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:                   path,
		Branch:                 "feature/reviewed",
		Generation:             generation,
		UnreviewedRemoteSource: true,
	}))
	require.NoError(t, r.AcknowledgeRemoteSource(path))
	expiresAt := time.Now().Add(time.Hour)

	updated, err := r.SetExpirationIfGeneration(
		path,
		generation,
		"https://github.com/example/repository.git",
		"feature/reviewed",
		&expiresAt,
	)

	require.NoError(t, err)
	require.True(t, updated)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.False(t, entry.UnreviewedRemoteSource)
	assert.Equal(t, generation, entry.Generation)
	assert.Equal(t, "https://github.com/example/repository.git", entry.Repository)
	require.NotNil(t, entry.ExpiresAt)
	assert.True(t, entry.ExpiresAt.Equal(expiresAt))
}

func TestRegistryCreationClaimRejectsChangedObservedEntry(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "reused-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:       path,
		Branch:     "feature/observed",
		Generation: "0123456789abcdef0123456789abcdef",
	}))
	observed, ok := r.Get(path)
	require.True(t, ok)
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:       path,
		Branch:     "feature/replacement",
		Generation: "fedcba9876543210fedcba9876543210",
	}))

	claimed, err := r.CompareAndSwap(
		path,
		observed,
		&WorktreeEntry{
			Path:          path,
			Branch:        "feature/claimant",
			CreationToken: "claimant-creation",
		},
	)

	require.NoError(t, err)
	assert.False(t, claimed)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Equal(t, "feature/replacement", entry.Branch)
	assert.Equal(t, "fedcba9876543210fedcba9876543210", entry.Generation)
}

func TestRegistryLegacyCleanupPreservesProvisionalCreation(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "provisional-worktree")
	require.NoError(t, r.Register(&WorktreeEntry{
		Path:          path,
		Branch:        "feature/creating",
		CreationToken: "active-creation",
	}))

	removed, err := r.UnregisterIfGeneration(path, "")

	require.NoError(t, err)
	assert.False(t, removed)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.Equal(t, "active-creation", entry.CreationToken)
}

func TestWorktreeEntry_ExpiresAt_JSONMarshal(t *testing.T) {
	// Test that ExpiresAt is omitted when nil (backwards compatibility)
	entry := &WorktreeEntry{
		Repository:   "https://github.com/test/repo",
		Branch:       "main",
		Path:         "/path/to/worktree",
		RegisteredAt: time.Now(),
		ExpiresAt:    nil,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Failed to marshal entry: %v", err)
	}

	// Check that expires_at is not present in JSON
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if _, ok := m["expires_at"]; ok {
		t.Error("expires_at should be omitted when nil")
	}

	// Test that ExpiresAt is included when set
	expiresAt := time.Now().Add(time.Hour)
	entry.ExpiresAt = &expiresAt

	data, err = json.Marshal(entry)
	if err != nil {
		t.Fatalf("Failed to marshal entry with expiration: %v", err)
	}

	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if _, ok := m["expires_at"]; !ok {
		t.Error("expires_at should be present when set")
	}
}
