// Package registry provides global worktree tracking across repositories.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
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
	configDir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".config")
	}

	registryDir := filepath.Join(configDir, "kwt")
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
	changed := change(entries)
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
	want := comparableRegistryPath(path)
	for _, entry := range entries {
		if comparableRegistryPath(entry.Path) == want {
			return entry, true
		}
	}
	return nil, false
}

func matchingRegistryKeys(
	entries map[string]*WorktreeEntry,
	path string,
) []string {
	want := comparableRegistryPath(path)
	keys := make([]string, 0, 1)
	for key, entry := range entries {
		if key == path || entry.Path == path ||
			comparableRegistryPath(entry.Path) == want {
			keys = append(keys, key)
		}
	}
	return keys
}

func comparableRegistryPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
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
