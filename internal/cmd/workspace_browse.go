package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	folders, truncated, err := readBrowseFolders(ctx, directory)
	if err != nil {
		return result, err
	}
	result.Truncated = truncated
	for _, name := range folders {
		folder := workspaceFolder{Name: name, Path: filepath.Join(root, name)}
		// Only probe likely roots; Git's upward search would otherwise label
		// every ordinary subdirectory of a checkout as a project.
		for _, marker := range []string{".git", ".bare"} {
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
	slices.SortFunc(result.Entries, func(a, b workspaceFolder) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Name, b.Name))
	})
	return result, ctx.Err()
}

const (
	browseFolderLimit = 1000
	browseEntryLimit  = 10000
)

// readBrowseFolders returns up to browseFolderLimit visible child folders,
// inspecting at most browseEntryLimit entries so files cannot exhaust the
// folder budget and huge folders still stay bounded.
func readBrowseFolders(ctx context.Context, directory *os.File) ([]string, bool, error) {
	var folders []string
	inspected := 0
	for {
		batch, err := directory.ReadDir(256)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, false, cancellation
		}
		for _, entry := range batch {
			listable := entry.IsDir() && !strings.HasPrefix(entry.Name(), ".")
			if inspected == browseEntryLimit || (listable && len(folders) == browseFolderLimit) {
				return folders, true, nil
			}
			inspected++
			if listable {
				folders = append(folders, entry.Name())
			}
		}
		if errors.Is(err, io.EOF) {
			return folders, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("read folder: %w", err)
		}
	}
}

func browseProjectPath(ctx context.Context, path string) (string, error) {
	repository := git.NewWithContext(ctx, path)
	// Inspect the live checkout as well as the recorded inventory. A stale
	// worktree record alone cannot establish that main still belongs here.
	if info, err := os.Stat(filepath.Join(path, ".bare")); err == nil && info.IsDir() {
		main := filepath.Join(path, "main")
		candidate := git.NewWithContext(ctx, main)
		if container, err := candidate.GetBareContainerPath(); err == nil && container != "" &&
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
		// A bare repository named .git has no work tree; kwt registers it by
		// its parent folder, where its linked worktrees also resolve.
		commonDir, commonErr := repository.GetCommonDir()
		if commonErr != nil || utils.PathKey(commonDir) != utils.PathKey(filepath.Join(path, ".git")) {
			return "", err
		}
		checkout = path
	}
	if utils.PathKey(checkout) != utils.PathKey(path) {
		return "", fmt.Errorf("folder is not a checkout root")
	}
	primary, err := repository.GetMainRepositoryPath()
	if err != nil {
		return "", err
	}
	if utils.PathKey(primary) != utils.PathKey(checkout) {
		// The inventory can still name a primary path after its checkout has been replaced.
		commonDir, err := repository.GetCommonDir()
		if err != nil {
			return "", err
		}
		primaryCommonDir, err := git.NewWithContext(ctx, primary).GetCommonDir()
		if err != nil {
			return "", err
		}
		if utils.PathKey(primaryCommonDir) != utils.PathKey(commonDir) {
			return "", fmt.Errorf("primary checkout belongs to a different repository")
		}
	}
	return filepath.EvalSymlinks(primary)
}
