// Package registry provides global worktree tracking across repositories.
package registry

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
)

// WorktreeEntry represents a registered worktree.
type WorktreeEntry struct {
	Repository             string     `json:"repository"`
	Branch                 string     `json:"branch"`
	Path                   string     `json:"path"`
	Hash                   string     `json:"hash"`
	IsMain                 bool       `json:"is_main"`
	RegisteredAt           time.Time  `json:"registered_at"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
	Generation             string     `json:"generation,omitempty"`
	CreationToken          string     `json:"creation_token,omitempty"`
	UnreviewedRemoteSource bool       `json:"unreviewed_remote_source,omitempty"`
}

// IsExpired returns true if the worktree has an expiration date that has passed.
func (e *WorktreeEntry) IsExpired() bool {
	if e.ExpiresAt == nil {
		return false
	}
	return time.Now().After(*e.ExpiresAt)
}

// Registry manages global worktree tracking.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*WorktreeEntry // key is path
	path    string
}

// New creates a new registry instance.
func New() (*Registry, error) {
	registryDir := os.Getenv("KWT_HOME")
	if registryDir != "" {
		if expanded, err := utils.ExpandPath(registryDir); err == nil {
			registryDir = expanded
		}
	} else {
		registryDir = platformRegistryDir()
	}
	return NewAt(registryDir)
}

// NewAt opens the registry rooted at an explicit kwt home.
func NewAt(registryDir string) (*Registry, error) {
	if registryDir == "" {
		return nil, fmt.Errorf("registry home is empty")
	}
	if !filepath.IsAbs(registryDir) {
		return nil, fmt.Errorf("registry home must be absolute")
	}
	if err := os.MkdirAll(registryDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create registry directory: %w", err)
	}

	registryPath := filepath.Join(registryDir, "registry.json")
	r := &Registry{
		entries: make(map[string]*WorktreeEntry),
		path:    registryPath,
	}

	if err := r.load(); err != nil {
		return nil, err
	}

	return r, nil
}

func platformRegistryDir() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "kwt")
}

// load reads the registry from disk.
func (r *Registry) load() error {
	entries, err := loadEntries(r.path)
	if err != nil {
		return err
	}

	r.mu.Lock()
	r.entries = entries
	r.mu.Unlock()
	return nil
}

func loadEntries(path string) (map[string]*WorktreeEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]*WorktreeEntry), nil
		}
		return nil, fmt.Errorf("failed to read registry: %w", err)
	}

	if len(data) == 0 {
		return make(map[string]*WorktreeEntry), nil
	}

	var entries []*WorktreeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("failed to unmarshal registry: %w", err)
	}

	result := make(map[string]*WorktreeEntry, len(entries))
	for _, entry := range entries {
		result[entry.Path] = entry
	}
	return result, nil
}

func saveEntries(path string, entriesByPath map[string]*WorktreeEntry) error {
	entries := make([]*WorktreeEntry, 0, len(entriesByPath))
	for _, entry := range entriesByPath {
		entries = append(entries, entry)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal registry: %w", err)
	}
	data = append(data, '\n')

	mode := os.FileMode(0600)
	if info, statErr := os.Stat(path); statErr == nil {
		existing := info.Mode().Perm()
		if existing&^os.FileMode(0600) == 0 {
			mode = existing
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create registry temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("failed to set registry temp permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("failed to write registry temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("failed to sync registry temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("failed to close registry temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("failed to replace registry: %w", err)
	}
	return nil
}

func (r *Registry) mutate(
	change func(map[string]*WorktreeEntry) bool,
) error {
	return r.mutateChecked(func(entries map[string]*WorktreeEntry) (bool, error) {
		return change(entries), nil
	})
}

func (r *Registry) mutateChecked(
	change func(map[string]*WorktreeEntry) (bool, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.path), 0755); err != nil {
		return fmt.Errorf("failed to create registry directory: %w", err)
	}
	lock := flock.New(r.path+".lock", flock.SetPermissions(0600))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("failed to lock registry: %w", err)
	}
	defer func() { _ = lock.Unlock() }()

	entries, err := loadEntries(r.path)
	if err != nil {
		return err
	}
	changed, err := change(entries)
	if err != nil {
		r.entries = entries
		return err
	}
	if changed {
		if err := saveEntries(r.path, entries); err != nil {
			return err
		}
	}
	r.entries = entries
	return nil
}

// Register adds or updates a worktree entry.
func (r *Registry) Register(entry *WorktreeEntry) error {
	copied := *entry
	if copied.RegisteredAt.IsZero() {
		copied.RegisteredAt = time.Now()
	}
	return r.mutate(func(entries map[string]*WorktreeEntry) bool {
		for _, key := range matchingRegistryKeys(entries, copied.Path) {
			delete(entries, key)
		}
		entries[copied.Path] = &copied
		return true
	})
}

// CompareAndSwap replaces a path only while its complete registry entry still
// matches the caller's observed state. A nil expected entry claims an
// unregistered path; a nil replacement removes the matched entry.
func (r *Registry) CompareAndSwap(
	path string,
	expected *WorktreeEntry,
	replacement *WorktreeEntry,
) (bool, error) {
	var copied *WorktreeEntry
	if replacement != nil {
		value := *replacement
		if value.RegisteredAt.IsZero() {
			value.RegisteredAt = time.Now()
		}
		copied = &value
	}

	replaced := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		if expected == nil {
			if len(keys) != 0 {
				return false
			}
		} else {
			if len(keys) != 1 ||
				!sameWorktreeEntry(entries[keys[0]], expected) {
				return false
			}
		}
		for _, key := range keys {
			delete(entries, key)
		}
		if copied != nil {
			entries[copied.Path] = copied
		}
		replaced = true
		return true
	})
	return replaced, err
}

// CompareAndSwapAliases collapses one complete canonical-path alias group only
// while every stored entry still matches the inspected group. A non-nil
// retained entry must be one of the expected entries; nil removes the complete
// group. Active creation ownership always prevents mutation.
func (r *Registry) CompareAndSwapAliases(
	expected []*WorktreeEntry,
	retained *WorktreeEntry,
) (bool, error) {
	if len(expected) < 2 {
		return false, nil
	}
	groupPath := expected[0].Path
	groupKey := PathKey(groupPath)
	retainedExpected := retained == nil
	for _, entry := range expected {
		if entry == nil || PathKey(entry.Path) != groupKey {
			return false, nil
		}
		if retained != nil && sameWorktreeEntry(entry, retained) {
			retainedExpected = true
		}
	}
	if !retainedExpected {
		return false, nil
	}

	var copied *WorktreeEntry
	if retained != nil {
		value := *retained
		copied = &value
	}
	replaced := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, groupPath)
		if len(keys) != len(expected) {
			return false
		}
		matched := make([]bool, len(expected))
		for _, key := range keys {
			current := entries[key]
			if current == nil || current.CreationToken != "" {
				return false
			}
			found := false
			for index, wanted := range expected {
				if !matched[index] && sameWorktreeEntry(current, wanted) {
					matched[index] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		for _, key := range keys {
			delete(entries, key)
		}
		if copied != nil {
			entries[copied.Path] = copied
		}
		replaced = true
		return true
	})
	return replaced, err
}

// RemoveIfMatchAfter runs removal while the complete observed registry state
// remains unchanged under the registry lock. A nil expected value claims an
// absent entry; a matching entry is deleted only after removal succeeds.
func (r *Registry) RemoveIfMatchAfter(
	path string,
	expected *WorktreeEntry,
	removal func() error,
) (bool, error) {
	removed := false
	err := r.mutateChecked(func(entries map[string]*WorktreeEntry) (bool, error) {
		keys := matchingRegistryKeys(entries, path)
		if expected == nil {
			if len(keys) != 0 {
				return false, nil
			}
			if err := removal(); err != nil {
				return false, err
			}
			removed = true
			return false, nil
		}
		if len(keys) != 1 || !sameWorktreeEntry(entries[keys[0]], expected) {
			return false, nil
		}
		if err := removal(); err != nil {
			return false, err
		}
		delete(entries, keys[0])
		removed = true
		return true, nil
	})
	return removed, err
}

// EntryMatches reports whether path still has the complete observed registry
// entry after refreshing from disk under the registry lock. A nil expected
// entry matches only while the path remains unregistered.
func (r *Registry) EntryMatches(path string, expected *WorktreeEntry) (bool, error) {
	matched := false
	err := r.mutateChecked(func(entries map[string]*WorktreeEntry) (bool, error) {
		keys := matchingRegistryKeys(entries, path)
		if expected == nil {
			matched = len(keys) == 0
		} else {
			matched = len(keys) == 1 &&
				sameWorktreeEntry(entries[keys[0]], expected)
		}
		return false, nil
	})
	return matched, err
}

// AcquireCreation holds path's creation ownership until the returned release
// function is called. The operating system releases the lock if the creating
// process exits, allowing a later operation to recover its provisional entry.
func (r *Registry) AcquireCreation(
	path string,
) (func() error, bool, error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0755); err != nil {
		return nil, false, fmt.Errorf(
			"failed to create registry directory: %w",
			err,
		)
	}
	pathHash := sha256.Sum256([]byte(PathKey(path)))
	lockPath := filepath.Join(
		filepath.Dir(r.path),
		fmt.Sprintf(".creation-%x.lock", pathHash),
	)
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	acquired, err := lock.TryLock()
	if err != nil {
		return nil, false, fmt.Errorf("lock worktree creation: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}
	return lock.Unlock, true, nil
}

// CreationActive reports whether another process currently owns path's
// creation lock. A persisted token without a lock is abandoned state.
func (r *Registry) CreationActive(path string) (bool, error) {
	release, acquired, err := r.AcquireCreation(path)
	if err != nil {
		return false, err
	}
	if !acquired {
		return true, nil
	}
	if err := release(); err != nil {
		return false, fmt.Errorf("release worktree creation inspection: %w", err)
	}
	return false, nil
}

// CompleteCreation finalizes only the provisional owner named by token. Other
// fields are retained so acknowledgement or metadata updates are not lost.
func (r *Registry) CompleteCreation(
	path string,
	token string,
	generation string,
) (bool, error) {
	completed := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		if len(keys) != 1 || entries[keys[0]].CreationToken != token {
			return false
		}
		entry := entries[keys[0]]
		entry.CreationToken = ""
		entry.Generation = generation
		completed = true
		return true
	})
	return completed, err
}

// ReclaimCreation transfers an abandoned provisional entry to a new owner.
func (r *Registry) ReclaimCreation(
	path string,
	token string,
	replacement *WorktreeEntry,
) (bool, error) {
	copied := *replacement
	if copied.RegisteredAt.IsZero() {
		copied.RegisteredAt = time.Now()
	}
	reclaimed := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		if len(keys) != 1 || entries[keys[0]].CreationToken != token {
			return false
		}
		delete(entries, keys[0])
		entries[copied.Path] = &copied
		reclaimed = true
		return true
	})
	return reclaimed, err
}

// AbortCreation restores the state replaced by token, preserving any
// acknowledgement made while creation was active.
func (r *Registry) AbortCreation(
	path string,
	token string,
	previous *WorktreeEntry,
) (bool, error) {
	aborted := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		if len(keys) != 1 || entries[keys[0]].CreationToken != token {
			return false
		}
		current := entries[keys[0]]
		delete(entries, keys[0])
		if previous != nil {
			restored := *previous
			restored.UnreviewedRemoteSource =
				previous.UnreviewedRemoteSource &&
					current.UnreviewedRemoteSource
			entries[restored.Path] = &restored
		}
		aborted = true
		return true
	})
	return aborted, err
}

// SetExpirationIfGeneration updates expiration metadata only while path still
// names generation. Unrelated fields are retained.
func (r *Registry) SetExpirationIfGeneration(
	path string,
	generation string,
	repository string,
	branch string,
	expiresAt *time.Time,
) (bool, error) {
	repository, _ = repositoryurl.CanonicalRepositoryIdentity(repository)
	updated := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		if len(keys) == 0 {
			entries[path] = &WorktreeEntry{
				Repository:   repository,
				Branch:       branch,
				Path:         path,
				RegisteredAt: time.Now(),
				ExpiresAt:    expiresAt,
				Generation:   generation,
			}
			updated = true
			return true
		}
		if len(keys) != 1 {
			return false
		}
		entry := entries[keys[0]]
		if entry.CreationToken != "" ||
			entry.Generation != generation {
			return false
		}
		entry.Repository = repository
		entry.Branch = branch
		entry.Path = path
		entry.IsMain = false
		entry.ExpiresAt = expiresAt
		updated = true
		return true
	})
	return updated, err
}

func sameWorktreeEntry(left *WorktreeEntry, right *WorktreeEntry) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Repository == right.Repository &&
		left.Branch == right.Branch &&
		left.Path == right.Path &&
		left.Hash == right.Hash &&
		left.IsMain == right.IsMain &&
		left.RegisteredAt.Equal(right.RegisteredAt) &&
		sameOptionalTime(left.ExpiresAt, right.ExpiresAt) &&
		left.Generation == right.Generation &&
		left.CreationToken == right.CreationToken &&
		left.UnreviewedRemoteSource == right.UnreviewedRemoteSource
}

func sameOptionalTime(left *time.Time, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

// Unregister removes a worktree entry by path.
func (r *Registry) Unregister(path string) error {
	return r.mutate(func(entries map[string]*WorktreeEntry) bool {
		keys := matchingRegistryKeys(entries, path)
		for _, key := range keys {
			delete(entries, key)
		}
		return len(keys) > 0
	})
}

// UnregisterIfGeneration removes path only while its registry entry still
// names the generation the caller acted on.
func (r *Registry) UnregisterIfGeneration(
	path string,
	generation string,
) (bool, error) {
	removed := false
	err := r.mutate(func(entries map[string]*WorktreeEntry) bool {
		for _, key := range matchingRegistryKeys(entries, path) {
			if entries[key].CreationToken != "" ||
				entries[key].Generation != generation {
				continue
			}
			delete(entries, key)
			removed = true
		}
		return removed
	})
	return removed, err
}

// List returns all registered worktrees.
func (r *Registry) List() []*WorktreeEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]*WorktreeEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}

	return entries
}

// ListByRepository returns worktrees for a specific repository.
func (r *Registry) ListByRepository(repository string) []*WorktreeEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var entries []*WorktreeEntry
	for _, entry := range r.entries {
		if entry.Repository == repository {
			entries = append(entries, entry)
		}
	}

	return entries
}

// Get returns a worktree entry by path.
func (r *Registry) Get(path string) (*WorktreeEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entryForPath(path)
	if !ok {
		return nil, false
	}
	copied := *entry
	return &copied, true
}

// IsUnreviewedRemoteSource reports whether path was created from remote
// content that has not yet been explicitly opened by the user.
func (r *Registry) IsUnreviewedRemoteSource(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.entryForPath(path)
	return ok && entry.UnreviewedRemoteSource
}

// AcknowledgeRemoteSource marks a remote-source worktree as reviewed while
// preserving any other registry metadata such as its expiration.
func (r *Registry) AcknowledgeRemoteSource(path string) error {
	return r.mutate(func(entries map[string]*WorktreeEntry) bool {
		changed := false
		for _, key := range matchingRegistryKeys(entries, path) {
			if entries[key].UnreviewedRemoteSource {
				entries[key].UnreviewedRemoteSource = false
				changed = true
			}
		}
		return changed
	})
}

func (r *Registry) entryForPath(path string) (*WorktreeEntry, bool) {
	return entryForPath(r.entries, path)
}

func entryForPath(
	entries map[string]*WorktreeEntry,
	path string,
) (*WorktreeEntry, bool) {
	if entry, ok := entries[path]; ok {
		return entry, true
	}
	want := PathKey(path)
	for _, entry := range entries {
		if PathKey(entry.Path) == want {
			return entry, true
		}
	}
	return nil, false
}

func matchingRegistryKeys(
	entries map[string]*WorktreeEntry,
	path string,
) []string {
	want := PathKey(path)
	keys := make([]string, 0, 1)
	for key, entry := range entries {
		if key == path || entry.Path == path ||
			PathKey(entry.Path) == want {
			keys = append(keys, key)
		}
	}
	return keys
}

// PathKey returns the canonical identity used for registry path matching. It
// resolves the nearest existing ancestor before restoring missing path
// components, then applies the platform's path comparison rules.
func PathKey(path string) string {
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	current := filepath.Clean(path)
	var missing []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return utils.PathKey(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return utils.PathKey(path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// ListExpired returns all worktrees that have expired.
func (r *Registry) ListExpired() []*WorktreeEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var entries []*WorktreeEntry
	for _, entry := range r.entries {
		if entry.IsExpired() {
			entries = append(entries, entry)
		}
	}

	return entries
}

// Cleanup removes entries that no longer exist on disk.
func (r *Registry) Cleanup() error {
	return r.mutate(func(entries map[string]*WorktreeEntry) bool {
		changed := false
		for path := range entries {
			gitDir := filepath.Join(path, ".git")
			if _, err := os.Stat(gitDir); os.IsNotExist(err) {
				delete(entries, path)
				changed = true
			}
		}
		return changed
	})
}
