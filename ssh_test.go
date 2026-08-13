package kwt

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	internalssh "go.kenn.io/kwt/internal/ssh"
)

type recordingSSHResolver struct{}

func (recordingSSHResolver) Resolve(
	context.Context,
	SSHResolveRequest,
) (SSHRouteSnapshot, error) {
	return SSHRouteSnapshot{}, nil
}

func TestSSHServiceReloadsConfiguredProtectedEnvironmentPerRequest(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte("[fleet]\ntoken_env = \"TOKEN_ONE\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := []string{
		"TOKEN_ONE=first-secret",
		"TOKEN_TWO=second-secret",
		"SAFE=value",
	}
	var captured []internalssh.ResolverOptions
	service := NewSSHService(SSHServiceOptions{
		Home:        home,
		Environment: func() []string { return append([]string(nil), environment...) },
	})
	service.build = func(options internalssh.ResolverOptions) sshSnapshotResolver {
		captured = append(captured, options)
		return recordingSSHResolver{}
	}

	if _, err := service.Resolve(context.Background(), SSHResolveRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[fleet]\ntoken_env = \"TOKEN_TWO\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resolve(context.Background(), SSHResolveRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured resolver options = %d, want 2", len(captured))
	}
	if slices.Contains(captured[0].Environment, "TOKEN_ONE=first-secret") ||
		!slices.Contains(captured[0].Environment, "TOKEN_TWO=second-secret") {
		t.Fatalf("first resolver environment = %v", captured[0].Environment)
	}
	if slices.Contains(captured[1].Environment, "TOKEN_TWO=second-secret") ||
		!slices.Contains(captured[1].Environment, "TOKEN_ONE=first-secret") {
		t.Fatalf("second resolver environment = %v", captured[1].Environment)
	}
}

func TestSSHServiceUsesRequestEnvironmentAndStripsConfiguredCredentials(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte("[fleet]\ntoken_env = \"GHOSTHUB_AUTH\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var captured internalssh.ResolverOptions
	service := NewSSHService(SSHServiceOptions{
		Home:        home,
		Environment: func() []string { return []string{"PATH=/daemon/bin"} },
	})
	service.build = func(options internalssh.ResolverOptions) sshSnapshotResolver {
		captured = options
		return recordingSSHResolver{}
	}

	_, err := service.Resolve(context.Background(), SSHResolveRequest{
		Environment: []string{
			"PATH=/invocation/bin",
			"SSH_AUTH_SOCK=/invocation/agent.sock",
			"GHOSTHUB_AUTH=fleet-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(captured.Environment, "PATH=/daemon/bin") {
		t.Fatalf("resolver inherited daemon environment: %v", captured.Environment)
	}
	if !slices.Contains(captured.Environment, "PATH=/invocation/bin") ||
		!slices.Contains(captured.Environment, "SSH_AUTH_SOCK=/invocation/agent.sock") {
		t.Fatalf("resolver environment = %v", captured.Environment)
	}
	if slices.Contains(captured.Environment, "GHOSTHUB_AUTH=fleet-secret") {
		t.Fatalf("resolver inherited protected credential: %v", captured.Environment)
	}
}
