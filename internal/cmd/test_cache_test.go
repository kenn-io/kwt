package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	testCacheMu        sync.Mutex
	testCacheRoot      string
	testDaemonBinaries = map[daemonTestBuild]string{}
	testToolBinaries   = map[string]string{}
	testRepoTemplate   string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testCacheRoot != "" {
		_ = os.RemoveAll(testCacheRoot)
	}
	os.Exit(code)
}

func testCacheDir(t *testing.T, sub string) string {
	t.Helper()
	if testCacheRoot == "" {
		dir, err := os.MkdirTemp("", "kwt-cmd-test-cache-")
		require.NoError(t, err)
		testCacheRoot = dir
	}
	dir := filepath.Join(testCacheRoot, sub)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return dir
}

func cachedDaemonTestBinary(t *testing.T, build daemonTestBuild) string {
	t.Helper()
	testCacheMu.Lock()
	defer testCacheMu.Unlock()
	if path, ok := testDaemonBinaries[build]; ok {
		return path
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	dir := testCacheDir(t, fmt.Sprintf("daemon-%d", len(testDaemonBinaries)))
	path := filepath.Join(dir, build.Name+suffix)
	command := exec.CommandContext(
		t.Context(),
		"go",
		"build",
		"-buildvcs=false",
		"-ldflags",
		"-X go.kenn.io/kwt/internal/cmd.version="+build.Version+
			" -X go.kenn.io/kwt/internal/cmd.commit="+build.Revision+
			" -X go.kenn.io/kwt/internal/cmd.revisionTime="+build.RevisionTime,
		"-o",
		path,
		"./cmd/kwt",
	)
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	testDaemonBinaries[build] = path
	return path
}

func cachedTestToolBinary(t *testing.T, pkg, name string) string {
	t.Helper()
	testCacheMu.Lock()
	defer testCacheMu.Unlock()
	key := pkg + "\x00" + name
	if path, ok := testToolBinaries[key]; ok {
		return path
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	dir := testCacheDir(t, fmt.Sprintf("tool-%d", len(testToolBinaries)))
	path := filepath.Join(dir, name+suffix)
	command := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", path, pkg)
	command.Dir = root
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	testToolBinaries[key] = path
	return path
}

func copyTUITestRepoTemplate(t *testing.T, repoPath string) {
	t.Helper()
	templateDir := ensureTUITestRepoTemplate(t)
	require.NoError(t, os.CopyFS(repoPath, os.DirFS(templateDir)))
}

func ensureTUITestRepoTemplate(t *testing.T) string {
	t.Helper()
	testCacheMu.Lock()
	defer testCacheMu.Unlock()
	if testRepoTemplate != "" {
		return testRepoTemplate
	}
	dir := testCacheDir(t, "repo-template")
	runTUITestGit(t, "", "init", "-b", "main", dir)
	runTUITestGit(t, dir, "config", "user.name", "Test User")
	runTUITestGit(t, dir, "config", "user.email", "test@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test Repository\n"), 0o644))
	runTUITestGit(t, dir, "add", ".")
	runTUITestGit(t, dir, "commit", "-m", "Initial commit")
	testRepoTemplate = dir
	return dir
}
