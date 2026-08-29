// Package testharness runs the Go test suite without ambient kwt, Git, or
// network state.
package testharness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/utils"
)

const minimumGitMinor = 32
const callerKwtHomeEnvironment = "KWT_TEST_CALLER_KWT_HOME"

var gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)

type commandExecutor func(
	ctx context.Context,
	environment []string,
	stdout io.Writer,
	stderr io.Writer,
	name string,
	args ...string,
) (string, error)

type denyProxy struct {
	listener net.Listener
	server   *http.Server
	done     chan error
	mu       sync.Mutex
	requests []string
}

func startDenyProxy() (*denyProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for deny proxy: %w", err)
	}
	proxy := &denyProxy{
		listener: listener,
		done:     make(chan error, 1),
	}
	proxy.server = &http.Server{
		Handler:           http.HandlerFunc(proxy.reject),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	go func() {
		proxy.done <- proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *denyProxy) reject(writer http.ResponseWriter, request *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, request.Method+" "+request.Host)
	p.mu.Unlock()
	writer.WriteHeader(http.StatusBadGateway)
}

func (p *denyProxy) Address() string {
	return p.listener.Addr().String()
}

func (p *denyProxy) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func (p *denyProxy) Close() error {
	closeErr := p.server.Close()
	serveErr := <-p.done
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(closeErr, serveErr)
}

// Run executes go test with isolated kwt, Git, and HTTP state.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	protectedNames, err := protectedEnvironmentNames()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "load kwt credential environment names: %v\n", err)
		return 1
	}
	return runWith(ctx, args, stdout, stderr, os.Environ(), protectedNames, executeCommand)
}

func protectedEnvironmentNames() ([]string, error) {
	snapshot, err := callerGlobalSnapshot()
	if err != nil {
		return nil, err
	}
	if os.Getenv("KWT_TEST_BOOTSTRAP") == "1" && snapshot.Config != nil {
		tokenName := strings.TrimSpace(snapshot.Config.Fleet.TokenEnv)
		if tokenName != "" && environmentContains(os.Environ(), tokenName) {
			return nil, fmt.Errorf(
				"fleet.token_env %q conflicts with the test bootstrap environment; choose a dedicated token variable",
				tokenName,
			)
		}
	}
	return credentials.ProtectedNames(snapshot.Config), nil
}

func callerGlobalSnapshot() (*config.GlobalSnapshot, error) {
	callerHome, bootstrap := os.LookupEnv(callerKwtHomeEnvironment)
	if os.Getenv("KWT_TEST_BOOTSTRAP") != "1" || !bootstrap {
		return config.LoadGlobalSnapshot()
	}
	if callerHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			callerHome = filepath.Join(".", ".config", "kwt")
		} else {
			callerHome = filepath.Join(home, ".config", "kwt")
		}
	} else {
		expanded, err := utils.ExpandPath(callerHome)
		if err != nil {
			return nil, fmt.Errorf("resolve caller KWT_HOME: %w", err)
		}
		callerHome = expanded
	}
	return config.LoadGlobalSnapshotAt(callerHome)
}

func environmentContains(environment []string, name string) bool {
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func runWith(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	environment []string,
	protectedNames []string,
	execute commandExecutor,
) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: testharness [go test flags and packages]")
		return 2
	}

	scratch, err := os.MkdirTemp("", "kwt-test-harness-")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "create test scratch directory: %v\n", err)
		return 1
	}
	cleanup := func() error { return os.RemoveAll(scratch) }
	if err = os.Chmod(scratch, 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "protect test scratch directory: %v\n", err)
		_ = cleanup()
		return 1
	}
	if err = os.Mkdir(filepath.Join(scratch, "kwt"), 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "create isolated KWT_HOME: %v\n", err)
		_ = cleanup()
		return 1
	}
	// gopsutil reads HOST_PROC on Linux. An empty process table keeps removal
	// fixtures independent of unrelated processes on the test host.
	if err = os.Mkdir(filepath.Join(scratch, "proc"), 0o700); err != nil {
		_, _ = fmt.Fprintf(stderr, "create isolated process table: %v\n", err)
		_ = cleanup()
		return 1
	}
	if err = os.WriteFile(filepath.Join(scratch, "gitconfig"), nil, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "create isolated Git config: %v\n", err)
		_ = cleanup()
		return 1
	}

	exitCode := runPrepared(
		ctx,
		args,
		stdout,
		stderr,
		environment,
		protectedNames,
		scratch,
		execute,
	)
	if err = cleanup(); err != nil {
		_, _ = fmt.Fprintf(stderr, "remove test scratch directory: %v\n", err)
		return 1
	}
	return exitCode
}

func runPrepared(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	environment []string,
	protectedNames []string,
	scratch string,
	execute commandExecutor,
) int {
	preparationEnv := preparationEnvironment(environment, protectedNames, scratch)
	versionOutput, err := execute(ctx, preparationEnv, io.Discard, stderr, "git", "version")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "determine Git version: %v\n", err)
		return 1
	}
	if err = requireGitVersion(versionOutput); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if _, err = execute(ctx, preparationEnv, stdout, stderr, "go", "mod", "download"); err != nil {
		_, _ = fmt.Fprintf(stderr, "download Go modules: %v\n", err)
		return 1
	}

	proxy, err := startDenyProxy()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "start outbound deny proxy: %v\n", err)
		return 1
	}
	testEnv := isolatedEnvironment(environment, protectedNames, scratch, proxy.Address())
	_, testErr := execute(ctx, testEnv, stdout, stderr, "go", append([]string{"test"}, args...)...)
	closeErr := proxy.Close()
	requests := proxy.Requests()

	failed := false
	if testErr != nil {
		_, _ = fmt.Fprintf(stderr, "go test: %v\n", testErr)
		failed = true
	}
	if closeErr != nil {
		_, _ = fmt.Fprintf(stderr, "stop outbound deny proxy: %v\n", closeErr)
		failed = true
	}
	if len(requests) > 0 {
		_, _ = fmt.Fprintln(stderr, "tests attempted outbound HTTP requests:")
		for _, request := range requests {
			_, _ = fmt.Fprintf(stderr, "  %s\n", request)
		}
		failed = true
	}
	entries, stateErr := os.ReadDir(filepath.Join(scratch, "kwt"))
	if stateErr != nil {
		_, _ = fmt.Fprintf(stderr, "inspect shared harness KWT_HOME: %v\n", stateErr)
		failed = true
	} else if len(entries) > 0 {
		_, _ = fmt.Fprintln(stderr, "tests used shared harness KWT_HOME; each stateful test must set its own KWT_HOME:")
		for _, entry := range entries {
			_, _ = fmt.Fprintf(stderr, "  %s\n", entry.Name())
		}
		failed = true
	}
	if failed {
		return 1
	}
	return 0
}

func executeCommand(
	ctx context.Context,
	environment []string,
	stdout io.Writer,
	stderr io.Writer,
	name string,
	args ...string,
) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	command.Stderr = stderr
	if name == "git" && len(args) == 1 && args[0] == "version" {
		output, err := command.Output()
		return string(output), err
	}
	command.Stdout = stdout
	return "", command.Run()
}

func requireGitVersion(output string) error {
	trimmed := strings.TrimSpace(output)
	match := gitVersionPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return fmt.Errorf("unexpected Git version output %q", trimmed)
	}

	major, err := strconv.Atoi(match[1])
	if err != nil {
		return fmt.Errorf("parse Git major version %q: %w", match[1], err)
	}
	minor, err := strconv.Atoi(match[2])
	if err != nil {
		return fmt.Errorf("parse Git minor version %q: %w", match[2], err)
	}
	patch := 0
	if match[3] != "" {
		patch, err = strconv.Atoi(match[3])
		if err != nil {
			return fmt.Errorf("parse Git patch version %q: %w", match[3], err)
		}
	}
	if major < 2 || major == 2 && minor < minimumGitMinor {
		return fmt.Errorf(
			"git 2.%d or newer is required; found Git %d.%d.%d",
			minimumGitMinor,
			major,
			minor,
			patch,
		)
	}
	return nil
}

func preparationEnvironment(base, protectedNames []string, scratch string) []string {
	return appendOwnedEnvironment(
		withoutEnvironment(credentials.StripEnvironment(base, protectedNames), func(key string) bool {
			return strings.HasPrefix(strings.ToUpper(key), "GIT_") ||
				strings.EqualFold(key, "KWT_HOME") ||
				strings.EqualFold(key, "KWT_TEST_HARNESS") ||
				strings.EqualFold(key, callerKwtHomeEnvironment)
		}),
		scratch,
		"",
	)
}

func isolatedEnvironment(base, protectedNames []string, scratch, proxyAddress string) []string {
	environment := appendOwnedEnvironment(
		withoutEnvironment(credentials.StripEnvironment(base, protectedNames), func(key string) bool {
			upper := strings.ToUpper(key)
			return strings.HasPrefix(upper, "GIT_") ||
				strings.EqualFold(key, "KWT_HOME") ||
				strings.EqualFold(key, "KWT_TEST_HARNESS") ||
				strings.EqualFold(key, callerKwtHomeEnvironment) ||
				strings.EqualFold(key, "HOST_PROC") ||
				upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" ||
				upper == "ALL_PROXY" || upper == "NO_PROXY"
		}),
		scratch,
		proxyAddress,
	)
	return append(environment, "HOST_PROC="+filepath.Join(scratch, "proc"))
}

func appendOwnedEnvironment(environment []string, scratch, proxyAddress string) []string {
	result := append([]string(nil), environment...)
	result = append(result,
		"KWT_HOME="+filepath.Join(scratch, "kwt"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(scratch, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	if proxyAddress == "" {
		return result
	}
	proxyURL := "http://" + proxyAddress
	return append(result,
		"GIT_ALLOW_PROTOCOL=file",
		"KWT_TEST_HARNESS="+filepath.Join(scratch, "kwt"),
		"HTTP_PROXY="+proxyURL,
		"HTTPS_PROXY="+proxyURL,
		"ALL_PROXY="+proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1",
	)
}

func withoutEnvironment(environment []string, remove func(string) bool) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || remove(key) {
			continue
		}
		result = append(result, entry)
	}
	return result
}
