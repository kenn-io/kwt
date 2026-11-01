package command

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStandardExecutor_Execute(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	tests := []struct {
		name      string
		command   string
		args      []string
		wantError bool
	}{
		{
			name:      "successful command",
			command:   helperCommandName(),
			args:      helperCommandArgs("echo", "hello"),
			wantError: false,
		},
		{
			name:      "command with multiple args",
			command:   helperCommandName(),
			args:      helperCommandArgs("echo", "hello", "world"),
			wantError: false,
		},
		{
			name:      "non-existent command",
			command:   "nonexistentcommand123",
			args:      []string{},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := executor.Execute(ctx, tt.command, tt.args...)
			if (err != nil) != tt.wantError {
				t.Errorf("Execute() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestStandardExecutor_ExecuteWithOutput(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	tests := []struct {
		name         string
		command      string
		args         []string
		wantContains string
		wantError    bool
	}{
		{
			name:         "echo command",
			command:      helperCommandName(),
			args:         helperCommandArgs("echo", "hello world"),
			wantContains: "hello world",
			wantError:    false,
		},
		{
			name:         "date command",
			command:      helperCommandName(),
			args:         helperCommandArgs("date"),
			wantContains: "20", // Should contain year starting with 20
			wantError:    false,
		},
		{
			name:      "failing command",
			command:   helperCommandName(),
			args:      helperCommandArgs("false"),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executor.ExecuteWithOutput(ctx, tt.command, tt.args...)
			if (err != nil) != tt.wantError {
				t.Errorf("ExecuteWithOutput() error = %v, wantError %v", err, tt.wantError)
				return
			}
			if !tt.wantError && !strings.Contains(output, tt.wantContains) {
				t.Errorf("ExecuteWithOutput() output = %v, want to contain %v", output, tt.wantContains)
			}
		})
	}
}

func TestStandardExecutor_ExecuteInDir(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	// Create a temporary directory for testing
	tmpDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "missing")
	command, args := helperCommand("pwd")

	tests := []struct {
		name      string
		dir       string
		command   string
		args      []string
		wantError bool
	}{
		{
			name:      "execute in valid directory",
			dir:       tmpDir,
			command:   command,
			args:      args,
			wantError: false,
		},
		{
			name:      "execute in non-existent directory",
			dir:       missingDir,
			command:   command,
			args:      args,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := executor.ExecuteInDir(ctx, tt.dir, tt.command, tt.args...)
			if (err != nil) != tt.wantError {
				t.Errorf("ExecuteInDir() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestStandardExecutor_ExecuteInDirWithOutput(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	command, args := helperCommand("pwd")
	output, err := executor.ExecuteInDirWithOutput(ctx, tmpDir, command, args...)
	if err != nil {
		t.Fatalf("ExecuteInDirWithOutput() error = %v", err)
	}

	// The output should contain the temp directory path
	assertOutputContainsDir(t, output, tmpDir)
}

func TestStandardExecutor_ExecuteWithStreams(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	// Test with custom streams
	stdin := strings.NewReader("test input")
	var stdout, stderr bytes.Buffer

	command, args := helperCommand("cat")
	err := executor.ExecuteWithStreams(ctx, stdin, &stdout, &stderr, command, args...)
	if err != nil {
		t.Fatalf("ExecuteWithStreams() error = %v", err)
	}

	if stdout.String() != "test input" {
		t.Errorf("ExecuteWithStreams() stdout = %v, want %v", stdout.String(), "test input")
	}
}

func TestStandardExecutor_ExecuteWithEnv(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	// Set a custom environment variable
	env := []string{"TEST_VAR=test_value"}

	command, args := helperCommand("env", "TEST_VAR", "test_value")
	err := executor.ExecuteWithEnv(ctx, env, command, args...)
	if err != nil {
		t.Errorf("ExecuteWithEnv() error = %v", err)
	}
}

func TestStandardExecutor_ExecuteWithEnvInDir(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	tmpDir := t.TempDir()
	env := []string{"TEST_VAR=test_value"}

	command, args := helperCommand("env", "TEST_VAR", "test_value")
	err := executor.ExecuteWithEnvInDir(ctx, env, tmpDir, command, args...)
	if err != nil {
		t.Errorf("ExecuteWithEnvInDir() error = %v", err)
	}
}

func TestStandardExecutor_ExecuteWithOptions(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	tmpDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	opts := &CommandOptions{
		WorkingDir:  tmpDir,
		Environment: []string{"TEST_VAR=test_value"},
		Stdin:       strings.NewReader(""),
		Stdout:      &stdout,
		Stderr:      &stderr,
	}

	command, args := helperCommand("pwd")
	err := executor.ExecuteWithOptions(ctx, command, args, opts)
	if err != nil {
		t.Errorf("ExecuteWithOptions() error = %v", err)
	}

	assertOutputContainsDir(t, stdout.String(), tmpDir)
}

func TestStandardExecutor_ExecuteWithOptionsAndOutput(t *testing.T) {
	executor := NewStandardExecutor()
	ctx := t.Context()

	tmpDir := t.TempDir()

	opts := &CommandOptions{
		WorkingDir:  tmpDir,
		Environment: []string{"TEST_VAR=test_value"},
	}

	command, args := helperCommand("pwd")
	output, err := executor.ExecuteWithOptionsAndOutput(ctx, command, args, opts)
	if err != nil {
		t.Errorf("ExecuteWithOptionsAndOutput() error = %v", err)
	}

	assertOutputContainsDir(t, output, tmpDir)
}

func TestStandardExecutor_CancelledContext(t *testing.T) {
	executor := NewStandardExecutor()
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel immediately

	command, args := helperCommand("sleep", "1s")
	err := executor.Execute(ctx, command, args...)
	if err == nil {
		t.Error("Execute() should fail with cancelled context")
	}
}

// Mock executor for testing interface compliance
type MockExecutor struct {
	executeFunc func(ctx context.Context, name string, args ...string) error
	outputFunc  func(ctx context.Context, name string, args ...string) (string, error)
}

func (m *MockExecutor) Execute(ctx context.Context, name string, args ...string) error {
	if m.executeFunc != nil {
		return m.executeFunc(ctx, name, args...)
	}
	return nil
}

func (m *MockExecutor) ExecuteWithOutput(ctx context.Context, name string, args ...string) (string, error) {
	if m.outputFunc != nil {
		return m.outputFunc(ctx, name, args...)
	}
	return "mock output", nil
}

func (m *MockExecutor) ExecuteInDir(ctx context.Context, dir, name string, args ...string) error {
	return m.Execute(ctx, name, args...)
}

func (m *MockExecutor) ExecuteInDirWithOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	return m.ExecuteWithOutput(ctx, name, args...)
}

func (m *MockExecutor) ExecuteWithStreams(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	return m.Execute(ctx, name, args...)
}

func (m *MockExecutor) ExecuteWithEnv(ctx context.Context, env []string, name string, args ...string) error {
	return m.Execute(ctx, name, args...)
}

func (m *MockExecutor) ExecuteWithEnvInDir(ctx context.Context, env []string, dir, name string, args ...string) error {
	return m.Execute(ctx, name, args...)
}

func TestCommandExecutorInterface(t *testing.T) {
	// Test that MockExecutor implements CommandExecutor interface
	var executor CommandExecutor = &MockExecutor{}
	ctx := t.Context()

	err := executor.Execute(ctx, "test")
	if err != nil {
		t.Errorf("Interface implementation failed: %v", err)
	}

	output, err := executor.ExecuteWithOutput(ctx, "test")
	if err != nil {
		t.Errorf("Interface implementation failed: %v", err)
	}
	if output != "mock output" {
		t.Errorf("Expected 'mock output', got %v", output)
	}
}

func TestAdvancedCommandExecutorInterface(t *testing.T) {
	// Test that StandardExecutor implements AdvancedCommandExecutor interface
	var executor AdvancedCommandExecutor = NewStandardExecutor()
	ctx := t.Context()

	opts := &CommandOptions{
		WorkingDir: os.TempDir(),
	}

	command, args := helperCommand("echo", "test")
	err := executor.ExecuteWithOptions(ctx, command, args, opts)
	if err != nil {
		t.Errorf("AdvancedCommandExecutor implementation failed: %v", err)
	}

	output, err := executor.ExecuteWithOptionsAndOutput(ctx, command, args, opts)
	if err != nil {
		t.Errorf("AdvancedCommandExecutor implementation failed: %v", err)
	}
	if !strings.Contains(output, "test") {
		t.Errorf("Expected output to contain 'test', got %v", output)
	}
}

func helperCommand(command string, args ...string) (string, []string) {
	return helperCommandName(), helperCommandArgs(command, args...)
}

func helperCommandName() string {
	return os.Args[0]
}

func helperCommandArgs(command string, args ...string) []string {
	helperArgs := []string{"-test.run=TestHelperProcess", "--", command}
	return append(helperArgs, args...)
}

func TestHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator == -1 || separator+1 >= len(os.Args) {
		return
	}

	command := os.Args[separator+1]
	args := os.Args[separator+2:]
	switch command {
	case "echo":
		_, _ = fmt.Fprintln(os.Stdout, strings.Join(args, " "))
	case "date":
		_, _ = fmt.Fprintln(os.Stdout, time.Now().Format("2006"))
	case "false":
		os.Exit(1)
	case "pwd":
		wd, err := os.Getwd()
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = fmt.Fprintln(os.Stdout, wd)
	case "cat":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "env":
		if len(args) != 2 || os.Getenv(args[0]) != args[1] {
			_, _ = fmt.Fprintf(os.Stderr, "%s=%q\n", args[0], os.Getenv(args[0]))
			os.Exit(1)
		}
	case "sleep":
		duration := time.Second
		if len(args) > 0 {
			parsed, err := time.ParseDuration(args[0])
			if err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			duration = parsed
		}
		time.Sleep(duration)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown helper command %q\n", command)
		os.Exit(1)
	}
	os.Exit(0)
}

func assertOutputContainsDir(t *testing.T, output string, dir string) {
	t.Helper()

	got := filepath.Clean(strings.TrimSpace(output))
	want := filepath.Clean(dir)
	if !strings.Contains(got, want) {
		t.Errorf("output = %q, want to contain %q", output, dir)
	}
}
