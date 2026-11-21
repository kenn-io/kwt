package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBrowseWorkspaceOnlyListsVisibleImmediateDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"notes/deeper", "project", ".hidden"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), nil, 0600))
	require.NoError(t, os.Symlink(root, filepath.Join(root, "loop")))
	listing, err := browseWorkspace(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, listing.Entries, 2)
	require.Equal(t, "notes", listing.Entries[0].Name)
	require.Equal(t, "project", listing.Entries[1].Name)
	require.Empty(t, listing.Entries[0].ProjectPath)
}

func TestBrowseWorkspaceRecognizesCheckoutWithoutRegistering(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "widget")
	require.NoError(t, os.Mkdir(checkout, 0700))
	output, err := exec.CommandContext(t.Context(), "git", "-C", checkout, "init", "--quiet").CombinedOutput()
	require.NoError(t, err, string(output))
	canonical, err := filepath.EvalSymlinks(checkout)
	require.NoError(t, err)
	for _, existing := range []bool{false, true} {
		settings := filepath.Join(t.TempDir(), "settings")
		const config = "[[projects]]\nname = 'existing'\npath = '/existing'\n\n[[workspaces]]\nname = 'notes'\npath = '/notes'\n"
		if existing {
			require.NoError(t, os.Mkdir(settings, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(settings, "config.toml"), []byte(config), 0600))
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrowseCLIHelper$")
		cmd.Env = append(os.Environ(), "KWT_TEST_BROWSE_PATH="+root, "KWT_HOME="+settings)
		output, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, string(output))
		var listing workspaceFolderListing
		require.NoError(t, json.NewDecoder(strings.NewReader(string(output))).Decode(&listing))
		require.Len(t, listing.Entries, 1)
		require.Equal(t, canonical, listing.Entries[0].ProjectPath)
		if existing {
			contents, err := os.ReadFile(filepath.Join(settings, "config.toml"))
			require.NoError(t, err)
			require.Equal(t, config, string(contents))
			files, err := os.ReadDir(settings)
			require.NoError(t, err)
			require.Len(t, files, 1)
		} else {
			require.NoDirExists(t, settings)
		}
	}
}

func TestBrowseCLIHelper(t *testing.T) {
	path := os.Getenv("KWT_TEST_BROWSE_PATH")
	if path == "" {
		t.Skip("helper process")
	}
	os.Args = []string{os.Args[0], "workspace", "browse", path}
	Execute()
}

func TestBrowseWorkspaceRejectsMissingFolderAndCancellation(t *testing.T) {
	_, err := browseWorkspace(t.Context(), filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = browseWorkspace(ctx, t.TempDir())
	require.ErrorIs(t, err, context.Canceled)
}

func TestBrowseWorkspaceCancellationDuringFinalProbe(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "only-child", ".git"), 0700))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := browseWorkspaceWithProbe(ctx, root, func(context.Context, string) (string, error) {
		cancel()
		return "", context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestBrowseWorkspaceRecognizesBareContainerAndLinkedWorktrees(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "widget")
	require.NoError(t, os.Mkdir(container, 0700))
	run := func(args ...string) {
		t.Helper()
		output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, string(output))
	}
	bare := filepath.Join(container, ".bare")
	main := filepath.Join(container, "main")
	topic := filepath.Join(container, "topic")
	seed := t.TempDir()
	run("init", "--initial-branch=main", seed)
	run("-C", seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "Initial")
	run("clone", "--bare", seed, bare)
	run("--git-dir="+bare, "worktree", "add", main, "main")
	run("-C", main, "worktree", "add", "-b", "topic", topic)
	trace := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	listing, err := browseWorkspace(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, listing.Entries, 1)
	canonical, err := filepath.EvalSymlinks(main)
	require.NoError(t, err)
	require.Equal(t, canonical, listing.Entries[0].ProjectPath)
	events, err := os.ReadFile(trace)
	require.NoError(t, err)
	processes := 0
	for _, line := range strings.Split(strings.TrimSpace(string(events)), "\n") {
		var event struct {
			Event string `json:"event"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		if event.Event == "start" {
			processes++
		}
	}
	require.LessOrEqual(t, processes, 3, "container recognition must fit three Git probes")
	children, err := browseWorkspace(t.Context(), container)
	require.NoError(t, err)
	require.Len(t, children.Entries, 2)
	for _, entry := range children.Entries {
		require.Equal(t, canonical, entry.ProjectPath)
	}
}

func TestBrowseWorkspaceDoesNotMistakeNestedMainForBareAnchor(t *testing.T) {
	for _, layout := range []string{"malformed", "missing-main", "unrelated-main", "ordinary-checkout"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			container := filepath.Join(root, "widget")
			require.NoError(t, os.Mkdir(container, 0700))
			run := func(args ...string) {
				t.Helper()
				output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
				require.NoError(t, err, string(output))
			}
			bare := filepath.Join(container, ".bare")
			if layout == "missing-main" || layout == "unrelated-main" {
				run("init", "--bare", bare)
			} else {
				require.NoError(t, os.Mkdir(bare, 0700))
			}
			if layout == "unrelated-main" {
				run("init", filepath.Join(container, "main"))
				t.Chdir(container)
			}
			if layout == "ordinary-checkout" {
				run("init", container)
			} else {
				run("init", root)
			}
			listing, err := browseWorkspace(t.Context(), root)
			require.NoError(t, err)
			require.Len(t, listing.Entries, 1)
			if layout == "ordinary-checkout" {
				canonical, err := filepath.EvalSymlinks(container)
				require.NoError(t, err)
				require.Equal(t, canonical, listing.Entries[0].ProjectPath)
			} else {
				require.Empty(t, listing.Entries[0].ProjectPath)
			}
		})
	}
}

func TestBrowseWorkspaceRejectsReplacedBareCheckout(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "widget", ".bare")
	main := filepath.Join(root, "widget", "main")
	topic := filepath.Join(root, "widget", "topic")
	run := func(args ...string) {
		t.Helper()
		output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, string(output))
	}
	seed := t.TempDir()
	run("init", "--initial-branch=main", seed)
	run("-C", seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test",
		"commit", "--allow-empty", "-m", "Initial")
	run("clone", "--bare", seed, bare)
	run("--git-dir="+bare, "worktree", "add", main, "main")
	run("-C", main, "worktree", "add", "-b", "topic", topic)
	require.NoError(t, os.RemoveAll(main))
	run("init", main)
	listing, err := browseWorkspace(t.Context(), root)
	require.NoError(t, err)
	require.Empty(t, listing.Entries[0].ProjectPath)

	children, err := browseWorkspace(t.Context(), filepath.Dir(main))
	require.NoError(t, err)
	require.Len(t, children.Entries, 2)
	require.Equal(t, "topic", children.Entries[1].Name)
	require.Empty(t, children.Entries[1].ProjectPath)
}

func TestBrowseWorkspaceReportsTruncation(t *testing.T) {
	root := t.TempDir()
	for i := range 1002 {
		require.NoError(t, os.Mkdir(filepath.Join(root, fmt.Sprintf("folder-%04d", i)), 0700))
	}
	listing, err := browseWorkspace(t.Context(), root)
	require.NoError(t, err)
	require.True(t, listing.Truncated)
	require.Len(t, listing.Entries, 1000)
}
