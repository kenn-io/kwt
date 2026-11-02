package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorePutLatestAndBuildStateGroupsRows(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	first := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	second := testManifest("host-b", "Host-B", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")

	require.NoError(t, store.Put(t.Context(), first))
	require.NoError(t, store.Put(t.Context(), second))
	state, err := store.State(t.Context())

	require.NoError(t, err)
	require.Len(t, state.Rows, 1)
	assert.Equal(t, StateSchemaVersion, state.SchemaVersion)
	assert.Equal(t, "github.com/kenn-io/kwt", state.Rows[0].ProjectIdentity)
	assert.Equal(t, "kwt", state.Rows[0].ProjectName)
	assert.Equal(t, "branch", state.Rows[0].Kind)
	assert.Equal(t, "feature/fleet", state.Rows[0].Ref)
	assert.Equal(t, "feature/fleet", state.Rows[0].Branch)
	assert.ElementsMatch(t, []string{"host-a", "host-b"}, observationHosts(state.Rows[0]))
	assert.NotEmpty(t, state.StateVersion)
}

// Each host derives the project identity from its own clone's remote, so two of
// them can publish one repository under different capitalization.
func TestStoreGroupsRowsAcrossProjectIdentityCasing(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	first := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/KWT", "branch", "feature/fleet", "aaa")
	second := testManifest("host-b", "Host-B", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")

	require.NoError(t, store.Put(t.Context(), first))
	require.NoError(t, store.Put(t.Context(), second))
	state, err := store.State(t.Context())

	require.NoError(t, err)
	require.Len(t, state.Rows, 1,
		"one worktree must not become a row per spelling of its repository")
	assert.ElementsMatch(t, []string{"host-a", "host-b"}, observationHosts(state.Rows[0]),
		"both hosts' observations belong to the single row")
}

// A plain Git server on a case-sensitive filesystem can hold two repositories
// whose identities differ only in case, so their observations must stay apart.
func TestStoreKeepsCaseSensitiveHostIdentitiesSeparate(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	first := testManifest("host-a", "Host-A", "darwin/arm64", "git.example.com/srv/KWT", "branch", "feature/fleet", "aaa")
	second := testManifest("host-b", "Host-B", "darwin/arm64", "git.example.com/srv/kwt", "branch", "feature/fleet", "bbb")

	require.NoError(t, store.Put(t.Context(), first))
	require.NoError(t, store.Put(t.Context(), second))
	state, err := store.State(t.Context())

	require.NoError(t, err)
	require.Len(t, state.Rows, 2,
		"an unrecognized host may distinguish repositories by case")
	assert.Equal(t, []string{"host-a"}, observationHosts(state.Rows[0]))
	assert.Equal(t, []string{"host-b"}, observationHosts(state.Rows[1]))
}

func TestStoreGroupsRowsAcrossProjectIdentityCasingRegardlessOfOrder(t *testing.T) {
	// The surviving spelling must not depend on which host published first.
	identities := func(lowerFirst bool) string {
		store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
		a, b := "github.com/kenn-io/KWT", "github.com/kenn-io/kwt"
		if lowerFirst {
			a, b = b, a
		}
		require.NoError(t, store.Put(t.Context(),
			testManifest("host-a", "Host-A", "darwin/arm64", a, "branch", "feature/fleet", "aaa")))
		require.NoError(t, store.Put(t.Context(),
			testManifest("host-b", "Host-B", "darwin/arm64", b, "branch", "feature/fleet", "bbb")))
		state, err := store.State(t.Context())
		require.NoError(t, err)
		require.Len(t, state.Rows, 1)
		return state.Rows[0].ProjectIdentity
	}

	assert.Equal(t, "github.com/kenn-io/KWT", identities(false))
	assert.Equal(t, "github.com/kenn-io/kwt", identities(true),
		"the first host in sorted order supplies the displayed spelling")
}

func TestStoreProjectNameUsesLaterMatchingProjectManifest(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	first := testManifest("alpha", "alpha", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	first.Projects = nil
	second := testManifest("zulu", "zulu", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")

	require.NoError(t, store.Put(ctx, first))
	require.NoError(t, store.Put(ctx, second))
	state, err := store.State(ctx)

	require.NoError(t, err)
	require.Len(t, state.Rows, 1)
	assert.Equal(t, "kwt", state.Rows[0].ProjectName)
}

func TestStoreHostCollisionWarning(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, store.Put(ctx, testManifestWithHost("same", "one", "darwin/arm64")))
	require.NoError(t, store.Put(ctx, testManifestWithHost("same", "two", "linux/amd64")))
	state, err := store.State(ctx)
	require.NoError(t, err)
	require.Len(t, state.Warnings, 1)
	assert.Equal(t, "host_id_collision", state.Warnings[0].Code)
	assert.Equal(t, "same", state.Warnings[0].HostID)
}

func TestStorePutReplacesLatestManifestForHost(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	first := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	second := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")
	second.Worktrees[0].Ahead = 2

	require.NoError(t, store.Put(ctx, first))
	require.NoError(t, store.Put(ctx, second))
	state, err := store.State(ctx)

	require.NoError(t, err)
	require.Len(t, state.Hosts, 1)
	require.Len(t, state.Rows, 1)
	require.Len(t, state.Rows[0].Observations, 1)
	assert.Equal(t, "bbb", state.Rows[0].Observations[0].Head)
	assert.Equal(t, 2, state.Rows[0].Observations[0].Ahead)
}

func TestStoreDeleteRemovesHostAndChangesStateVersion(t *testing.T) {
	ctx := t.Context()
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	require.NoError(t, store.Put(ctx, testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")))
	require.NoError(t, store.Put(ctx, testManifest("host-b", "Host-B", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")))
	before, err := store.State(ctx)
	require.NoError(t, err)

	require.NoError(t, store.Delete(ctx, "host-a"))
	after, err := store.State(ctx)

	require.NoError(t, err)
	assert.NotEqual(t, before.StateVersion, after.StateVersion)
	require.Len(t, after.Hosts, 1)
	assert.Equal(t, "host-b", after.Hosts[0].HostID)
	require.Len(t, after.Rows, 1)
	assert.Equal(t, []string{"host-b"}, observationHosts(after.Rows[0]))
}

func TestStoreMissingFileReturnsEmptyDeterministicState(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "missing", "state.json"))

	first, err := store.State(t.Context())
	require.NoError(t, err)
	second, err := store.State(t.Context())
	require.NoError(t, err)

	assert.Equal(t, StateSchemaVersion, first.SchemaVersion)
	assert.NotEmpty(t, first.StateVersion)
	assert.Equal(t, first.StateVersion, second.StateVersion)
	assert.Empty(t, first.Hosts)
	assert.Empty(t, first.Rows)
	assert.Empty(t, first.Warnings)
}

func TestStorePutRejectsInvalidManifest(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Manifest)
	}{
		{
			name: "unknown schema",
			change: func(manifest *Manifest) {
				manifest.SchemaVersion = 99
			},
		},
		{
			name: "invalid host ID",
			change: func(manifest *Manifest) {
				manifest.HostID = "bad/id"
			},
		},
		{
			name: "normalized host ID mismatch",
			change: func(manifest *Manifest) {
				manifest.HostID = "Host-A"
			},
		},
		{
			name: "empty project identity",
			change: func(manifest *Manifest) {
				manifest.Projects[0].Identity = ""
			},
		},
		{
			name: "empty worktree project identity",
			change: func(manifest *Manifest) {
				manifest.Worktrees[0].ProjectIdentity = ""
			},
		},
		{
			name: "empty worktree kind",
			change: func(manifest *Manifest) {
				manifest.Worktrees[0].Kind = "   "
			},
		},
		{
			name: "empty worktree ref",
			change: func(manifest *Manifest) {
				manifest.Worktrees[0].Ref = "   "
			},
		},
		{
			name: "unknown worktree kind",
			change: func(manifest *Manifest) {
				manifest.Worktrees[0].Kind = "tag"
			},
		},
		{
			name: "raw remote URL as project identity",
			change: func(manifest *Manifest) {
				manifest.Projects[0].Identity = "https://user:token@github.com/kenn-io/kwt.git"
			},
		},
		{
			name: "raw remote URL as worktree project identity",
			change: func(manifest *Manifest) {
				manifest.Worktrees[0].ProjectIdentity = "git@github.com:kenn-io/kwt.git"
			},
		},
		{
			name: "unparseable project identity",
			change: func(manifest *Manifest) {
				manifest.Projects[0].Identity = "///"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
			manifest := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
			tt.change(&manifest)

			err := store.Put(t.Context(), manifest)

			require.Error(t, err)
		})
	}
}

func TestStorePutIdentityErrorDoesNotEchoSubmittedValue(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	manifest := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	manifest.Projects[0].Identity = "https://user:sekret-token@github.com/kenn-io/kwt.git"

	err := store.Put(t.Context(), manifest)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sekret-token",
		"validation errors travel back in HTTP responses and must not echo credentials")
}

func TestStorePutAcceptsCanonicalIdentityWithExplicitPort(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "state.json"))
	identity := "github.com:2222/kenn-io/kwt"
	manifest := testManifest("host-a", "Host-A", "darwin/arm64", identity, "branch", "feature/fleet", "aaa")

	err := store.Put(t.Context(), manifest)

	require.NoError(t, err)
}

func TestStoreStateVersionDeterministicRegardlessOfWriteOrder(t *testing.T) {
	ctx := t.Context()
	first := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	second := testManifest("host-b", "Host-B", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "bbb")

	left := NewFileStore(filepath.Join(t.TempDir(), "left.json"))
	require.NoError(t, left.Put(ctx, first))
	require.NoError(t, left.Put(ctx, second))
	leftState, err := left.State(ctx)
	require.NoError(t, err)

	right := NewFileStore(filepath.Join(t.TempDir(), "right.json"))
	require.NoError(t, right.Put(ctx, second))
	require.NoError(t, right.Put(ctx, first))
	rightState, err := right.State(ctx)
	require.NoError(t, err)

	assert.Equal(t, leftState.StateVersion, rightState.StateVersion)
	assert.Equal(t, leftState.Hosts, rightState.Hosts)
	assert.Equal(t, leftState.Rows, rightState.Rows)
	assert.Equal(t, leftState.Warnings, rightState.Warnings)
}

func TestStoreAtomicPersistenceCreatesPrivateParentAndReloads(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "nested", "fleet", "state.json")
	store := NewFileStore(path)
	require.NoError(t, store.Put(ctx, testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")))

	parent, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0700), parent.Mode().Perm())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, json.Valid(body))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	assert.Contains(t, raw, "hosts")

	reloaded, err := NewFileStore(path).State(ctx)
	require.NoError(t, err)
	require.Len(t, reloaded.Hosts, 1)
	assert.Equal(t, "host-a", reloaded.Hosts[0].HostID)
}

func TestStoreCanceledContextReturnsContextErrorAndDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)

	err := store.Put(ctx, testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	_, statErr := os.Stat(path)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func observationHosts(row FleetRow) []string {
	hosts := make([]string, 0, len(row.Observations))
	for _, observation := range row.Observations {
		hosts = append(hosts, observation.HostID)
	}
	return hosts
}

func testManifestWithHost(hostID string, hostname string, platform string) Manifest {
	return testManifest(hostID, hostname, platform, "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
}

func testManifest(hostID string, hostname string, platform string, projectIdentity string, kind string, ref string, head string) Manifest {
	return Manifest{
		SchemaVersion: ManifestSchemaVersion,
		HostID:        hostID,
		Host:          HostInfo{Hostname: hostname, Platform: platform},
		ObservedAt:    fixedStoreTime,
		Projects: []ProjectManifest{{
			Identity:  projectIdentity,
			Name:      "kwt",
			LocalRoot: "/workspace/user-a/code/kwt",
		}},
		Worktrees: []WorktreeManifest{{
			ProjectIdentity: projectIdentity,
			Kind:            kind,
			Ref:             ref,
			Branch:          branchForKind(kind, ref),
			Path:            filepath.Join("/worktrees", hostID, ref),
			Head:            head,
			HeadTime:        fixedStoreTime.Add(-time.Hour),
			Upstream:        "origin/" + ref,
			Ahead:           1,
			Behind:          2,
			Status:          ChangeStatus{Modified: 1, Untracked: 1},
			LastActivity:    fixedStoreTime.Add(-30 * time.Minute),
			IsMain:          ref == "main",
		}},
	}
}

func branchForKind(kind string, ref string) string {
	if kind != "branch" {
		return ""
	}
	return ref
}

var fixedStoreTime = time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)
