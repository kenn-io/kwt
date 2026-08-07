package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestLoadGlobalSnapshotPairsPersistedAndEffectiveProjects(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	raw := `[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = "~/code/widget"
last_touched = "before"
`
	require.NoError(t, os.WriteFile(
		filepath.Join(configHome, "config.toml"), []byte(raw), 0o600,
	))

	snapshot, err := LoadGlobalSnapshot()

	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	assert.Equal(t, "~/code/widget", snapshot.Projects[0].Persisted.Path)
	assert.Equal(t,
		filepath.Join(testHome, "code", "widget"),
		snapshot.Projects[0].Effective.Path,
	)
	assert.Equal(t, "~/code/widget", snapshot.Projects[0].Persisted.Path,
		"expanding the effective config must not mutate the persisted slice")
	assert.Equal(t, snapshot.Projects[0].Effective, snapshot.Config.Projects[0])
}

func TestLoadGlobalSnapshotKeepsMissingSymlinkPathSpellingEffective(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("symbolic links are not supported: %v", err)
	}
	missing := filepath.Join(aliasParent, "missing")
	raw := "[[projects]]\nrepository = \"github.com/acme/widget\"\nname = \"widget\"\npath = " +
		strconvQuote(missing) + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(configHome, "config.toml"), []byte(raw), 0o600,
	))

	snapshot, err := LoadGlobalSnapshot()

	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)
	assert.Equal(t, missing, snapshot.Projects[0].Persisted.Path)
	assert.Equal(t, filepath.Clean(missing), snapshot.Projects[0].Effective.Path)
}

func TestCompareAndSwapProjectUsesExactPersistedEntry(t *testing.T) {
	tests := []struct {
		name        string
		projects    []models.Project
		expected    models.Project
		replacement *models.Project
		wantChanged bool
		want        []models.Project
	}{
		{
			name: "replace one exact raw entry",
			projects: []models.Project{
				{Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before"},
				{Repository: "github.com/acme/other", Name: "other", Path: "~/other", LastTouched: "kept"},
			},
			expected: models.Project{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before",
			},
			replacement: &models.Project{
				Repository: "github.com/acme/widget", Name: "widget", Path: "/repos/widget", LastTouched: "before",
			},
			wantChanged: true,
			want: []models.Project{
				{Repository: "github.com/acme/widget", Name: "widget", Path: "/repos/widget", LastTouched: "before"},
				{Repository: "github.com/acme/other", Name: "other", Path: "~/other", LastTouched: "kept"},
			},
		},
		{
			name: "remove one exact raw entry",
			projects: []models.Project{{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before",
			}},
			expected: models.Project{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before",
			},
			wantChanged: true,
			want:        []models.Project{},
		},
		{
			name: "changed metadata is a no-op",
			projects: []models.Project{{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "after",
			}},
			expected: models.Project{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before",
			},
			want: []models.Project{{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "after",
			}},
		},
		{
			name: "duplicate exact entries are a no-op",
			projects: []models.Project{
				{Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before"},
				{Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before"},
			},
			expected: models.Project{
				Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before",
			},
			want: []models.Project{
				{Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before"},
				{Repository: "github.com/acme/widget", Name: "widget", Path: "~/old/widget", LastTouched: "before"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configHome := t.TempDir()
			t.Setenv("KWT_HOME", configHome)
			writePersistedProjects(t, filepath.Join(configHome, "config.toml"), []models.Project{tt.expected})
			snapshot, err := LoadGlobalSnapshot()
			require.NoError(t, err)
			require.Len(t, snapshot.Projects, 1)
			writePersistedProjects(t, filepath.Join(configHome, "config.toml"), tt.projects)

			changed, err := CompareAndSwapProject(snapshot.Projects[0], tt.replacement)

			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
			stored, err := readGlobalViper(filepath.Join(configHome, "config.toml"))
			require.NoError(t, err)
			var projects []models.Project
			require.NoError(t, stored.UnmarshalKey("projects", &projects))
			if projects == nil {
				projects = []models.Project{}
			}
			assert.Equal(t, tt.want, projects)
		})
	}
}

func TestCompareAndSwapProjectRejectsOccupiedCanonicalTarget(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	configPath := filepath.Join(configHome, "config.toml")
	stale := models.Project{
		Repository:  "github.com/acme/widget",
		Name:        "stale-widget",
		Path:        "/old/widget",
		LastTouched: "before",
	}
	writePersistedProjects(t, configPath, []models.Project{stale})
	snapshot, err := LoadGlobalSnapshot()
	require.NoError(t, err)
	require.Len(t, snapshot.Projects, 1)

	target := t.TempDir()
	alias := filepath.Join(t.TempDir(), "widget-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symbolic links are not supported: %v", err)
	}
	live := models.Project{
		Repository:  "github.com/acme/widget",
		Name:        "live-widget",
		Path:        alias,
		LastTouched: "concurrent",
	}
	writePersistedProjects(t, configPath, []models.Project{stale, live})
	replacement := stale
	replacement.Path = target

	changed, err := CompareAndSwapProject(snapshot.Projects[0], &replacement)

	require.NoError(t, err)
	assert.False(t, changed)
	stored, err := readGlobalViper(configPath)
	require.NoError(t, err)
	var projects []models.Project
	require.NoError(t, stored.UnmarshalKey("projects", &projects))
	assert.Equal(t, []models.Project{stale, live}, projects)
}

func TestCompareAndSwapProjectPreservesUnknownFields(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(configHome, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = "~/old/widget"
last_touched = "before"
future_policy = "keep-matched"

[[projects]]
repository = "github.com/acme/other"
name = "other"
path = "/repos/other"
last_touched = "kept"
future_policy = "keep-unrelated"
`), 0o600))
	snapshot, err := LoadGlobalSnapshot()
	require.NoError(t, err)
	replacement := snapshot.Projects[0].Persisted
	replacement.Path = "/repos/widget"

	changed, err := CompareAndSwapProject(snapshot.Projects[0], &replacement)

	require.NoError(t, err)
	assert.True(t, changed)
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(stored), "future_policy = 'keep-matched'")
	assert.Contains(t, string(stored), "future_policy = 'keep-unrelated'")
}

func TestCompareAndSwapProjectRejectsUnknownFieldChange(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("KWT_HOME", configHome)
	path := filepath.Join(configHome, "config.toml")
	original := `[[projects]]
repository = "github.com/acme/widget"
name = "widget"
path = "/old/widget"
last_touched = "before"
future_policy = "observed"
`
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
	snapshot, err := LoadGlobalSnapshot()
	require.NoError(t, err)
	concurrent := strings.Replace(original, "observed", "concurrent", 1)
	require.NoError(t, os.WriteFile(path, []byte(concurrent), 0o600))
	replacement := snapshot.Projects[0].Persisted
	replacement.Path = "/repos/widget"

	changed, err := CompareAndSwapProject(snapshot.Projects[0], &replacement)

	require.NoError(t, err)
	assert.False(t, changed)
	stored, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, concurrent, string(stored))
}

func writePersistedProjects(t *testing.T, path string, projects []models.Project) {
	t.Helper()
	current := viper.New()
	current.SetConfigType(configType)
	current.Set("projects", projects)
	require.NoError(t, writeGlobalViperAtomically(path, current))
}

func strconvQuote(value string) string {
	return strconv.Quote(filepath.ToSlash(value))
}
