package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestLoadTokenFromFileAndEnv(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("  file-secret\n"), 0o600))
	t.Setenv("KWT_FLEET_TOKEN", "env-secret")

	got, err := LoadToken(models.FleetConfig{TokenFile: tokenFile, TokenEnv: "KWT_FLEET_TOKEN"})
	require.NoError(t, err)
	assert.Equal(t, "file-secret", got)

	t.Setenv("KWT_FLEET_TOKEN", "  env-secret\n")
	got, err = LoadToken(models.FleetConfig{TokenEnv: "KWT_FLEET_TOKEN"})
	require.NoError(t, err)
	assert.Equal(t, "env-secret", got)
}

func TestLoadTokenRejectsEmptyAndMissingTokens(t *testing.T) {
	emptyTokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(emptyTokenFile, []byte(" \n\t"), 0o600))
	t.Setenv("KWT_FLEET_EMPTY_TOKEN", " \n")

	tests := []struct {
		name string
		cfg  models.FleetConfig
	}{
		{name: "empty file", cfg: models.FleetConfig{TokenFile: emptyTokenFile}},
		{name: "missing file", cfg: models.FleetConfig{TokenFile: filepath.Join(t.TempDir(), "missing")}},
		{name: "empty env", cfg: models.FleetConfig{TokenEnv: "KWT_FLEET_EMPTY_TOKEN"}},
		{name: "missing source", cfg: models.FleetConfig{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadToken(tt.cfg)

			require.Error(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestClientUsesBearerTokenAndTimeout(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{HubURL: server.URL, Token: "secret", Timeout: 2 * time.Second})
	manifest := testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")
	err := client.Publish(context.Background(), manifest)
	require.NoError(t, err)
	assert.Equal(t, "Bearer secret", gotAuth)
}

func TestClientRejectsPlaintextNonLoopbackHubURL(t *testing.T) {
	tests := []struct {
		name   string
		hubURL string
	}{
		{name: "public", hubURL: "http://192.0.2.10:8787"},
		{name: "private lan", hubURL: "http://192.168.1.10:8787"},
		{name: "magicdns", hubURL: "http://myhub.tailnet-1234.ts.net:8787"},
		{name: "tailscale ipv4", hubURL: "http://100.64.1.2:8787"},
		{name: "tailscale ipv6", hubURL: "http://[fd7a:115c:a1e0::ab12]:8787"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(ClientOptions{HubURL: tt.hubURL, Token: "secret"})

			req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/fleet/state", nil)

			require.Error(t, err)
			assert.Nil(t, req)
			assert.Contains(t, err.Error(), "only allowed for loopback")
		})
	}
}

func TestClientAllowsPlaintextLoopbackHubURL(t *testing.T) {
	client := NewClient(ClientOptions{HubURL: "http://127.0.0.1:8787", Token: "secret"})

	req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/fleet/state", nil)

	require.NoError(t, err)
	assert.Equal(t, "Bearer secret", req.Header.Get("Authorization"))
}

func TestClientAllowsHTTPSHubURL(t *testing.T) {
	client := NewClient(ClientOptions{HubURL: "https://hub.example.test", Token: "secret"})

	req, err := client.newRequest(context.Background(), http.MethodGet, "/api/v1/fleet/state", nil)

	require.NoError(t, err)
	assert.Equal(t, "Bearer secret", req.Header.Get("Authorization"))
}

func TestClientRejectsPlaintextRedirectToUnverifiedHost(t *testing.T) {
	var mu sync.Mutex
	destinationRequests := 0
	destinationAuth := ""
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		destinationRequests++
		destinationAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer destination.Close()
	_, destinationPort, err := net.SplitHostPort(destination.Listener.Addr().String())
	require.NoError(t, err)

	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+net.JoinHostPort("hub.example.test", destinationPort)+r.URL.Path, http.StatusFound)
	}))
	defer redirect.Close()
	_, redirectPort, err := net.SplitHostPort(redirect.Listener.Addr().String())
	require.NoError(t, err)

	dialer := &net.Dialer{}
	actualAddressByPort := map[string]string{
		redirectPort:    redirect.Listener.Addr().String(),
		destinationPort: destination.Listener.Addr().String(),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec -- test server certificate
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr == nil && host == "hub.example.test" {
			address = actualAddressByPort[port]
		}
		return dialer.DialContext(ctx, network, address)
	}

	client := NewClient(ClientOptions{
		HubURL: "https://" + net.JoinHostPort("hub.example.test", redirectPort),
		Token:  "secret",
	})
	client.httpClient.Transport = transport
	_, _, _, err = client.State(context.Background(), "")

	assert.Error(t, err)
	if err != nil {
		assert.Contains(t, err.Error(), "plaintext sync hub URL")
	}
	mu.Lock()
	assert.Zero(t, destinationRequests)
	assert.Empty(t, destinationAuth)
	mu.Unlock()
}

func TestClientRejectsPlaintextTailnetBeforeEnvironmentProxy(t *testing.T) {
	if os.Getenv("KWT_TEST_PROXY_HELPER") == "1" {
		client := NewClient(ClientOptions{HubURL: "http://100.64.1.2:8787", Token: "secret"})
		_, _, _, err := client.State(context.Background(), "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only allowed for loopback")
		return
	}

	var mu sync.Mutex
	proxyRequests := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyRequests++
		mu.Unlock()
	}))
	defer proxy.Close()

	baseEnv := make([]string, 0, len(os.Environ()))
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.EqualFold(key, "HTTP_PROXY") || strings.EqualFold(key, "HTTPS_PROXY") ||
			strings.EqualFold(key, "ALL_PROXY") || strings.EqualFold(key, "NO_PROXY") || key == "REQUEST_METHOD" ||
			key == "KWT_TEST_PROXY_HELPER" {
			continue
		}
		baseEnv = append(baseEnv, env)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestClientRejectsPlaintextTailnetBeforeEnvironmentProxy$")
	cmd.Env = append(baseEnv,
		"HTTP_PROXY="+proxy.URL,
		"http_proxy="+proxy.URL,
		"NO_PROXY=",
		"no_proxy=",
		"KWT_TEST_PROXY_HELPER=1",
	)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	mu.Lock()
	assert.Zero(t, proxyRequests)
	mu.Unlock()
}

func TestClientHonorsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{HubURL: server.URL, Token: "secret", Timeout: 10 * time.Millisecond})
	start := time.Now()
	err := client.Publish(context.Background(), testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa"))

	require.Error(t, err)
	assert.Less(t, time.Since(start), 150*time.Millisecond)
}

func TestClientStateReturnsStateETagAndNotModified(t *testing.T) {
	wantState := FleetState{
		SchemaVersion: StateSchemaVersion,
		StateVersion:  "state-1",
		Hosts: []HostState{{
			HostID:     "host-a",
			Hostname:   "Host-A",
			Platform:   "darwin/arm64",
			ObservedAt: fixedStoreTime,
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		w.Header().Set("ETag", `"state-1"`)
		if r.Header.Get("If-None-Match") == `"state-1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(w, http.StatusOK, wantState)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{HubURL: server.URL, Token: "secret"})
	gotState, gotETag, notModified, err := client.State(context.Background(), "")
	require.NoError(t, err)
	assert.False(t, notModified)
	assert.Equal(t, `"state-1"`, gotETag)
	assert.Equal(t, wantState, gotState)

	gotState, gotETag, notModified, err = client.State(context.Background(), gotETag)
	require.NoError(t, err)
	assert.True(t, notModified)
	assert.Equal(t, `"state-1"`, gotETag)
	assert.Equal(t, FleetState{}, gotState)
}

func TestClientForgetSendsDeleteWithBearerToken(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{HubURL: server.URL, Token: "secret"})
	err := client.Forget(context.Background(), "host-a")

	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/api/v1/fleet/hosts/host-a", gotPath)
	assert.Equal(t, "Bearer secret", gotAuth)
}

func TestClientEffectiveHubURL(t *testing.T) {
	assert.Equal(t, "https://hub.example.test", EffectiveHubURL(models.FleetConfig{
		HubURL: " https://hub.example.test ",
		Hub:    models.FleetHubConfig{ListenAddr: "127.0.0.1:8787"},
	}))
	assert.Equal(t, "http://127.0.0.1:8787", EffectiveHubURL(models.FleetConfig{
		Hub: models.FleetHubConfig{ListenAddr: " 127.0.0.1:8787 "},
	}))
	assert.Equal(t, "http://127.0.0.1:8787", EffectiveHubURL(models.FleetConfig{
		Hub: models.FleetHubConfig{ListenAddr: " http://127.0.0.1:8787 "},
	}))
	assert.Empty(t, EffectiveHubURL(models.FleetConfig{}))
}

func TestPublishBestEffortWarnsAndReturnsNilOnUnreachableHub(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	hubURL := server.URL
	server.Close()
	t.Setenv("KWT_FLEET_TOKEN", "secret")
	builder := &stubFleetManifestBuilder{
		manifest: ptrManifest(testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")),
	}
	var warn bytes.Buffer

	err := PublishBestEffort(context.Background(), &models.Config{
		Fleet: models.FleetConfig{
			Enabled:  true,
			HubURL:   hubURL,
			TokenEnv: "KWT_FLEET_TOKEN",
		},
	}, builder, &warn)

	require.NoError(t, err)
	assert.Equal(t, 1, builder.calls)
	assert.Contains(t, warn.String(), "warning: sync publish failed:")
	assert.Less(t, len(warn.String()), 240)
}

func TestPublishBestEffortSkipsManifestBuildWhenHubURLInvalid(t *testing.T) {
	t.Setenv("KWT_FLEET_TOKEN", "secret")
	builder := &stubFleetManifestBuilder{
		manifest: ptrManifest(testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")),
	}
	var warn bytes.Buffer

	err := PublishBestEffort(context.Background(), &models.Config{
		Fleet: models.FleetConfig{
			Enabled:  true,
			HubURL:   "http://192.0.2.10:8787",
			TokenEnv: "KWT_FLEET_TOKEN",
		},
	}, builder, &warn)

	require.NoError(t, err)
	assert.Zero(t, builder.calls,
		"an unusable hub URL must fail before the expensive manifest build")
	assert.Contains(t, warn.String(), "plaintext sync hub URL")
}

func TestPublishBestEffortNoopWhenFleetDisabled(t *testing.T) {
	builder := &stubFleetManifestBuilder{
		manifest: ptrManifest(testManifest("host-a", "Host-A", "darwin/arm64", "github.com/kenn-io/kwt", "branch", "feature/fleet", "aaa")),
	}
	var warn bytes.Buffer

	err := PublishBestEffort(context.Background(), &models.Config{Fleet: models.FleetConfig{Enabled: false}}, builder, &warn)

	require.NoError(t, err)
	assert.Zero(t, builder.calls)
	assert.Empty(t, warn.String())
}

func TestPublishBestEffortWarnsOnBuilderFailure(t *testing.T) {
	t.Setenv("KWT_FLEET_TOKEN", "secret")
	builder := &stubFleetManifestBuilder{err: errors.New("build failed")}
	var warn bytes.Buffer

	err := PublishBestEffort(context.Background(), &models.Config{
		Fleet: models.FleetConfig{
			Enabled:  true,
			HubURL:   "http://127.0.0.1:1",
			TokenEnv: "KWT_FLEET_TOKEN",
		},
	}, builder, &warn)

	require.NoError(t, err)
	assert.Equal(t, 1, builder.calls)
	assert.Contains(t, warn.String(), "warning: sync publish failed:")
	assert.Contains(t, warn.String(), "build failed")
}

func TestPublishBestEffortReturnsOnCallerDeadlineWhenBuilderIgnoresContext(t *testing.T) {
	t.Setenv("KWT_FLEET_TOKEN", "secret")
	builder := &sleepingFleetManifestBuilder{sleep: 120 * time.Millisecond}
	var warn bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := PublishBestEffort(ctx, &models.Config{
		Fleet: models.FleetConfig{
			Enabled:  true,
			HubURL:   "http://127.0.0.1:1",
			TokenEnv: "KWT_FLEET_TOKEN",
		},
	}, builder, &warn)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 80*time.Millisecond)
	assert.Contains(t, warn.String(), "warning: sync publish failed:")
	assert.Contains(t, warn.String(), context.DeadlineExceeded.Error())
}

type stubFleetManifestBuilder struct {
	manifest *Manifest
	err      error
	calls    int
}

func (b *stubFleetManifestBuilder) Build(context.Context, *models.Config) (*Manifest, error) {
	b.calls++
	return b.manifest, b.err
}

type sleepingFleetManifestBuilder struct {
	sleep time.Duration
}

func (b *sleepingFleetManifestBuilder) Build(context.Context, *models.Config) (*Manifest, error) {
	time.Sleep(b.sleep)
	return nil, nil
}

func ptrManifest(manifest Manifest) *Manifest {
	return &manifest
}
