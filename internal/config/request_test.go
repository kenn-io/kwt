package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requestConfigFixture(t *testing.T) (home, repo, localPath string) {
	t.Helper()
	home = t.TempDir()
	repo = t.TempDir()
	t.Setenv("KWT_HOME", home)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, "config.toml"),
		[]byte(""),
		0o600,
	))
	localPath = filepath.Join(repo, ".kwt.toml")
	require.NoError(t, os.WriteFile(
		localPath,
		[]byte("[naming]\ntemplate = \"trusted/{{.Branch}}\"\n"),
		0o600,
	))
	localPath, err := filepath.EvalSymlinks(localPath)
	require.NoError(t, err)
	return home, repo, localPath
}

func TestResolveWorkingDirectoryRequiresTrust(t *testing.T) {
	home, repo, localPath := requestConfigFixture(t)

	_, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})

	var required *TrustRequiredError
	require.ErrorAs(t, err, &required)
	assert.Equal(t, localPath, required.Path)
	assert.Len(t, required.Digest, 64)
	assert.Equal(t, len("[naming]\ntemplate = \"trusted/{{.Branch}}\"\n"), required.Size)
	assert.Contains(t, required.Preview, "trusted/{{.Branch}}")
}

func TestResolveWorkingDirectoryIgnoresUntrustedForOneRequest(t *testing.T) {
	home, repo, localPath := requestConfigFixture(t)

	result, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  IgnoreUntrusted,
	})

	require.NoError(t, err)
	assert.Equal(t, DefaultNamingTemplate, result.Config.Naming.Template)
	assert.Equal(t, []ConfigNote{{
		Code: "untrusted_config_skipped",
		Path: localPath,
	}}, result.Notes)

	_, err = ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})
	var required *TrustRequiredError
	assert.ErrorAs(t, err, &required)
}

func TestResolveWorkingDirectoryIgnoresUnavailableTrustStore(t *testing.T) {
	home, repo, localPath := requestConfigFixture(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, trustStoreFilename), 0o700))

	result, err := ResolveWorkingDirectory(ResolveRequest{
		Home: home, WorkingDirectory: repo, UntrustedPolicy: IgnoreUntrusted,
	})

	require.NoError(t, err)
	assert.Equal(t, DefaultNamingTemplate, result.Config.Naming.Template)
	require.Len(t, result.Notes, 2)
	assert.Equal(t, "trust_store_unavailable", result.Notes[0].Code)
	assert.Equal(t, "untrusted_config_skipped", result.Notes[1].Code)
	assert.Equal(t, localPath, result.Notes[1].Path)
}

func TestResolveWorkingDirectorySkipsUnsafeLocalConfig(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Mkdir(path, 0o700))
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation requires privileges on Windows")
				}
				target := filepath.Join(t.TempDir(), "target.toml")
				require.NoError(t, os.WriteFile(target, []byte("[naming]\ntemplate = 'unsafe'\n"), 0o600))
				require.NoError(t, os.Symlink(target, path))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, repo := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(
				filepath.Join(home, "config.toml"),
				[]byte("[naming]\ntemplate = 'global/{{.Branch}}'\n"),
				0o600,
			))
			path := filepath.Join(repo, ".kwt.toml")
			test.setup(t, path)
			canonicalRepo, err := filepath.EvalSymlinks(repo)
			require.NoError(t, err)

			result, err := ResolveWorkingDirectory(ResolveRequest{
				Home: home, WorkingDirectory: repo, UntrustedPolicy: RequireInteraction,
			})

			require.NoError(t, err)
			assert.Equal(t, "global/{{.Branch}}", result.Config.Naming.Template)
			assert.Equal(t, []ConfigNote{{
				Code: "unsafe_config_skipped", Path: filepath.Join(canonicalRepo, ".kwt.toml"),
			}}, result.Notes)
		})
	}
}

func TestApproveWorkingDirectoryRevalidatesDigest(t *testing.T) {
	home, repo, localPath := requestConfigFixture(t)
	_, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})
	var required *TrustRequiredError
	require.ErrorAs(t, err, &required)

	require.NoError(t, os.WriteFile(
		localPath,
		[]byte("[naming]\ntemplate = \"changed/{{.Branch}}\"\n"),
		0o600,
	))
	err = ApproveWorkingDirectory(Approval{
		Home: home, Path: required.Path, Digest: required.Digest,
	})
	assert.ErrorIs(t, err, ErrConfigChanged)
}

func TestApproveWorkingDirectoryLoadsTrustedConfig(t *testing.T) {
	home, repo, _ := requestConfigFixture(t)
	_, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})
	var required *TrustRequiredError
	require.ErrorAs(t, err, &required)
	require.NoError(t, ApproveWorkingDirectory(Approval{
		Home: home, Path: required.Path, Digest: required.Digest,
	}))

	result, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})

	require.NoError(t, err)
	assert.Equal(t, "trusted/{{.Branch}}", result.Config.Naming.Template)
}

func TestTrustRequirementBoundsAndSanitizesPreview(t *testing.T) {
	home, repo, localPath := requestConfigFixture(t)
	content := "\x1b[31m" + strings.Repeat("x", promptPreviewLimit+100)
	require.NoError(t, os.WriteFile(localPath, []byte(content), 0o600))

	_, err := ResolveWorkingDirectory(ResolveRequest{
		Home:             home,
		WorkingDirectory: repo,
		UntrustedPolicy:  RequireInteraction,
	})

	var required *TrustRequiredError
	require.True(t, errors.As(err, &required))
	assert.True(t, required.Truncated)
	assert.NotContains(t, required.Preview, "\x1b")
	assert.Contains(t, required.Preview, `\x1b`)
}
