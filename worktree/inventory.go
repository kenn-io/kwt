package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/fslink"
	gitworktree "go.kenn.io/kit/git/worktree"
)

// GenerationStatus describes a durable generation without initializing it.
type GenerationStatus string

const (
	GenerationValid      GenerationStatus = "valid"
	GenerationMissing    GenerationStatus = "missing"
	GenerationInvalid    GenerationStatus = "invalid"
	GenerationUnreadable GenerationStatus = "unreadable"
)

// Entry is a read-only snapshot of one Git worktree record.
type Entry struct {
	CommonDir         string           `json:"common_dir"`
	Bare              bool             `json:"bare"`
	Detached          bool             `json:"detached"`
	Path              string           `json:"path"`
	Branch            string           `json:"branch"`
	Head              string           `json:"head"`
	Generation        string           `json:"generation"`
	GitDir            string           `json:"git_dir"`
	DotGitTarget      string           `json:"dot_git_target"`
	GenerationStatus  GenerationStatus `json:"generation_status"`
	IsMain            bool             `json:"is_main"`
	Exists            bool             `json:"exists"`
	Locked            bool             `json:"locked"`
	Prunable          bool             `json:"prunable"`
	LockedReason      string           `json:"locked_reason"`
	PrunableReason    string           `json:"prunable_reason"`
	GitDirError       string           `json:"git_dir_error"`
	CreatedAt         time.Time        `json:"created_at"`
	registrationError error
}

// Inventory reports whether all registered paths could be observed.
type Inventory struct {
	Entries  []Entry
	Complete bool
}

// IncompleteInventoryError prevents callers from replacing a complete snapshot
// with a partially observed repository.
type IncompleteInventoryError struct {
	Path string
	Err  error
}

func (e *IncompleteInventoryError) Error() string {
	return fmt.Sprintf("incomplete worktree inventory at %s: %v", e.Path, e.Err)
}
func (e *IncompleteInventoryError) Unwrap() error { return e.Err }

func IsIncompleteInventory(err error) bool {
	var incomplete *IncompleteInventoryError
	return errors.As(err, &incomplete)
}

// Inspect lists registrations without initializing identity files.
func (r *Repository) Inspect(ctx context.Context) (inventory Inventory, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { inventory, err = s.Inspect(ctx); return err })
	return inventory, err
}
func (s *Scope) Inspect(ctx context.Context) (Inventory, error) { return s.inspect(ctx, true) }

// List initializes the selected identity policy under the same inventory lock.
func (r *Repository) List(ctx context.Context, identity IdentityPolicy) (entries []Entry, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { entries, err = s.List(ctx, identity); return err })
	return entries, err
}
func (s *Scope) List(ctx context.Context, identity IdentityPolicy) ([]Entry, error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	inventory, err := s.inspect(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range inventory.Entries {
		e := &inventory.Entries[i]
		if identity.FileName == "" {
			continue
		}
		value, err := s.EnsureIdentity(ctx, e.Path, identity)
		if err != nil {
			return nil, &IncompleteInventoryError{Path: e.Path, Err: fmt.Errorf("initialize worktree identity: %w", err)}
		}
		if identity.Generate {
			e.Generation = value
			e.GenerationStatus = GenerationValid
		}
	}
	return inventory.Entries, nil
}

func (s *Scope) inspect(ctx context.Context, expire bool) (Inventory, error) {
	if err := s.check(ctx); err != nil {
		return Inventory{}, err
	}
	excluded, err := s.activeCreation()
	if err != nil {
		return Inventory{}, err
	}
	args := []string{"worktree", "list", "--porcelain"}
	if expire {
		args = append(args, "--expire", "now")
	}
	output, err := s.repo.run(ctx, s.repo.path, args...)
	if err != nil {
		return Inventory{}, fmt.Errorf("list worktrees: %w", err)
	}
	raw := gitworktree.ParsePorcelain(string(output))
	main, err := s.repo.mainRoot(ctx, raw)
	if err != nil {
		return Inventory{}, err
	}
	if len(raw) > 0 && !raw[0].Bare {
		raw[0].Path = main
	}
	inventory := Inventory{Entries: make([]Entry, 0, len(raw))}
	for _, entry := range raw {
		if entry.Bare || (excluded != "" && comparableWorktreePath(entry.Path) == comparableWorktreePath(excluded)) {
			continue
		}
		target, err := ReadWorktreeBacklink(ctx, entry.Path)
		if err != nil {
			return inventory, &IncompleteInventoryError{Path: entry.Path, Err: fmt.Errorf("inspect worktree backlink: %w", err)}
		}
		e := Entry{Path: entry.Path, CommonDir: s.repo.commonDir, Branch: entry.Branch, Head: entry.Head, DotGitTarget: target, Detached: entry.Detached, Locked: entry.Locked, LockedReason: entry.LockedReason, Prunable: entry.Prunable, PrunableReason: entry.PrunableReason, GenerationStatus: GenerationUnreadable, IsMain: main != "" && pathKey(entry.Path) == pathKey(main)}
		if e.Branch == "" {
			e.Branch = "HEAD"
		}
		if info, err := os.Stat(e.Path); err == nil {
			e.Exists = true
			e.CreatedAt = info.ModTime()
		} else if !os.IsNotExist(err) {
			return inventory, &IncompleteInventoryError{Path: e.Path, Err: err}
		}
		dir, err := s.repo.registration(e.Path)
		if err != nil {
			e.GitDirError = err.Error()
			e.registrationError = err
		} else {
			e.GitDir = dir
			e.Generation, e.GenerationStatus = inspectGeneration(dir)
		}
		inventory.Entries = append(inventory.Entries, e)
	}
	inventory.Complete = true
	return inventory, nil
}

func inspectGeneration(dir string) (string, GenerationStatus) {
	data, err := fslink.ReadFile(filepath.Join(dir, "kwt-generation"))
	if os.IsNotExist(err) {
		return "", GenerationMissing
	}
	if err != nil {
		return "", GenerationUnreadable
	}
	generation := strings.TrimSpace(string(data))
	if ValidateWorktreeGeneration(generation) != nil {
		return generation, GenerationInvalid
	}
	return generation, GenerationValid
}

func (r *Repository) mainRoot(ctx context.Context, entries []gitworktree.PorcelainEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	if entries[0].Bare {
		// Container-style repositories treat their main linked checkout as primary.
		if filepath.Base(r.commonDir) == ".bare" {
			anchor := filepath.Join(filepath.Dir(r.commonDir), "main")
			for _, entry := range entries[1:] {
				if pathKey(entry.Path) == pathKey(anchor) {
					return entry.Path, nil
				}
			}
		}
		return entries[0].Path, nil
	}
	dir, err := r.run(ctx, r.path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if pathKey(r.path) != pathKey(r.commonDir) && pathKey(strings.TrimSpace(string(dir))) == pathKey(r.commonDir) {
		root, err := r.run(ctx, r.path, "rev-parse", "--show-toplevel")
		return strings.TrimSpace(string(root)), err
	}
	if filepath.Base(r.commonDir) == ".git" {
		root := filepath.Dir(r.commonDir)
		if dir, err := r.registration(root); err == nil && pathKey(dir) == pathKey(r.commonDir) {
			return root, nil
		}
	}
	if pathKey(entries[0].Path) != pathKey(r.commonDir) {
		return entries[0].Path, nil
	}
	root, err := r.run(ctx, r.path, "config", "--path", "--get", "core.worktree")
	if err == nil && strings.TrimSpace(string(root)) != "" {
		path := strings.TrimSpace(string(root))
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.commonDir, path)
		}
		if dir, err := r.registration(path); err == nil && pathKey(dir) == pathKey(r.commonDir) {
			return path, nil
		}
		return "", fmt.Errorf("configured core.worktree does not name the main worktree for %s", r.commonDir)
	}
	return "", fmt.Errorf("main worktree path is unavailable for separate Git directory %s", r.commonDir)
}

// HasExactWorktreeRoot requires a unique inventory entry at the selected path.
func HasExactWorktreeRoot(entries []Entry, path string) bool {
	count := 0
	for _, entry := range entries {
		if pathKey(entry.Path) == pathKey(path) {
			count++
		}
	}
	return count == 1
}

// PrimaryPath resolves the repository's primary checkout without creating
// identities or taking a mutation lock. It is an advisory path lookup, including
// when application policy needs it while a Scope is already held. A bare
// repository returns its bare root, or the main checkout beside a
// conventional .bare control directory.
func (r *Repository) PrimaryPath(ctx context.Context) (string, error) {
	dir, err := r.run(ctx, r.path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if pathKey(r.path) != pathKey(r.commonDir) && pathKey(strings.TrimSpace(string(dir))) == pathKey(r.commonDir) && !r.bare {
		root, err := r.run(ctx, r.path, "rev-parse", "--show-toplevel")
		return strings.TrimSpace(string(root)), err
	}
	if filepath.Base(r.commonDir) == ".git" {
		root := filepath.Dir(r.commonDir)
		if dir, err := r.registration(root); err == nil && pathKey(dir) == pathKey(r.commonDir) {
			return root, nil
		}
	}
	out, err := r.run(ctx, r.path, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	primary, err := r.mainRoot(ctx, gitworktree.ParsePorcelain(string(out)))
	if err == nil && primary == "" {
		err = fmt.Errorf("primary worktree path is unavailable for %s", r.commonDir)
	}
	return primary, err
}
