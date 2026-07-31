package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakeInstallUsesSharedGoBinByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Makefile installation integration test requires POSIX make")
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	scratch := t.TempDir()
	firstGopath := filepath.Join(scratch, "gopath with spaces")
	secondGopath := filepath.Join(scratch, "secondary-gopath")
	gopath := strings.Join([]string{firstGopath, secondGopath}, string(os.PathListSeparator))
	privateBin := filepath.Join(scratch, "toolchain-bin")
	buildPath := filepath.Join(scratch, "kwt")
	moduleCacheOutput, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOMODCACHE failed: %v", err)
	}
	moduleCache := strings.TrimSpace(string(moduleCacheOutput))

	runMake := func(args ...string) {
		t.Helper()
		cmd := exec.Command("make", append([]string{
			"-C", repoRoot,
			"BINARY_NAME=" + buildPath,
		}, args...)...)
		cmd.Env = append(os.Environ(),
			"GOPATH="+gopath,
			"GOBIN="+privateBin,
			"GOMODCACHE="+moduleCache,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make %v failed: %v\n%s", args, err, output)
		}
	}

	runMake("install")
	sharedBinary := filepath.Join(firstGopath, "bin", "kwt")
	if _, err := os.Stat(sharedBinary); err != nil {
		t.Fatalf("default install did not create %s: %v", sharedBinary, err)
	}
	if _, err := os.Stat(filepath.Join(secondGopath, "bin", "kwt")); !os.IsNotExist(err) {
		t.Fatalf("default install unexpectedly used the second GOPATH entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(privateBin, "kwt")); !os.IsNotExist(err) {
		t.Fatalf("default install unexpectedly used private GOBIN: %v", err)
	}
	version := exec.Command(sharedBinary, "--version")
	if output, err := version.CombinedOutput(); err != nil || !strings.Contains(string(output), "kwt version") {
		t.Fatalf("installed binary did not run: %v\n%s", err, output)
	}

	override := filepath.Join(scratch, "override-bin")
	runMake("INSTALL_DIR="+override, "install")
	if _, err := os.Stat(filepath.Join(override, "kwt")); err != nil {
		t.Fatalf("INSTALL_DIR override did not create binary: %v", err)
	}
}
