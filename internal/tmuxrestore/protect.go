package tmuxrestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	kwttmux "go.kenn.io/kwt/internal/tmux"
)

// ProtectedWorkspace is a pull-request workspace as its provenance records
// it. A checkout that moved keeps its generation, so either one identifies it.
type ProtectedWorkspace struct {
	Path       string
	Generation string
}

// Protection returns the pull-request workspaces. Restore never saves or
// starts anything in them, because their code came from a contributor and
// kwt opens them only as inert shells.
type Protection func(ctx context.Context) ([]ProtectedWorkspace, error)

// ProvenanceProtection reads kwt's pull-request provenance store, the same
// authority kwt's open path uses to refuse protected workspaces.
func ProvenanceProtection(storePath string) Protection {
	return func(ctx context.Context) ([]ProtectedWorkspace, error) {
		var workspaces []ProtectedWorkspace
		err := pullrequest.NewFileStore(storePath).View(ctx, func(records map[string]pullrequest.Provenance) error {
			for _, record := range records {
				w := record.Workspace
				if w.Path != "" || w.Generation != "" {
					workspaces = append(workspaces, ProtectedWorkspace{Path: w.Path, Generation: w.Generation})
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read pull-request provenance: %w", err)
		}
		return workspaces, nil
	}
}

// protectedSet answers whether a session or directory belongs to a
// pull-request workspace. A set built from a failed read protects everything
// it cannot clear.
type protectedSet struct {
	failed      bool
	identities  map[string]bool
	generations map[string]bool
	roots       []string
	// dirGenerations caches the checkout generation found for each
	// directory.
	dirGenerations map[string]string
}

func loadProtection(ctx context.Context, protection Protection) protectedSet {
	if protection == nil {
		return protectedSet{}
	}
	workspaces, err := protection(ctx)
	if err != nil {
		return protectedSet{failed: true}
	}
	set := protectedSet{identities: map[string]bool{}, generations: map[string]bool{}, dirGenerations: map[string]string{}}
	for _, w := range workspaces {
		if w.Path != "" {
			set.identities[kwttmux.WorkspacePathIdentity(w.Path)] = true
			set.roots = append(set.roots, resolvePath(w.Path))
		}
		if w.Generation != "" {
			set.generations[w.Generation] = true
		}
	}
	return set
}

// session reports whether a session with these workspace markers must not be
// saved or restored.
func (p protectedSet) session(s Session) bool {
	if s.WorkspaceIdentity == "" {
		return false
	}
	return p.failed || p.identities[s.WorkspaceIdentity] ||
		s.WorkspaceGeneration != "" && p.generations[s.WorkspaceGeneration]
}

// dir reports whether no program may start in dir: it is inside a
// pull-request workspace's recorded path, or inside a checkout carrying a
// pull-request workspace's generation, wherever that checkout now is.
func (p protectedSet) dir(ctx context.Context, dir string) bool {
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
	if len(p.generations) == 0 {
		return false
	}
	generation, ok := p.dirGenerations[resolved]
	if !ok {
		generation = checkoutGeneration(ctx, resolved)
		p.dirGenerations[resolved] = generation
	}
	return p.generations[generation]
}

// checkoutGeneration returns the kwt generation of the Git checkout holding
// dir, or "" when dir is in no checkout or the checkout has none. kwt keeps
// the generation in the checkout's Git directory, which moves with it.
func checkoutGeneration(ctx context.Context, dir string) string {
	gitDir, err := git.NewWithContext(ctx, dir).RunWithContext(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(strings.TrimSpace(gitDir), "kwt-generation"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
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
func (p protectedSet) apply(ctx context.Context, snap *Snapshot) {
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
			if p.dir(ctx, window.Panes[i].Dir) {
				window.Panes[i].Resume = nil
				window.Panes[i].PresetAgent = ""
			}
		}
		snap.Windows[key] = window
	}
}
