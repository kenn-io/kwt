// Package registry provides global worktree tracking across repositories.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			// Registry doesn't exist yet, that's OK
			return nil
		}
		return fmt.Errorf("failed to read registry: %w", err)
	}

	if len(data) == 0 {
		return nil
	}

	var entries []*WorktreeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("failed to unmarshal registry: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries = make(map[string]*WorktreeEntry)
	for _, entry := range entries {
		r.entries[entry.Path] = entry
	}

	return nil
}

// save writes the registry to disk.
func (r *Registry) save() error {
	r.mu.RLock()
	entries := make([]*WorktreeEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal registry: %w", err)
	}

	if err := os.WriteFile(r.path, data, 0644); err != nil {
		return fmt.Errorf("failed to write registry: %w", err)
	}

	return nil
}

// Register adds or updates a worktree entry.
func (r *Registry) Register(entry *WorktreeEntry) error {
	r.mu.Lock()
	entry.RegisteredAt = time.Now()
	r.entries[entry.Path] = entry
	r.mu.Unlock()

	return r.save()
}

// Unregister removes a worktree entry by path.
func (r *Registry) Unregister(path string) error {
	r.mu.Lock()
	delete(r.entries, path)
	r.mu.Unlock()

	return r.save()
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

	entry, ok := r.entries[path]
	return entry, ok
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
	r.mu.Lock()
	entry, ok := r.entryForPath(path)
	if !ok || !entry.UnreviewedRemoteSource {
		r.mu.Unlock()
		return nil
	}
	entry.UnreviewedRemoteSource = false
	r.mu.Unlock()
	return r.save()
}

func (r *Registry) entryForPath(path string) (*WorktreeEntry, bool) {
	if entry, ok := r.entries[path]; ok {
		return entry, true
	}
	want := comparableRegistryPath(path)
	for _, entry := range r.entries {
		if comparableRegistryPath(entry.Path) == want {
			return entry, true
		}
	}
	return nil, false
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

// UnreviewedRemoteSourcePaths returns the paths that automatic status and
// fleet collection must not inspect.
func (r *Registry) UnreviewedRemoteSourcePaths() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	paths := make([]string, 0)
	for _, entry := range r.entries {
		if entry.UnreviewedRemoteSource {
			paths = append(paths, entry.Path)
		}
	}
	return paths
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
	r.mu.Lock()
	defer r.mu.Unlock()

	var toRemove []string
	for path, entry := range r.entries {
		// Check if the worktree directory still exists
		gitDir := filepath.Join(path, ".git")
		if _, err := os.Stat(gitDir); os.IsNotExist(err) {
			toRemove = append(toRemove, entry.Path)
		}
	}

	for _, path := range toRemove {
		delete(r.entries, path)
	}

	if len(toRemove) > 0 {
		return r.save()
	}

	return nil
}
