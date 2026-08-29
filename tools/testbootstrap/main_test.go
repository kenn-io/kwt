package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapEnvironmentUsesStrictAllowlist(t *testing.T) {
	scratch := t.TempDir()
	got := bootstrapEnvironment([]string{
		"PATH=/tools",
		"TEMP=/tmp",
		`SYSTEMROOT=C:\Windows`,
		"HTTP_PROXY=http://ambient",
		"https_proxy=http://ambient-secure",
		"ALL_PROXY=socks5://ambient",
		"NO_PROXY=ambient.example",
		"GIT_CONFIG_GLOBAL=/ambient/gitconfig",
		"GIT_ASKPASS=/ambient/askpass",
		"KWT_HOME=/ambient/kwt",
		"KWT_GITHUB_TOKEN=github-secret",
		"CUSTOM_FLEET_TOKEN=fleet-secret",
		"GOAUTH=netrc",
		"GOENV=/ambient/goenv",
		"GOPRIVATE=private.example",
		"GONOPROXY=private.example",
		"GONOSUMDB=private.example",
		"GOPROXY=http://ambient-proxy",
		"GOTOOLCHAIN=local",
		"MY_TOKEN=secret",
	}, scratch, "/repository/tools/testbootstrap")

	values := environmentMap(got)
	for key, want := range map[string]string{
		"PATH":                   "/tools",
		"TEMP":                   "/tmp",
		"SYSTEMROOT":             `C:\Windows`,
		"GOAUTH":                 "off",
		"GOENV":                  "off",
		"GOPROXY":                "https://proxy.golang.org",
		"GOSUMDB":                "sum.golang.org",
		"GOTOOLCHAIN":            "auto",
		"GOVCS":                  "*:off",
		"GIT_CONFIG_GLOBAL":      filepath.Join(scratch, "gitconfig"),
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_TERMINAL_PROMPT":    "0",
		"KWT_HOME":               filepath.Join(scratch, "kwt"),
		"KWT_TEST_BOOTSTRAP":     "1",
		callerKwtHomeEnvironment: "/ambient/kwt",
	} {
		if gotValue := values[key]; gotValue != want {
			t.Errorf("%s = %q, want %q", key, gotValue, want)
		}
	}
	for _, key := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"GIT_ASKPASS", "KWT_GITHUB_TOKEN", "CUSTOM_FLEET_TOKEN",
		"GOPRIVATE", "GONOPROXY", "GONOSUMDB", "MY_TOKEN",
	} {
		if _, ok := values[key]; ok {
			t.Errorf("%s unexpectedly remains in the bootstrap environment", key)
		}
	}
	if len(values) != len(got) {
		t.Fatalf("bootstrap environment contains duplicate case-insensitive keys: %q", got)
	}
}

func TestBootstrapEnvironmentResolvesRelativeCallerKwtHome(t *testing.T) {
	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)

	values := environmentMap(bootstrapEnvironment(
		[]string{"KWT_HOME=config/kwt"},
		t.TempDir(),
		workingDirectory,
	))

	want := filepath.Join(workingDirectory, "config", "kwt")
	if got := values[callerKwtHomeEnvironment]; got != want {
		t.Fatalf("%s = %q, want %q", callerKwtHomeEnvironment, got, want)
	}
}

func TestBootstrapTestsDoNotReceiveAmbientCredentials(t *testing.T) {
	for _, key := range []string{
		"KWT_HOME",
		"KWT_GITHUB_TOKEN",
		"KWT_FLEET_TOKEN",
		"MY_TOKEN",
		"GOPRIVATE",
	} {
		if _, ok := os.LookupEnv(key); ok {
			t.Errorf("%s reached the bootstrap test process", key)
		}
	}
}

func TestBootstrapRemovesAmbientVariablesEndToEnd(t *testing.T) {
	command := exec.Command(
		"go", "run", ".", "--",
		"-run", "^TestBootstrapEnvironmentContract$",
		"./internal/testharness",
	)
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"GOAUTH":      "off",
		"GOENV":       "off",
		"GONOPROXY":   "private.example",
		"GONOSUMDB":   "private.example",
		"GOPRIVATE":   "private.example",
		"GOPROXY":     "off",
		"GOSUMDB":     "off",
		"GOTOOLCHAIN": "local",
		"MY_TOKEN":    "secret",
	})
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap integration: %v\n%s", err, output)
	}
}

func TestBootstrapRejectsConfiguredTokenCollisions(t *testing.T) {
	tests := []struct {
		name  string
		value func(*testing.T) string
	}{
		{name: "LANG", value: func(*testing.T) string { return "fleet-secret" }},
		{name: "PATH", value: func(*testing.T) string { return os.Getenv("PATH") }},
		{name: "HOME", value: func(*testing.T) string { return os.Getenv("HOME") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kwtHome := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(kwtHome, "config.toml"),
				[]byte(fmt.Sprintf("[fleet]\ntoken_env = %q\n", tt.name)),
				0o600,
			); err != nil {
				t.Fatalf("write global config: %v", err)
			}
			command := exec.Command(
				"go", "run", ".", "--",
				"-run", "^TestBootstrapEnvironmentContract$",
				"./internal/testharness",
			)
			command.Env = replaceEnvironment(os.Environ(), map[string]string{
				"GOAUTH":      "off",
				"GOENV":       "off",
				"GOPROXY":     "off",
				"GOSUMDB":     "off",
				"GOTOOLCHAIN": "local",
				"KWT_HOME":    kwtHome,
				tt.name:       tt.value(t),
			})
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("bootstrap accepted fleet.token_env collision %q:\n%s", tt.name, output)
			}
			want := fmt.Sprintf("fleet.token_env %q conflicts with the test bootstrap environment", tt.name)
			if !strings.Contains(string(output), want) {
				t.Fatalf("bootstrap output = %q, want %q", output, want)
			}
		})
	}
}

func TestBootstrapForwardsFailure(t *testing.T) {
	command := exec.Command("go", "run", ".")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("bootstrap returned success without test arguments:\n%s", output)
	}
	if !strings.Contains(string(output), "usage: testharness") {
		t.Fatalf("bootstrap failure output = %q, want harness usage", output)
	}
}

func environmentMap(entries []string) map[string]string {
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			result[strings.ToUpper(key)] = value
		}
	}
	return result
}

func replaceEnvironment(base []string, replacements map[string]string) []string {
	result := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replace := replacements[strings.ToUpper(key)]; replace {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}
