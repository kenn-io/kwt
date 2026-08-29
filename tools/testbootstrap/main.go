// Command testbootstrap starts kwt's test harness before the root module or
// its toolchain can inherit ambient credentials and network configuration.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const callerKwtHomeEnvironment = "KWT_TEST_CALLER_KWT_HOME"

var allowedEnvironment = map[string]struct{}{
	"APPDATA":           {},
	"CGO_ENABLED":       {},
	"COMSPEC":           {},
	"GOCACHE":           {},
	"GOARCH":            {},
	"GOMODCACHE":        {},
	"GOPATH":            {},
	"GOOS":              {},
	"GOROOT":            {},
	"HOME":              {},
	"HOMEDRIVE":         {},
	"HOMEPATH":          {},
	"LANG":              {},
	"LC_ALL":            {},
	"LC_CTYPE":          {},
	"LOCALAPPDATA":      {},
	"NIX_SSL_CERT_FILE": {},
	"PATH":              {},
	"PATHEXT":           {},
	"PROGRAMDATA":       {},
	"SSL_CERT_DIR":      {},
	"SSL_CERT_FILE":     {},
	"SYSTEMROOT":        {},
	"TEMP":              {},
	"TMP":               {},
	"TMPDIR":            {},
	"USERPROFILE":       {},
	"WINDIR":            {},
	"XDG_CACHE_HOME":    {},
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) (exitCode int) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: determine working directory: %v\n", err)
		return 1
	}
	repositoryRoot := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	if _, err = os.Stat(filepath.Join(repositoryRoot, "go.mod")); err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: locate repository root: %v\n", err)
		return 1
	}

	scratch, err := os.MkdirTemp("", "kwt-test-bootstrap-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: create scratch directory: %v\n", err)
		return 1
	}
	defer func() {
		if cleanupErr := os.RemoveAll(scratch); cleanupErr != nil {
			_, _ = fmt.Fprintf(stderr, "test bootstrap: remove scratch directory: %v\n", cleanupErr)
			exitCode = 1
		}
	}()
	if err = os.Chmod(scratch, 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: protect scratch directory: %v\n", err)
		return 1
	}
	if err = os.Mkdir(filepath.Join(scratch, "kwt"), 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: create KWT_HOME: %v\n", err)
		return 1
	}
	if err = os.WriteFile(filepath.Join(scratch, "gitconfig"), nil, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "test bootstrap: create Git config: %v\n", err)
		return 1
	}

	commandArgs := append([]string{"run", "./internal/testharness/cmd", "--"}, args...)
	command := exec.CommandContext(ctx, "go", commandArgs...)
	command.Dir = repositoryRoot
	command.Env = bootstrapEnvironment(os.Environ(), scratch, workingDirectory)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err = command.Run(); err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	_, _ = fmt.Fprintf(stderr, "test bootstrap: start test harness: %v\n", err)
	return 1
}

func bootstrapEnvironment(base []string, scratch, workingDirectory string) []string {
	values := make(map[string]string, len(allowedEnvironment))
	callerKwtHome := ""
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		key = strings.ToUpper(key)
		if key == "KWT_HOME" {
			callerKwtHome = value
			if value != "" && !filepath.IsAbs(value) && !strings.HasPrefix(value, "~") {
				callerKwtHome = filepath.Clean(filepath.Join(workingDirectory, value))
			}
		}
		if _, allowed := allowedEnvironment[key]; allowed {
			values[key] = value
		}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys)+12)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return append(result,
		"GOAUTH=off",
		"GOENV=off",
		"GOPROXY=https://proxy.golang.org",
		"GOSUMDB=sum.golang.org",
		"GOTOOLCHAIN=auto",
		"GOVCS=*:off",
		"GIT_CONFIG_GLOBAL="+filepath.Join(scratch, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"KWT_HOME="+filepath.Join(scratch, "kwt"),
		"KWT_TEST_BOOTSTRAP=1",
		callerKwtHomeEnvironment+"="+callerKwtHome,
	)
}
