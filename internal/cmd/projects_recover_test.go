package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/internal/config"
)

func TestProjectsRecoverRenamedRepository(t *testing.T) {
	for _, scenario := range []string{"automatic", "chosen", "missing", "ambiguous", "wrong repository", "changed registration"} {
		t.Run(scenario, func(t *testing.T) {
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
			original := []byte(fmt.Sprintf("[worktree]\nbasedir = %q\n[[projects]]\nrepository = 'github.com/acme/widget'\nname = 'My Project'\npath = %q\nlast_touched = 'before'\n", base, oldPath))
			require.NoError(t, os.WriteFile(configPath, original, 0o600))
			snapshot, err := config.LoadGlobalSnapshotAt(home)
			require.NoError(t, err)
			fingerprint, err := snapshot.Projects[0].Fingerprint()
			require.NoError(t, err)
			if scenario == "changed registration" {
				original = bytes.ReplaceAll(original, []byte("My Project"), []byte("Changed Project"))
				require.NoError(t, os.WriteFile(configPath, original, 0o600))
			}
			command := newProjectsRecoverCommand()
			command.SetContext(t.Context())
			output := new(bytes.Buffer)
			command.SetOut(output)
			command.SetErr(output)
			args := []string{oldPath, "--json", "--expected-repository", "github.com/acme/widget", "--expected-registration", fingerprint}
			if scenario == "chosen" || scenario == "wrong repository" {
				args = append(args, "--to", newPath)
			}
			require.NoError(t, command.ParseFlags(args))
			err = command.RunE(command, command.Flags().Args())
			if scenario == "wrong repository" || scenario == "changed registration" {
				require.Error(t, err)
			} else {
				require.NoError(t, err, output.String())
			}
			current, err := config.LoadGlobalSnapshotAt(home)
			require.NoError(t, err)
			require.Len(t, current.Projects, 1)
			if scenario == "automatic" || scenario == "chosen" {
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
