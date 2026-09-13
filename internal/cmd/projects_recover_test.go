package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

func TestProjectsRecoverRenamedRepository(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   string
		code     service.Code
		exitCode int
	}{
		{name: "automatic", status: "recovered"},
		{name: "chosen", status: "recovered"},
		{name: "missing", status: "unresolved"},
		{name: "ambiguous", status: "unresolved"},
		{name: "wrong repository", code: service.InvalidRequest, exitCode: 2},
		{name: "changed registration", code: service.RegistrationChanged, exitCode: 1},
		{name: "not registered", code: service.ProjectNotFound, exitCode: 2},
		{name: "missing expectations", code: service.InvalidRequest, exitCode: 2},
		{name: "invalid identity", code: service.InvalidRequest, exitCode: 2},
		{name: "different identity", code: service.RegistrationChanged, exitCode: 1},
		{name: "concurrent relocation", code: service.RegistrationChanged, exitCode: 1},
		{name: "concurrent rewrite", code: service.RegistrationChanged, exitCode: 1},
		{name: "edited after relocation", code: service.RegistrationChanged, exitCode: 1},
		{name: "inaccessible registry", status: "unresolved"},
		{name: "inaccessible inventory", status: "unresolved"},
		{name: "invalid registry", code: service.Internal, exitCode: 1},
		{name: "invalid config", code: service.Internal, exitCode: 1},
		{name: "cancelled", code: service.Internal, exitCode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := test.name
			home, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Setenv("KWT_HOME", home)
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			oldPath := newTUITestRepoAt(t, filepath.Join(root, "original"))
			runTUITestGit(t, oldPath, "remote", "add", "origin", "https://github.com/acme/widget.git")
			base := filepath.Join(root, "worktrees")
			newPath := filepath.Join(base, "widget")
			require.NoError(t, os.MkdirAll(base, 0o700))
			require.NoError(t, os.Rename(oldPath, newPath))
			if scenario == "chosen" {
				newPath = filepath.Join(root, "elsewhere")
				require.NoError(t, os.Rename(filepath.Join(base, "widget"), newPath))
			}
			if scenario == "missing" {
				require.NoError(t, os.RemoveAll(newPath))
			}
			if scenario == "ambiguous" {
				other := newTUITestRepoAt(t, filepath.Join(base, "second-clone"))
				runTUITestGit(t, other, "remote", "add", "origin", "https://github.com/acme/widget.git")
			}
			if scenario == "wrong repository" {
				runTUITestGit(t, newPath, "remote", "set-url", "origin", "https://github.com/acme/another.git")
			}
			configPath := filepath.Join(home, "config.toml")
			original := []byte(fmt.Sprintf("[worktree]\nbasedir = %q\n[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'My Project'\npath = %q\nlast_touched = 'before'\ncustom = 'custom-before'\n", base, oldPath))
			require.NoError(t, os.WriteFile(configPath, original, 0o600))
			snapshot, err := config.LoadGlobalSnapshotAt(home)
			require.NoError(t, err)
			fingerprint, err := snapshot.Projects[0].Fingerprint()
			require.NoError(t, err)
			if scenario == "changed registration" {
				original = bytes.ReplaceAll(original, []byte("My Project"), []byte("Changed Project"))
				require.NoError(t, os.WriteFile(configPath, original, 0o600))
			}
			if scenario == "concurrent relocation" || scenario == "concurrent rewrite" || scenario == "edited after relocation" {
				transition := transitionDoctorProjectRegistration
				t.Cleanup(func() { transitionDoctorProjectRegistration = transition })
				transitionDoctorProjectRegistration = func(
					ctx context.Context, home string, expansion kwt.ExpansionContext,
					replacement models.Project, mutation func() error,
				) error {
					if scenario == "edited after relocation" {
						err := transition(ctx, home, expansion, replacement, mutation)
						require.NoError(t, err)
						current, readErr := os.ReadFile(configPath)
						require.NoError(t, readErr)
						original = bytes.ReplaceAll(current, []byte("custom-before"), []byte("custom-after"))
						require.NotEqual(t, current, original)
						require.NoError(t, os.WriteFile(configPath, original, 0o600))
						return nil
					}
					concurrent := replacement
					if scenario == "concurrent rewrite" {
						concurrent.Name = "Another registration"
					}
					// Another writer completes its guarded transition before
					// recovery acquires the transition fence.
					require.NoError(t, transition(ctx, home, expansion, concurrent, func() error {
						changed, swapErr := config.CompareAndSwapProjectAt(home, snapshot.Projects[0], &concurrent)
						require.NoError(t, swapErr)
						require.True(t, changed)
						return nil
					}))
					original, err = os.ReadFile(configPath)
					require.NoError(t, err)
					return transition(ctx, home, expansion, replacement, mutation)
				}
			}
			if scenario == "inaccessible registry" || scenario == "inaccessible inventory" || scenario == "cancelled" {
				blocked := filepath.Join(home, "registry.json")
				require.NoError(t, os.WriteFile(blocked, []byte("[]"), 0o600))
				if scenario == "inaccessible inventory" {
					blocked = base
				}
				require.NoError(t, os.Chmod(blocked, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(blocked, 0o700)) })
				file, openErr := os.Open(blocked)
				if openErr == nil {
					require.NoError(t, file.Close())
					t.Skip("filesystem permits reading a path with mode 000")
				}
				require.ErrorIs(t, openErr, fs.ErrPermission)
			}
			if scenario == "invalid registry" {
				require.NoError(t, os.WriteFile(filepath.Join(home, "registry.json"), []byte("{invalid"), 0o600))
			}
			if scenario == "invalid config" {
				original = []byte("[invalid")
				require.NoError(t, os.WriteFile(configPath, original, 0o600))
			}
			command := newProjectsRecoverCommand()
			command.SetContext(t.Context())
			if scenario == "cancelled" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				command.SetContext(ctx)
			}
			output := new(bytes.Buffer)
			command.SetOut(output)
			command.SetErr(new(bytes.Buffer))
			args := []string{oldPath, "--json", "--expected-repository", "github.com/acme/widget", "--expected-registration", fingerprint}
			if scenario == "not registered" {
				args[0] = filepath.Join(root, "not-registered")
			}
			if scenario == "missing expectations" {
				args = []string{oldPath, "--json"}
			}
			if scenario == "invalid identity" {
				args[3] = "not-an-identity"
			}
			if scenario == "different identity" {
				args[3] = "github.com/acme/another"
			}
			if scenario == "chosen" || scenario == "wrong repository" {
				args = append(args, "--to", newPath)
			}
			require.NoError(t, command.ParseFlags(args))
			err = command.RunE(command, command.Flags().Args())
			if test.code != "" {
				var exitErr interface{ ExitCode() int }
				require.ErrorAs(t, err, &exitErr)
				assert.Equal(t, test.exitCode, exitErr.ExitCode())
				var response jsonErrorEnvelope
				require.NoError(t, json.Unmarshal(output.Bytes(), &response))
				assert.Equal(t, test.code, response.Error.Code)
				assert.Equal(t, test.code == service.RegistrationChanged, response.Error.Retryable)
			} else {
				require.NoError(t, err, output.String())
				var result projectRecoveryResult
				require.NoError(t, json.Unmarshal(output.Bytes(), &result))
				assert.Equal(t, test.status, result.Status)
				if test.status == "unresolved" {
					assert.Equal(t, oldPath, result.Project.Path)
					assert.Equal(t, fingerprint, result.Project.RegistrationFingerprint)
				}
			}
			if scenario == "automatic" || scenario == "chosen" {
				current, err := config.LoadGlobalSnapshotAt(home)
				require.NoError(t, err)
				require.Len(t, current.Projects, 1)
				assert.Equal(t, newPath, current.Projects[0].Persisted.Path)
				assert.Equal(t, "My Project", current.Projects[0].Persisted.Name)
				assert.Equal(t, "before", current.Projects[0].Persisted.LastTouched)
				var result projectRecoveryResult
				require.NoError(t, json.Unmarshal(output.Bytes(), &result))
				assert.Equal(t, "recovered", result.Status)
				assert.Equal(t, newPath, result.Project.Path)
				assert.Empty(t, result.Project.PathIssue)
				assert.NotEqual(t, fingerprint, result.Project.RegistrationFingerprint)
			} else {
				unchanged, err := os.ReadFile(configPath)
				require.NoError(t, err)
				assert.Equal(t, original, unchanged)
			}
		})
	}
}
