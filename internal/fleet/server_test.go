package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerRejectsMissingBearerToken(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/fleet/state", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServerPingUnauthenticated(t *testing.T) {
	srv := NewServer(ServerOptions{
		Store:   NewMemoryStoreForTest(),
		Token:   "secret",
		Service: "kwt-sync-test",
		Version: "test-version",
		PID:     123,
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ping", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["ok"])
	assert.Equal(t, "kwt-sync-test", body["service"])
	assert.Equal(t, "test-version", body["version"])
	assert.Equal(t, float64(123), body["pid"])
}

func TestServerPingDefaultsToSyncService(t *testing.T) {
	srv := NewServer(ServerOptions{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ping", nil)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "kwt-sync", body["service"])
}

func TestServerStoresManifestAndReturnsGroupedStateWithETag(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewFileStore(filepath.Join(t.TempDir(), "state.json")), Token: "secret"})
	body := encodeJSON(t, testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))
	post := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest", bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, post)
	require.Equal(t, http.StatusNoContent, rec.Code)

	get := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/fleet/state", nil)
	get.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, get)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("ETag"))

	var state FleetState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state))
	require.Len(t, state.Rows, 1)
	assert.Equal(t, "github.com/kenn-io/kwt", state.Rows[0].ProjectIdentity)
	assert.Equal(t, `"`+state.StateVersion+`"`, rec.Header().Get("ETag"))

	get304 := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/fleet/state", nil)
	get304.Header.Set("Authorization", "Bearer secret")
	get304.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec304 := httptest.NewRecorder()
	srv.ServeHTTP(rec304, get304)
	assert.Equal(t, http.StatusNotModified, rec304.Code)
	assert.Empty(t, rec304.Body.String())
}

func TestServerDeletesHost(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	body := encodeJSON(t, testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))
	post := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest", bytes.NewReader(body))
	post.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, post)
	require.Equal(t, http.StatusNoContent, rec.Code)

	del := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/fleet/hosts/host-a", nil)
	del.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, del)
	require.Equal(t, http.StatusNoContent, rec.Code)

	get := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/fleet/state", nil)
	get.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, get)
	require.Equal(t, http.StatusOK, rec.Code)
	var state FleetState
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state))
	assert.Empty(t, state.Hosts)
	assert.Empty(t, state.Rows)
}

func TestServerRejectsInvalidFleetPath(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest/extra", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestServerRejectsBodyHostMismatch(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	body := encodeJSON(t, testManifest("host-b", "Host-B", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServerRejectsInvalidHostIDPath(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	body := encodeJSON(t, testManifest("bad$id", "bad", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/bad$id/manifest", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServerRejectsEscapedSlashInManifestHostIDPath(t *testing.T) {
	store := NewMemoryStoreForTest()
	srv := NewServer(ServerOptions{Store: store, Token: "secret"})
	body := encodeJSON(t, testManifest("foo", "foo", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/foo%2Fmanifest", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assertEscapedSlashRejected(t, rec.Code)
	state, err := store.State(t.Context())
	require.NoError(t, err)
	assert.Empty(t, state.Hosts)
	assert.Empty(t, state.Rows)
}

func TestServerRejectsEscapedSlashInDeleteHostIDPath(t *testing.T) {
	store := NewMemoryStoreForTest()
	require.NoError(t, store.Put(t.Context(), testManifest("foo", "foo", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")))
	srv := NewServer(ServerOptions{Store: store, Token: "secret"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/fleet/hosts/foo%2Fbar", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assertEscapedSlashRejected(t, rec.Code)
	state, err := store.State(t.Context())
	require.NoError(t, err)
	require.Len(t, state.Hosts, 1)
	assert.Equal(t, "foo", state.Hosts[0].HostID)
}

func TestServerRejectsUnknownManifestSchemaVersion(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	manifest := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	manifest.SchemaVersion = 99
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest", bytes.NewReader(encodeJSON(t, manifest)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestServerRejectsManifestBodyOverOneMiB(t *testing.T) {
	srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/fleet/hosts/host-a/manifest", strings.NewReader(strings.Repeat("x", 1<<20+1)))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestServerUnsupportedMethodsReturnMethodNotAllowed(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "ping", method: http.MethodPost, path: "/api/v1/ping"},
		{name: "state", method: http.MethodPost, path: "/api/v1/fleet/state"},
		{name: "manifest", method: http.MethodGet, path: "/api/v1/fleet/hosts/host-a/manifest"},
		{name: "host delete", method: http.MethodPost, path: "/api/v1/fleet/hosts/host-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewServer(ServerOptions{Store: NewMemoryStoreForTest(), Token: "secret"})
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		})
	}
}

func TestParseHubEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "loopback", raw: "127.0.0.1:8787"},
		{name: "localhost", raw: "localhost:8787"},
		{name: "tailscale ipv4", raw: "100.64.1.2:8787", wantErr: true},
		{name: "tailscale ipv6", raw: "[fd7a:115c:a1e0::ab12]:8787", wantErr: true},
		{name: "non-self tailscale address", raw: "100.64.5.5:8787", wantErr: true},
		{name: "below tailnet cgnat block", raw: "100.63.255.255:8787", wantErr: true},
		{name: "private lan", raw: "192.168.1.10:8787", wantErr: true},
		{name: "unspecified", raw: "0.0.0.0:8787", wantErr: true},
		{name: "public", raw: "8.8.8.8:8787", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseHubEndpoint(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func encodeJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return body
}

func assertEscapedSlashRejected(t *testing.T, code int) {
	t.Helper()
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, code)
	assert.NotEqual(t, http.StatusNoContent, code)
}

func NewMemoryStoreForTest() Store {
	return &memoryStoreForTest{hosts: map[string]Manifest{}}
}

type memoryStoreForTest struct {
	hosts map[string]Manifest
}

func (s *memoryStoreForTest) Put(ctx context.Context, manifest Manifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateStoreManifest(manifest); err != nil {
		return err
	}
	s.hosts[manifest.HostID] = manifest
	return nil
}

func (s *memoryStoreForTest) Delete(ctx context.Context, hostID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := NormalizeHostID(hostID); err != nil {
		return err
	}
	delete(s.hosts, hostID)
	return nil
}

func (s *memoryStoreForTest) State(ctx context.Context) (FleetState, error) {
	if err := ctx.Err(); err != nil {
		return FleetState{}, err
	}
	hosts := make(map[string]Manifest, len(s.hosts))
	for hostID, manifest := range s.hosts {
		hosts[hostID] = manifest
	}
	return buildFleetState(storeFile{Hosts: hosts})
}
