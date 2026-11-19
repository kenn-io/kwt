package tmuxrestore

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kwt/internal/pullrequest"
	kwttmux "go.kenn.io/kwt/internal/tmux"
)

// Protection returns the paths of pull-request workspaces. Restore never
// saves or starts anything in them, because their code came from a
// contributor and kwt opens them only as inert shells.
type Protection func(ctx context.Context) ([]string, error)

// ProvenanceProtection reads kwt's pull-request provenance store, the same
// authority kwt's open path uses to refuse protected workspaces.
func ProvenanceProtection(storePath string) Protection {
	return func(ctx context.Context) ([]string, error) {
		var paths []string
		err := pullrequest.NewFileStore(storePath).View(ctx, func(records map[string]pullrequest.Provenance) error {
			for _, record := range records {
				if record.Workspace.Path != "" {
					paths = append(paths, record.Workspace.Path)
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read pull-request provenance: %w", err)
		}
		return paths, nil
	}
}

// protectedSet answers whether a session or directory belongs to a
// pull-request workspace. A set built from a failed read protects everything
// it cannot clear.
type protectedSet struct {
	failed     bool
	identities map[string]bool
	roots      []string
}

func loadProtection(ctx context.Context, protection Protection) protectedSet {
	if protection == nil {
		return protectedSet{}
	}
	paths, err := protection(ctx)
	if err != nil {
		return protectedSet{failed: true}
	}
	set := protectedSet{identities: map[string]bool{}}
	for _, path := range paths {
		set.identities[kwttmux.WorkspacePathIdentity(path)] = true
		set.roots = append(set.roots, resolvePath(path))
	}
	return set
}

// session reports whether a session with these workspace markers must not be
// saved or restored.
func (p protectedSet) session(s Session) bool {
	if s.WorkspaceIdentity == "" {
		return false
	}
	return p.failed || p.identities[s.WorkspaceIdentity]
}

// dir reports whether no program may start in dir.
func (p protectedSet) dir(dir string) bool {
	if p.failed {
		return true
	}
	resolved := resolvePath(dir)
	for _, root := range p.roots {
		rel, err := filepath.Rel(root, resolved)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func resolvePath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

// apply removes protected sessions and the program records of panes in
// protected directories.
func (p protectedSet) apply(snap *Snapshot) {
	snap.Sessions = slices.DeleteFunc(snap.Sessions, p.session)
	used := map[string]bool{}
	for _, session := range snap.Sessions {
		for _, ref := range session.Windows {
			used[ref.Key] = true
		}
	}
	for key, window := range snap.Windows {
		if !used[key] {
			delete(snap.Windows, key)
			continue
		}
		for i := range window.Panes {
			if p.dir(window.Panes[i].Dir) {
				window.Panes[i].Resume = nil
				window.Panes[i].PresetAgent = ""
			}
		}
		snap.Windows[key] = window
	}
}
