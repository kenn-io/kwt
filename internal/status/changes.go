package status

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/kwt/internal/git"
)

const collectChangesTimeout = 5 * time.Second

// FileState is the semantic state of one side of a changed path.
type FileState string

const (
	FileStateModified   FileState = "modified"
	FileStateAdded      FileState = "added"
	FileStateDeleted    FileState = "deleted"
	FileStateRenamed    FileState = "renamed"
	FileStateCopied     FileState = "copied"
	FileStateConflicted FileState = "conflicted"
	FileStateUntracked  FileState = "untracked"
)

// FileChange describes the index and worktree state of one resulting path.
type FileChange struct {
	Path         string    `json:"path"`
	OriginalPath string    `json:"original_path,omitempty"`
	Index        FileState `json:"index,omitempty"`
	Worktree     FileState `json:"worktree,omitempty"`
}

// ChangeState summarizes the highest-priority local state in a change set.
type ChangeState string

const (
	ChangeStateClean      ChangeState = "clean"
	ChangeStateModified   ChangeState = "modified"
	ChangeStateStaged     ChangeState = "staged"
	ChangeStateConflicted ChangeState = "conflicted"
)

// ChangeSummary contains canonical per-path counts. Staged is orthogonal to
// the mutually exclusive semantic buckets.
type ChangeSummary struct {
	Modified  int `json:"modified"`
	Added     int `json:"added"`
	Deleted   int `json:"deleted"`
	Untracked int `json:"untracked"`
	Staged    int `json:"staged"`
	Conflicts int `json:"conflicts"`
}

// ChangeSet is a point-in-time semantic view of local worktree changes.
type ChangeSet struct {
	State   ChangeState   `json:"state"`
	Summary ChangeSummary `json:"summary"`
	Files   []FileChange  `json:"files"`
}

// CollectChanges reads one bounded, foreground local-change snapshot.
func CollectChanges(
	ctx context.Context,
	path string,
	protectedNames []string,
) (ChangeSet, error) {
	gitContext, cancel := context.WithTimeout(ctx, collectChangesTimeout)
	defer cancel()

	output, err := git.NewForInventory(
		gitContext,
		path,
		protectedNames,
	).RunBytesWithEnvironment(
		map[string]string{
			"GIT_OPTIONAL_LOCKS": "0",
			"LC_ALL":             "C",
		},
		"status",
		"--porcelain=v2",
		"-z",
		"--untracked-files=all",
	)
	if err != nil {
		if gitContext.Err() != nil {
			return ChangeSet{}, fmt.Errorf(
				"collect local changes: %w",
				gitContext.Err(),
			)
		}
		return ChangeSet{}, fmt.Errorf("collect local changes: %w", err)
	}
	files, err := parseChangePorcelainV2(output)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("parse local changes: %w", err)
	}
	files, err = coalesceFileChanges(files)
	if err != nil {
		return ChangeSet{}, fmt.Errorf("coalesce local changes: %w", err)
	}
	return deriveChangeSet(files), nil
}

func coalesceFileChanges(files []FileChange) ([]FileChange, error) {
	coalesced := make([]FileChange, 0, len(files))
	byPath := make(map[string]int, len(files))
	for _, file := range files {
		index, exists := byPath[file.Path]
		if !exists {
			byPath[file.Path] = len(coalesced)
			coalesced = append(coalesced, file)
			continue
		}

		current := &coalesced[index]
		if current.Index != "" && file.Index != "" && current.Index != file.Index {
			return nil, fmt.Errorf("incompatible index states for path %q", file.Path)
		}
		if current.Worktree != "" && file.Worktree != "" && current.Worktree != file.Worktree {
			return nil, fmt.Errorf("incompatible worktree states for path %q", file.Path)
		}
		if current.OriginalPath != "" && file.OriginalPath != "" && current.OriginalPath != file.OriginalPath {
			return nil, fmt.Errorf("incompatible original path values for path %q", file.Path)
		}
		if current.Index == "" {
			current.Index = file.Index
		}
		if current.Worktree == "" {
			current.Worktree = file.Worktree
		}
		if current.OriginalPath == "" {
			current.OriginalPath = file.OriginalPath
		}
	}
	return coalesced, nil
}

func deriveChangeSet(files []FileChange) ChangeSet {
	ordered := append([]FileChange(nil), files...)
	if ordered == nil {
		ordered = make([]FileChange, 0)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path == ordered[j].Path {
			return ordered[i].OriginalPath < ordered[j].OriginalPath
		}
		return ordered[i].Path < ordered[j].Path
	})

	result := ChangeSet{State: ChangeStateClean, Files: ordered}
	for _, file := range ordered {
		if file.Index != "" && file.Index != FileStateConflicted {
			result.Summary.Staged++
		}
		switch primaryFileState(file) {
		case FileStateConflicted:
			result.Summary.Conflicts++
		case FileStateUntracked:
			result.Summary.Untracked++
		case FileStateAdded:
			result.Summary.Added++
		case FileStateDeleted:
			result.Summary.Deleted++
		case FileStateModified, FileStateRenamed, FileStateCopied:
			result.Summary.Modified++
		}
	}

	switch {
	case result.Summary.Conflicts > 0:
		result.State = ChangeStateConflicted
	case result.Summary.Staged > 0:
		result.State = ChangeStateStaged
	case len(result.Files) > 0:
		result.State = ChangeStateModified
	}
	return result
}

func primaryFileState(file FileChange) FileState {
	states := []FileState{file.Index, file.Worktree}
	for _, candidate := range []FileState{
		FileStateConflicted,
		FileStateUntracked,
		FileStateAdded,
		FileStateDeleted,
		FileStateRenamed,
		FileStateCopied,
		FileStateModified,
	} {
		for _, state := range states {
			if state == candidate {
				return candidate
			}
		}
	}
	return ""
}
