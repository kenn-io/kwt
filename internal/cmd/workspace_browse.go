package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/utils"
)

type workspaceFolder struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	ProjectPath string `json:"project_path,omitempty"`
}

type workspaceFolderListing struct {
	Path      string            `json:"path"`
	Entries   []workspaceFolder `json:"entries"`
	Truncated bool              `json:"truncated"`
}

var workspaceBrowseCmd *cobra.Command

func init() {
	workspaceBrowseCmd = &cobra.Command{
		Use:   "browse <path>",
		Short: "List visible child folders without registering them (JSON)",
		Args:  cobra.ExactArgs(1),
		// Browsing does not load repository config or mutate the registry.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			listing, err := browseWorkspace(commandContext(cmd), args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(listing)
		},
	}
	workspaceCmd.AddCommand(workspaceBrowseCmd)
}

// browseWorkspace reads one level only. Symlink entries are omitted so
// expanding a tree cannot follow cycles or silently leave the selected tree.
func browseWorkspace(ctx context.Context, path string) (workspaceFolderListing, error) {
	return browseWorkspaceWithProbe(ctx, path, browseProjectPath)
}

func browseWorkspaceWithProbe(ctx context.Context, path string, probe func(context.Context, string) (string, error)) (workspaceFolderListing, error) {
	result := workspaceFolderListing{Entries: []workspaceFolder{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !filepath.IsAbs(path) {
		return result, fmt.Errorf("browse requires an absolute folder path")
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return result, fmt.Errorf("resolve folder: %w", err)
	}
	result.Path = root
	directory, err := openBrowseDirectory(root)
	if err != nil {
		return result, fmt.Errorf("open folder: %w", err)
	}
	defer directory.Close()
	// Bound filesystem enumeration as well as Git probes per expansion.
	entries, err := directory.ReadDir(1001)
	if cancellation := ctx.Err(); cancellation != nil {
		return result, cancellation
	}
	if err != nil && err != io.EOF {
		return result, fmt.Errorf("read folder: %w", err)
	}
	if len(entries) > 1000 {
		result.Truncated = true
		entries = entries[:1000]
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		folder := workspaceFolder{Name: entry.Name(), Path: filepath.Join(root, entry.Name())}
		// Only probe likely roots; Git's upward search would otherwise label
		// every ordinary subdirectory of a checkout as a project.
		for _, marker := range []string{".git", ".bare", filepath.Join("main", ".git")} {
			if _, err := os.Lstat(filepath.Join(folder.Path, marker)); err == nil {
				if projectPath, err := probe(ctx, folder.Path); err == nil {
					folder.ProjectPath = projectPath
				}
				if err := ctx.Err(); err != nil {
					return result, err
				}
				break
			}
		}
		result.Entries = append(result.Entries, folder)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Name < result.Entries[j].Name })
	return result, ctx.Err()
}

func browseProjectPath(ctx context.Context, path string) (string, error) {
	repository := git.NewWithContext(ctx, path)
	// Inspect the live checkout as well as the recorded inventory. A stale
	// worktree record alone cannot establish that main still belongs here.
	if info, err := os.Stat(filepath.Join(path, ".bare")); err == nil && info.IsDir() {
		main := filepath.Join(path, "main")
		candidate := git.NewWithContext(ctx, main)
		if container, err := candidate.GetBareContainerPath(); err == nil &&
			utils.PathKey(container) == utils.PathKey(path) {
			if checkout, err := candidate.GetRepositoryPath(); err == nil &&
				utils.PathKey(checkout) == utils.PathKey(main) {
				return filepath.EvalSymlinks(main)
			}
		}
	}
	// Never allow Git's upward search to turn an ordinary child into its
	// enclosing checkout, even when that child has misleading layout markers.
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return "", err
	}
	checkout, err := repository.GetRepositoryPath()
	if err != nil {
		return "", err
	}
	if utils.PathKey(checkout) != utils.PathKey(path) {
		return "", fmt.Errorf("folder is not a checkout root")
	}
	primary, err := repository.GetMainRepositoryPath()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(primary)
}
