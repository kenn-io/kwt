package daemon

import (
	"context"
	"maps"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
)

type fixedRuntimeStatus struct{ status Status }

func (s fixedRuntimeStatus) Status(time.Time) Status { return s.status }

func validMetadata(home, token string) map[string]string {
	return map[string]string{
		metadataHome:          home,
		metadataRevision:      "abc",
		metadataRevisionTime:  "2026-08-09T12:00:00Z",
		metadataSchemaMajor:   strconv.Itoa(APISchemaMajor),
		metadataSchemaVersion: APISchemaVersion,
		metadataCapabilities: strings.Join([]string{
			CapabilityShutdown,
			CapabilityStatus,
		}, ","),
		metadataToken: token,
	}
}

func TestNewRuntimeRecordAlwaysAdvertisesRevisionTime(t *testing.T) {
	home := t.TempDir()
	record, _, err := NewRuntimeRecord(home, Build{
		Version:      "v1.2.3",
		Revision:     "abc",
		RevisionTime: "2026-08-09T12:00:00Z",
	}, kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"})
	require.NoError(t, err)

	value, present := record.Metadata[metadataRevisionTime]
	assert.True(t, present)
	assert.Equal(t, "2026-08-09T12:00:00Z", value)

	record, _, err = NewRuntimeRecord(home, Build{Version: "development"}, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: "127.0.0.1:1",
	})
	require.NoError(t, err)
	value, present = record.Metadata[metadataRevisionTime]
	assert.True(t, present)
	assert.Empty(t, value)
}

func TestNewRuntimeRecordAdvertisesFirstDomainContracts(t *testing.T) {
	record, _, err := NewRuntimeRecord(
		t.TempDir(),
		Build{Version: "development"},
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	require.NoError(t, err)

	capabilities := strings.Split(record.Metadata[metadataCapabilities], ",")
	assert.Contains(t, capabilities, "worktree.inventory.v1")
	assert.Contains(t, capabilities, "project.removal.v1")
}

func TestParseRuntimeMetadataRevisionTimeCompatibility(t *testing.T) {
	home := t.TempDir()
	record := kitdaemon.NewRuntimeRecord(
		ServiceName,
		"v1.2.3",
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	record.Metadata = validMetadata(home, "secret")

	t.Run("new schema accepts canonical time", func(t *testing.T) {
		metadata, err := parseRuntimeMetadata(record, home)
		require.NoError(t, err)
		assert.True(t, metadata.revisionTimeAdvertised)
		assert.Equal(t, "2026-08-09T12:00:00Z", metadata.revisionTime)
	})

	t.Run("new schema accepts explicit unknown time", func(t *testing.T) {
		unknown := record
		unknown.Metadata = maps.Clone(record.Metadata)
		unknown.Metadata[metadataRevisionTime] = ""
		metadata, err := parseRuntimeMetadata(unknown, home)
		require.NoError(t, err)
		assert.True(t, metadata.revisionTimeAdvertised)
		assert.Empty(t, metadata.revisionTime)
	})

	t.Run("new schema rejects missing time", func(t *testing.T) {
		missing := record
		missing.Metadata = maps.Clone(record.Metadata)
		delete(missing.Metadata, metadataRevisionTime)
		_, err := parseRuntimeMetadata(missing, home)
		require.ErrorContains(t, err, "revision time")
	})

	t.Run("new schema rejects malformed time", func(t *testing.T) {
		malformed := record
		malformed.Metadata = maps.Clone(record.Metadata)
		malformed.Metadata[metadataRevisionTime] = "2026-08-09 12:00:00"
		_, err := parseRuntimeMetadata(malformed, home)
		require.ErrorContains(t, err, "revision time")
	})

	t.Run("legacy schema accepts omitted time", func(t *testing.T) {
		legacy := record
		legacy.Metadata = maps.Clone(record.Metadata)
		legacy.Metadata[metadataSchemaVersion] = "1.2.0"
		delete(legacy.Metadata, metadataRevisionTime)
		metadata, err := parseRuntimeMetadata(legacy, home)
		require.NoError(t, err)
		assert.False(t, metadata.revisionTimeAdvertised)
		assert.Empty(t, metadata.revisionTime)
	})
}

func TestInspectRemovesADeadPIDRuntimeRecord(t *testing.T) {
	store := kitdaemon.RuntimeStore{Dir: t.TempDir(), Prefix: RuntimePrefix}
	rec := kitdaemon.RuntimeRecord{
		PID:             math.MaxInt32,
		ProcessIdentity: "1",
		Network:         kitdaemon.NetworkTCP,
		Address:         "127.0.0.1:1",
		Service:         ServiceName,
		Metadata:        validMetadata(t.TempDir(), "secret"),
	}
	path, err := store.Write(rec)
	require.NoError(t, err)

	observation, err := Inspect(
		context.Background(),
		store,
		rec.Metadata[metadataHome],
	)
	require.NoError(t, err)
	assert.Equal(t, RuntimeAbsent, observation.State)
	assert.NoFileExists(t, path)
}

func TestInspectRemovesAReusedPIDRecord(t *testing.T) {
	store := kitdaemon.RuntimeStore{Dir: t.TempDir(), Prefix: RuntimePrefix}
	rec := kitdaemon.NewRuntimeRecord(
		ServiceName,
		"v1",
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	rec.ProcessIdentity = "1"
	rec.Metadata = validMetadata(t.TempDir(), "secret")
	path, err := store.Write(rec)
	require.NoError(t, err)

	observation, err := Inspect(context.Background(), store, rec.Metadata[metadataHome])
	require.NoError(t, err)
	assert.Equal(t, RuntimeAbsent, observation.State)
	assert.NoFileExists(t, path)
}

func TestInspectPreservesMatchingButUnresponsiveOwner(t *testing.T) {
	home := t.TempDir()
	store := kitdaemon.RuntimeStore{
		Dir:    filepath.Join(home, "runtime"),
		Prefix: RuntimePrefix,
	}
	rec := kitdaemon.NewRuntimeRecord(
		ServiceName,
		"v1",
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	rec.Metadata = validMetadata(home, "secret")
	path, err := store.Write(rec)
	require.NoError(t, err)

	observation, err := Inspect(context.Background(), store, home)
	require.NoError(t, err)
	assert.Equal(t, RuntimeUnresponsive, observation.State)
	assert.FileExists(t, path)
}

func TestInspectPreservesOwnerWithInvalidRevisionTime(t *testing.T) {
	home := t.TempDir()
	store := RuntimeStore(home)
	record := kitdaemon.NewRuntimeRecord(
		ServiceName,
		"v1.2.3",
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
	)
	record.Metadata = validMetadata(home, "secret")
	record.Metadata[metadataRevisionTime] = "invalid"
	path, err := store.Write(record)
	require.NoError(t, err)

	observation, err := Inspect(context.Background(), store, home)
	require.NoError(t, err)
	assert.Equal(t, RuntimeUnresponsive, observation.State)
	require.ErrorContains(t, observation.Err, "revision time")
	assert.FileExists(t, path)
}

func TestInspectReturnsAProofVerifiedRuntime(t *testing.T) {
	home := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listener.Addr().String()}
	rec := kitdaemon.NewRuntimeRecord(ServiceName, "v1", ep)
	rec.Metadata = validMetadata(home, "secret")
	proof, err := kitdaemon.NewProof([]byte("secret"))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(rec)
	require.NoError(t, err)
	status := Status{
		Service:       ServiceName,
		State:         StateReady,
		PID:           rec.PID,
		Version:       rec.Version,
		Revision:      "abc",
		RevisionTime:  "2026-08-09T12:00:00Z",
		Home:          home,
		Endpoint:      ep.Address,
		SchemaMajor:   APISchemaMajor,
		SchemaVersion: APISchemaVersion,
		Capabilities:  []string{CapabilityShutdown, CapabilityStatus},
	}
	handler := NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: ep.Address,
		Status:       fixedRuntimeStatus{status: status},
		Ping:         ping,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return status, nil
		},
	})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	store := RuntimeStore(home)
	_, err = store.Write(rec)
	require.NoError(t, err)

	observation, err := Inspect(context.Background(), store, home)
	require.NoError(t, err)
	assert.Equal(t, RuntimeReady, observation.State)
	assert.Equal(t, "abc", observation.Record.Metadata[metadataRevision])
	require.NotNil(t, observation.Client)
}

func TestInspectPreservesUnknownProcessIdentityAsUnresponsive(t *testing.T) {
	home := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ep := kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listener.Addr().String()}
	rec := kitdaemon.NewRuntimeRecord(ServiceName, "v1", ep)
	rec.ProcessIdentity = ""
	rec.Metadata = validMetadata(home, "secret")
	proof, err := kitdaemon.NewProof([]byte("secret"))
	require.NoError(t, err)
	ping, err := proof.NewPingHandler(rec)
	require.NoError(t, err)
	status := Status{
		Service:       ServiceName,
		State:         StateReady,
		PID:           rec.PID,
		Version:       rec.Version,
		Revision:      "abc",
		RevisionTime:  "2026-08-09T12:00:00Z",
		Home:          home,
		Endpoint:      ep.Address,
		SchemaMajor:   APISchemaMajor,
		SchemaVersion: APISchemaVersion,
		Capabilities:  []string{CapabilityShutdown, CapabilityStatus},
	}
	handler := NewServer(ServerOptions{
		Token:        "secret",
		ExpectedHost: ep.Address,
		Status:       fixedRuntimeStatus{status: status},
		Ping:         ping,
		Shutdown: func(context.Context, ShutdownRequest) (Status, error) {
			return status, nil
		},
	})
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	store := RuntimeStore(home)
	path, err := store.Write(rec)
	require.NoError(t, err)

	observation, err := Inspect(context.Background(), store, home)
	require.NoError(t, err)
	assert.Equal(t, RuntimeUnresponsive, observation.State)
	assert.FileExists(t, path)
}

func TestValidateRuntimeStatusRejectsBuildIdentityMismatch(t *testing.T) {
	home := t.TempDir()
	rec := kitdaemon.NewRuntimeRecord(
		ServiceName,
		"v1.2.3",
		kitdaemon.Endpoint{
			Network: kitdaemon.NetworkTCP,
			Address: "127.0.0.1:43210",
		},
	)
	rec.Metadata = validMetadata(home, "secret")
	metadata, err := parseRuntimeMetadata(rec, home)
	require.NoError(t, err)
	status := Status{
		Service:       ServiceName,
		State:         StateReady,
		Home:          home,
		Endpoint:      rec.Endpoint().Address,
		PID:           rec.PID,
		Version:       rec.Version,
		Revision:      metadata.revision,
		RevisionTime:  metadata.revisionTime,
		SchemaMajor:   APISchemaMajor,
		SchemaVersion: APISchemaVersion,
		Capabilities:  []string{CapabilityShutdown, CapabilityStatus},
	}

	for name, mutate := range map[string]func(*Status){
		"version":       func(value *Status) { value.Version = "v9.9.9" },
		"revision":      func(value *Status) { value.Revision = "different" },
		"revision_time": func(value *Status) { value.RevisionTime = "2026-08-09T12:00:01Z" },
	} {
		t.Run(name, func(t *testing.T) {
			mismatched := status
			mutate(&mismatched)
			require.Error(t, validateRuntimeStatus(rec, metadata, mismatched))
		})
	}
}

func TestClassifyRuntimeStatusAllowsOnlyReadyAsReady(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     State
		want      RuntimeState
		wantError bool
	}{
		{name: "ready", state: StateReady, want: RuntimeReady},
		{name: "draining", state: StateDraining, want: RuntimeDraining},
		{name: "starting", state: StateStarting, want: RuntimeStarting},
		{name: "failed", state: StateFailed, want: RuntimeFailed},
		{name: "empty", state: "", want: RuntimeUnresponsive, wantError: true},
		{name: "future", state: "future", want: RuntimeUnresponsive, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyRuntimeStatus(Status{
				SchemaMajor: APISchemaMajor,
				State:       test.state,
			})
			assert.Equal(t, test.want, got)
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
