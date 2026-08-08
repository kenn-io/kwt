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

func TestPathKeyResolvesNearestExistingAncestor(t *testing.T) {
	assert.Empty(t, PathKey(""))
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	realPath := filepath.Join(realParent, "missing", "topic")
	aliasPath := filepath.Join(aliasParent, "missing", "topic")

	assert.Equal(t, PathKey(realPath), PathKey(aliasPath))
}

func TestNewUsesKWT_HOME(t *testing.T) {
	kwtHome := filepath.Join(t.TempDir(), "kwt-home")
	platformConfigHome := filepath.Join(t.TempDir(), "platform-config")
	t.Setenv("KWT_HOME", kwtHome)
	t.Setenv("HOME", platformConfigHome)
	t.Setenv("APPDATA", platformConfigHome)
	t.Setenv("XDG_CONFIG_HOME", platformConfigHome)

	reg, err := New()

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(kwtHome, "registry.json"), reg.path)
	assert.DirExists(t, kwtHome)
}

func TestEntryMatchesNilOnlyWhenPathIsUnregistered(t *testing.T) {
	t.Setenv("KWT_HOME", t.TempDir())
	reg, err := New()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worktree")

	matched, err := reg.EntryMatches(path, nil)
	require.NoError(t, err)
	assert.True(t, matched)

	require.NoError(t, reg.Register(&WorktreeEntry{Path: path}))
	matched, err = reg.EntryMatches(path, nil)
	require.NoError(t, err)
	assert.False(t, matched)
}

func TestNewWithFreshKwtHomeDoesNotImportPlatformRegistry(t *testing.T) {
	root := t.TempDir()
	platformConfigHome := filepath.Join(root, "platform-config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("APPDATA", platformConfigHome)
	t.Setenv("XDG_CONFIG_HOME", platformConfigHome)

	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	legacyPath := filepath.Join(configDir, "kwt", "registry.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0o755))
	legacyEntry := &WorktreeEntry{
		Repository: "github.com/example/widget",
		Branch:     "feature",
		Path:       filepath.Join(root, "worktrees", "feature"),
		Generation: "legacy-generation",
	}
	require.NoError(t, saveEntries(legacyPath, map[string]*WorktreeEntry{
		legacyEntry.Path: legacyEntry,
	}))
	kwtHome := filepath.Join(root, "kwt-home")
	t.Setenv("KWT_HOME", kwtHome)
	reg, err := New()

	require.NoError(t, err)
	assert.Empty(t, reg.List())
	assert.NoFileExists(t, filepath.Join(kwtHome, "registry.json"))
	assert.FileExists(t, legacyPath)
}

func TestNewWithFreshKwtHomeDoesNotCreateLegacyRegistryArtifacts(t *testing.T) {
	root := t.TempDir()
	platformConfigHome := filepath.Join(root, "platform-config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("APPDATA", platformConfigHome)
	t.Setenv("XDG_CONFIG_HOME", platformConfigHome)
	t.Setenv("KWT_HOME", filepath.Join(root, "kwt-home"))

	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	legacyDir := filepath.Join(configDir, "kwt")
	_, err = New()

	require.NoError(t, err)
	assert.NoDirExists(t, legacyDir)
}

func TestNewDoesNotOverwriteExistingKwtHomeRegistry(t *testing.T) {
	root := t.TempDir()
	platformConfigHome := filepath.Join(root, "platform-config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("APPDATA", platformConfigHome)
	t.Setenv("XDG_CONFIG_HOME", platformConfigHome)

	configDir, err := os.UserConfigDir()
	require.NoError(t, err)
	legacyPath := filepath.Join(configDir, "kwt", "registry.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0o755))
	legacyEntry := &WorktreeEntry{Path: filepath.Join(root, "legacy")}
	require.NoError(t, saveEntries(legacyPath, map[string]*WorktreeEntry{
		legacyEntry.Path: legacyEntry,
	}))

	kwtHome := filepath.Join(root, "kwt-home")
	require.NoError(t, os.MkdirAll(kwtHome, 0o755))
	targetPath := filepath.Join(kwtHome, "registry.json")
	targetEntry := &WorktreeEntry{Path: filepath.Join(root, "target")}
	require.NoError(t, saveEntries(targetPath, map[string]*WorktreeEntry{
		targetEntry.Path: targetEntry,
	}))
	t.Setenv("KWT_HOME", kwtHome)

	reg, err := New()

	require.NoError(t, err)
	require.Len(t, reg.List(), 1)
	assert.Equal(t, targetEntry, reg.List()[0])
	legacyEntries, err := loadEntries(legacyPath)
	require.NoError(t, err)
	assert.Contains(t, legacyEntries, legacyEntry.Path)
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

func TestRegistryCompareAndSwapAliases(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	worktreePath := t.TempDir()
	aliasPath := filepath.Dir(worktreePath) + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + filepath.Base(worktreePath)
	thirdAliasPath := filepath.Dir(worktreePath) + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(worktreePath)
	registeredAt := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	first := &WorktreeEntry{
		Repository: "github.com/acme/widget", Branch: "feature/topic",
		Path: worktreePath, Hash: "abc123", RegisteredAt: registeredAt,
		Generation: generation,
	}
	second := *first
	second.Path = aliasPath

	tests := []struct {
		name        string
		current     []*WorktreeEntry
		expected    []*WorktreeEntry
		wantChanged bool
	}{
		{
			name:        "collapse exact group",
			current:     []*WorktreeEntry{first, &second},
			expected:    []*WorktreeEntry{first, &second},
			wantChanged: true,
		},
		{
			name: "preserve metadata change",
			current: func() []*WorktreeEntry {
				changed := second
				changed.Branch = "feature/changed"
				return []*WorktreeEntry{first, &changed}
			}(),
			expected: []*WorktreeEntry{first, &second},
		},
		{
			name: "preserve added alias",
			current: func() []*WorktreeEntry {
				third := second
				third.Path = thirdAliasPath
				return []*WorktreeEntry{first, &second, &third}
			}(),
			expected: []*WorktreeEntry{first, &second},
		},
		{
			name: "preserve active creation",
			current: func() []*WorktreeEntry {
				active := second
				active.CreationToken = "active-owner"
				return []*WorktreeEntry{first, &active}
			}(),
			expected: func() []*WorktreeEntry {
				active := second
				active.CreationToken = "active-owner"
				return []*WorktreeEntry{first, &active}
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registryPath := filepath.Join(t.TempDir(), "registry.json")
			stored := make(map[string]*WorktreeEntry, len(tt.current))
			for _, entry := range tt.current {
				copy := *entry
				stored[entry.Path] = &copy
			}
			require.NoError(t, saveEntries(registryPath, stored))
			r := &Registry{entries: make(map[string]*WorktreeEntry), path: registryPath}
			require.NoError(t, r.load())

			changed, err := r.CompareAndSwapAliases(tt.expected, first)

			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
			entries := r.List()
			if tt.wantChanged {
				require.Len(t, entries, 1)
				assert.Equal(t, first.Path, entries[0].Path)
				assert.Equal(t, first.Generation, entries[0].Generation)
			} else {
				assert.Len(t, entries, len(tt.current))
			}
		})
	}
}

func TestRegistryCompareAndSwapAliasesRemovesExactGroup(t *testing.T) {
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "worktrees-link")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported on this filesystem: %v", err)
	}
	worktreePath := filepath.Join(realParent, "missing-worktree")
	aliasPath := filepath.Join(aliasParent, "missing-worktree")
	thirdAliasPath := realParent + string(os.PathSeparator) + "." +
		string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(worktreePath)
	registeredAt := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	first := &WorktreeEntry{
		Repository: "github.com/acme/widget", Branch: "feature/topic",
		Path: worktreePath, RegisteredAt: registeredAt,
	}
	second := *first
	second.Path = aliasPath
	tests := []struct {
		name        string
		current     []*WorktreeEntry
		wantChanged bool
	}{
		{name: "remove exact group", current: []*WorktreeEntry{first, &second}, wantChanged: true},
		{
			name: "preserve metadata change",
			current: func() []*WorktreeEntry {
				changed := second
				changed.Branch = "feature/changed"
				return []*WorktreeEntry{first, &changed}
			}(),
		},
		{
			name: "preserve added alias",
			current: func() []*WorktreeEntry {
				third := second
				third.Path = thirdAliasPath
				return []*WorktreeEntry{first, &second, &third}
			}(),
		},
		{
			name: "preserve active creation",
			current: func() []*WorktreeEntry {
				active := second
				active.CreationToken = "active-owner"
				return []*WorktreeEntry{first, &active}
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registryPath := filepath.Join(t.TempDir(), "registry.json")
			stored := make(map[string]*WorktreeEntry, len(tt.current))
			for _, entry := range tt.current {
				copy := *entry
				stored[entry.Path] = &copy
			}
			require.NoError(t, saveEntries(registryPath, stored))
			r := &Registry{entries: make(map[string]*WorktreeEntry), path: registryPath}
			require.NoError(t, r.load())

			changed, err := r.CompareAndSwapAliases(
				[]*WorktreeEntry{first, &second},
				nil,
			)

			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
			if tt.wantChanged {
				assert.Empty(t, r.List())
			} else {
				assert.Len(t, r.List(), len(tt.current))
			}
		})
	}
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

func TestRegistryReportsWhetherCreationOwnerIsActive(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	worktreePath := filepath.Join(t.TempDir(), "creating-worktree")
	release, acquired, err := r.AcquireCreation(worktreePath)
	require.NoError(t, err)
	require.True(t, acquired)

	active, err := r.CreationActive(worktreePath)
	require.NoError(t, err)
	assert.True(t, active)
	require.NoError(t, release())

	active, err = r.CreationActive(worktreePath)
	require.NoError(t, err)
	assert.False(t, active)
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

func TestRegistryExpirationUpdatePreservesAcknowledgementAndCanonicalizesIdentity(t *testing.T) {
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
		"https://user:credential-must-not-appear@github.com/example/repository.git",
		"feature/reviewed",
		&expiresAt,
	)

	require.NoError(t, err)
	require.True(t, updated)
	entry, ok := r.Get(path)
	require.True(t, ok)
	assert.False(t, entry.UnreviewedRemoteSource)
	assert.Equal(t, generation, entry.Generation)
	assert.Equal(t, "github.com/example/repository", entry.Repository)
	require.NotNil(t, entry.ExpiresAt)
	assert.True(t, entry.ExpiresAt.Equal(expiresAt))
	stored, err := os.ReadFile(r.path)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "credential-must-not-appear")
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

func TestRegistryRemoveIfMatchAfterDoesNotRunRemovalForChangedExpiration(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "worktree")
	expired := time.Now().Add(-time.Hour)
	observed := &WorktreeEntry{
		Path: path, Repository: "github.com/acme/widget",
		Generation: "0123456789abcdef0123456789abcdef", ExpiresAt: &expired,
	}
	require.NoError(t, r.Register(observed))
	extended := *observed
	future := time.Now().Add(time.Hour)
	extended.ExpiresAt = &future
	require.NoError(t, r.Register(&extended))
	removalCalled := false

	removed, err := r.RemoveIfMatchAfter(path, observed, func() error {
		removalCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.False(t, removed)
	assert.False(t, removalCalled)
	current, ok := r.Get(path)
	require.True(t, ok)
	assert.True(t, future.Equal(*current.ExpiresAt))
}

func TestRegistryRemoveIfMatchAfterClaimsAbsentEntryDuringRemoval(t *testing.T) {
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    filepath.Join(t.TempDir(), "registry.json"),
	}
	path := filepath.Join(t.TempDir(), "unregistered-worktree")
	removalCalled := false

	removed, err := r.RemoveIfMatchAfter(path, nil, func() error {
		removalCalled = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, removed)
	assert.True(t, removalCalled)
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
