package testharness

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRequireGitVersion(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr string
	}{
		{name: "too old", output: "git version 2.31.8", wantErr: "git 2.32 or newer is required; found Git 2.31.8"},
		{name: "minimum", output: "git version 2.32.0"},
		{name: "windows suffix", output: "git version 2.55.0.windows.1"},
		{name: "newer major", output: "git version 3.0.0"},
		{name: "malformed", output: "git unknown", wantErr: "unexpected Git version output"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireGitVersion(tt.output)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("requireGitVersion(%q): %v", tt.output, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("requireGitVersion(%q) error = %v, want containing %q", tt.output, err, tt.wantErr)
			}
		})
	}
}

func TestBootstrapEnvironmentContract(t *testing.T) {
	if os.Getenv("KWT_TEST_BOOTSTRAP") != "1" {
		t.Skip("requires the outer test bootstrap")
	}
	for _, key := range []string{
		"GOPRIVATE", "GONOPROXY", "GONOSUMDB", "MY_TOKEN",
		callerKwtHomeEnvironment,
	} {
		if _, ok := os.LookupEnv(key); ok {
			t.Fatalf("%s reached the test process", key)
		}
	}
	for key, want := range map[string]string{
		"GOAUTH":      "off",
		"GOENV":       "off",
		"GOPROXY":     "https://proxy.golang.org",
		"GOSUMDB":     "sum.golang.org",
		"GOTOOLCHAIN": "auto",
		"GOVCS":       "*:off",
	} {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestIsolatedEnvironmentReplacesGitKWTAndProxyState(t *testing.T) {
	scratch := t.TempDir()
	base := []string{
		"HOME=/developer/home",
		"PATH=/developer/bin",
		"GOCACHE=/developer/go-cache",
		"KWT_HOME=/developer/kwt",
		"GIT_DIR=/developer/repository/.git",
		"Git_Config_Global=/developer/global.gitconfig",
		"HTTP_PROXY=http://old-proxy.example",
		"https_proxy=http://old-secure-proxy.example",
		"ALL_PROXY=socks5://old-proxy.example",
		"no_proxy=old-host.example",
	}

	got := isolatedEnvironment(
		base,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
		scratch,
		"127.0.0.1:43123",
	)

	assertEnvironmentValue(t, got, "HOME", "/developer/home")
	assertEnvironmentValue(t, got, "PATH", "/developer/bin")
	assertEnvironmentValue(t, got, "GOCACHE", "/developer/go-cache")
	assertEnvironmentValue(t, got, "KWT_HOME", filepath.Join(scratch, "kwt"))
	assertEnvironmentValue(t, got, "KWT_TEST_HARNESS", filepath.Join(scratch, "kwt"))
	assertEnvironmentValue(t, got, "GIT_CONFIG_GLOBAL", filepath.Join(scratch, "gitconfig"))
	assertEnvironmentValue(t, got, "GIT_CONFIG_NOSYSTEM", "1")
	assertEnvironmentValue(t, got, "GIT_TERMINAL_PROMPT", "0")
	assertEnvironmentValue(t, got, "GIT_ALLOW_PROTOCOL", "file")
	assertEnvironmentValue(t, got, "HTTP_PROXY", "http://127.0.0.1:43123")
	assertEnvironmentValue(t, got, "HTTPS_PROXY", "http://127.0.0.1:43123")
	assertEnvironmentValue(t, got, "ALL_PROXY", "http://127.0.0.1:43123")
	assertEnvironmentValue(t, got, "NO_PROXY", "localhost,127.0.0.1,::1")
	assertEnvironmentMissing(t, got, "GIT_DIR")
	assertUniqueEnvironmentKeys(t, got)
}

func TestPreparationEnvironmentKeepsCallerProxyAndOmitsTransportLimit(t *testing.T) {
	scratch := t.TempDir()
	base := []string{
		"HOME=/developer/home",
		"HTTP_PROXY=http://module-proxy.example",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_WORK_TREE=/developer/repository",
	}

	got := preparationEnvironment(
		base,
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
		scratch,
	)

	assertEnvironmentValue(t, got, "HOME", "/developer/home")
	assertEnvironmentValue(t, got, "HTTP_PROXY", "http://module-proxy.example")
	assertEnvironmentValue(t, got, "KWT_HOME", filepath.Join(scratch, "kwt"))
	assertEnvironmentValue(t, got, "GIT_CONFIG_GLOBAL", filepath.Join(scratch, "gitconfig"))
	assertEnvironmentValue(t, got, "GIT_CONFIG_NOSYSTEM", "1")
	assertEnvironmentValue(t, got, "GIT_TERMINAL_PROMPT", "0")
	assertEnvironmentMissing(t, got, "GIT_ALLOW_PROTOCOL")
	assertEnvironmentMissing(t, got, "GIT_WORK_TREE")
}

func TestProtectedEnvironmentNamesIncludeConfiguredFleetToken(t *testing.T) {
	kwtHome := t.TempDir()
	t.Setenv("KWT_HOME", kwtHome)
	if err := os.WriteFile(
		filepath.Join(kwtHome, "config.toml"),
		[]byte("[fleet]\ntoken_env = 'CUSTOM_FLEET_TOKEN'\n"),
		0o600,
	); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	got, err := protectedEnvironmentNames()
	if err != nil {
		t.Fatalf("protectedEnvironmentNames: %v", err)
	}
	want := "KWT_GITHUB_TOKEN\nKWT_FLEET_TOKEN\nCUSTOM_FLEET_TOKEN"
	if strings.Join(got, "\n") != want {
		t.Fatalf("protected environment names = %q, want %q", got, want)
	}
}

func TestDenyProxyRecordsAndRejectsHTTPAndConnect(t *testing.T) {
	proxy, err := startDenyProxy()
	if err != nil {
		t.Fatalf("startDenyProxy: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := proxy.Close(); closeErr != nil {
			t.Errorf("close deny proxy: %v", closeErr)
		}
	})

	proxyURL, err := url.Parse("http://" + proxy.Address())
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	response, err := client.Get("http://external.example/path")
	if err != nil {
		t.Fatalf("GET through deny proxy: %v", err)
	}
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatalf("close GET response: %v", closeErr)
	}

	connection, err := net.Dial("tcp", proxy.Address())
	if err != nil {
		t.Fatalf("connect to deny proxy: %v", err)
	}
	if _, err = connection.Write([]byte("CONNECT secure.example:443 HTTP/1.1\r\nHost: secure.example:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}
	connectResponse, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if connectResponse.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want %d", connectResponse.StatusCode, http.StatusBadGateway)
	}
	if closeErr := connectResponse.Body.Close(); closeErr != nil {
		t.Fatalf("close CONNECT response: %v", closeErr)
	}
	if closeErr := connection.Close(); closeErr != nil {
		t.Fatalf("close CONNECT connection: %v", closeErr)
	}

	want := []string{"GET external.example", "CONNECT secure.example:443"}
	got := proxy.Requests()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

func TestRunRejectsGitOlderThan232BeforeGoCommands(t *testing.T) {
	var commands []string
	execute := func(
		_ context.Context,
		_ []string,
		_, _ io.Writer,
		name string,
		args ...string,
	) (string, error) {
		commands = append(commands, strings.Join(append([]string{name}, args...), " "))
		return "git version 2.31.8", nil
	}
	var stderr bytes.Buffer

	exitCode := runWith(
		context.Background(),
		[]string{"./..."},
		io.Discard,
		&stderr,
		[]string{"HOME=/developer/home"},
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
		execute,
	)

	if exitCode == 0 {
		t.Fatal("runWith exit code = 0, want failure")
	}
	if got, want := strings.Join(commands, "\n"), "git version"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "git 2.32 or newer is required") {
		t.Fatalf("stderr = %q, want Git version requirement", stderr.String())
	}
}

func TestRunDoesNotForwardProtectedEnvironment(t *testing.T) {
	protectedNames := []string{
		"KWT_GITHUB_TOKEN",
		"KWT_FLEET_TOKEN",
		"CUSTOM_FLEET_TOKEN",
	}
	var commands []string
	execute := func(
		_ context.Context,
		environment []string,
		_, _ io.Writer,
		name string,
		args ...string,
	) (string, error) {
		for _, protectedName := range protectedNames {
			assertEnvironmentMissing(t, environment, protectedName)
		}
		command := strings.Join(append([]string{name}, args...), " ")
		commands = append(commands, command)
		if command == "git version" {
			return "git version 2.55.0", nil
		}
		return "", nil
	}
	var stderr bytes.Buffer

	exitCode := runWith(
		context.Background(),
		[]string{"./internal/example"},
		io.Discard,
		&stderr,
		[]string{
			"HOME=/developer/home",
			"KWT_GITHUB_TOKEN=github-secret",
			"kwt_fleet_token=fleet-secret",
			"Custom_Fleet_Token=custom-secret",
		},
		protectedNames,
		execute,
	)

	if exitCode != 0 {
		t.Fatalf("runWith exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got, want := strings.Join(commands, "\n"), "git version\ngo mod download\ngo test ./internal/example"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestRunFailsAfterSuccessfulTestsAttemptOutboundHTTP(t *testing.T) {
	var commands []string
	execute := func(
		_ context.Context,
		environment []string,
		_, _ io.Writer,
		name string,
		args ...string,
	) (string, error) {
		command := strings.Join(append([]string{name}, args...), " ")
		commands = append(commands, command)
		switch command {
		case "git version":
			return "git version 2.55.0", nil
		case "go mod download":
			return "", nil
		case "go test ./internal/example":
			proxyValue := environmentValue(t, environment, "HTTP_PROXY")
			proxyURL, err := url.Parse(proxyValue)
			if err != nil {
				return "", err
			}
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
			response, err := client.Get("http://missed-fixture.example/path")
			if err != nil {
				return "", err
			}
			return "", response.Body.Close()
		default:
			t.Fatalf("unexpected command %q", command)
			return "", nil
		}
	}
	var stderr bytes.Buffer

	exitCode := runWith(
		context.Background(),
		[]string{"./internal/example"},
		io.Discard,
		&stderr,
		[]string{"HOME=/developer/home"},
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
		execute,
	)

	if exitCode == 0 {
		t.Fatal("runWith exit code = 0, want failure")
	}
	if got, want := strings.Join(commands, "\n"), "git version\ngo mod download\ngo test ./internal/example"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "GET missed-fixture.example") {
		t.Fatalf("stderr = %q, want recorded outbound request", stderr.String())
	}
}

func TestRunFailsWhenTestsUseSharedHarnessKwtHome(t *testing.T) {
	execute := func(
		_ context.Context,
		environment []string,
		_, _ io.Writer,
		name string,
		args ...string,
	) (string, error) {
		command := strings.Join(append([]string{name}, args...), " ")
		switch command {
		case "git version":
			return "git version 2.55.0", nil
		case "go mod download":
			return "", nil
		case "go test ./internal/example":
			kwtHome := environmentValue(t, environment, "KWT_HOME")
			return "", os.WriteFile(filepath.Join(kwtHome, "registry.json"), []byte("{}"), 0o600)
		default:
			t.Fatalf("unexpected command %q", command)
			return "", nil
		}
	}
	var stderr bytes.Buffer

	exitCode := runWith(
		context.Background(),
		[]string{"./internal/example"},
		io.Discard,
		&stderr,
		[]string{"HOME=/developer/home"},
		[]string{"KWT_GITHUB_TOKEN", "KWT_FLEET_TOKEN"},
		execute,
	)

	if exitCode == 0 {
		t.Fatal("runWith exit code = 0, want failure")
	}
	if !strings.Contains(stderr.String(), "tests used shared harness KWT_HOME") ||
		!strings.Contains(stderr.String(), "registry.json") {
		t.Fatalf("stderr = %q, want shared KWT_HOME failure", stderr.String())
	}
}

func assertEnvironmentValue(t *testing.T, environment []string, key, want string) {
	t.Helper()
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			if value != want {
				t.Fatalf("%s = %q, want %q", key, value, want)
			}
			return
		}
	}
	t.Fatalf("%s is missing from environment", key)
}

func environmentValue(t *testing.T, environment []string, key string) string {
	t.Helper()
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			return value
		}
	}
	t.Fatalf("%s is missing from environment", key)
	return ""
}

func assertEnvironmentMissing(t *testing.T, environment []string, key string) {
	t.Helper()
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			t.Fatalf("%s unexpectedly remains in environment", key)
		}
	}
}

func assertUniqueEnvironmentKeys(t *testing.T, environment []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if _, ok := seen[key]; ok {
			t.Fatalf("environment key %q appears more than once", key)
		}
		seen[key] = struct{}{}
	}
}
