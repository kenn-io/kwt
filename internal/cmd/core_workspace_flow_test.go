package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/pkg/models"
)

// This synthetic CLI implements only Core's documented process protocol. It
// never starts Core, kwt's daemon, a shell workspace, or any real account client.
func installWorkspaceCoreCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state.json")
	script := `#!/usr/bin/python3
import json,os,sys
p=os.environ['KWT_CORE_FIXTURE_STATE']; a=sys.argv[1:]
try:s=json.load(open(p))
except FileNotFoundError:s={'records':[],'commands':[],'launches':0}
s.setdefault('routes',[]).append({'socket':os.environ.get('HC_SOCKET'),'session':os.environ.get('HC_SESSION')})
def save():
 with open(p,'w') as f:json.dump(s,f)
def record(address):
 for r in s['records']:
  if r['address']==address:return r
 print(json.dumps({'error':{'kind':'not_found','message':'fixture terminal absent'}}),file=sys.stderr);sys.exit(3)
def value(flag):return a[a.index(flag)+1]
if a[0]=='ls':out={'terminals':[{'address':r['address']} for r in s['records']]}
elif a[0]=='get':out=record(a[-1])
elif a[0]=='run':
 s['launches']+=1
 labels={}
 for i,x in enumerate(a):
  if x=='--label':k,v=a[i+1].split('=',1);labels[k]=v
 out={'address':'t'+str(90+s['launches']),'name':value('--name'),'cwd':value('--cwd'),'argv':a[a.index('--')+1:],'state':'live','created_ms':123+s['launches'],'labels':labels,'keep':False,'reason':'','no_parent':'--no-parent' in a}
 s['records'].append(out)
elif a[0]=='send':s['commands'].append(a);out={}
elif a[0]=='label':
 r=record(a[1])
 for pair in a[2:]:
  k,v=pair.split('=',1);r['labels'][k]=v
 out={}
elif a[0]=='attach':s['attached']=a[-1];out={}
elif a[0]=='kill':r=record(a[-1]);r['state']='finished';r['exit_code']=0;out={}
elif a[0]=='rm':r=record(a[-1]);s['records'].remove(r);out={}
else:raise RuntimeError('unsupported fixture operation '+repr(a))
save();print(json.dumps(out))
`
	if err := os.WriteFile(filepath.Join(root, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("KWT_CORE_FIXTURE_STATE", state)
	// Inventory observes Core directly rather than invoking the fake CLI. Serve
	// the same synthetic records over a private, short Unix socket.
	socketRoot, err := os.MkdirTemp("/tmp", "kwt-cli-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socket := filepath.Join(socketRoot, "core.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var connections sync.WaitGroup
	t.Cleanup(func() { _ = listener.Close(); <-done; connections.Wait() })
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				scanner := bufio.NewScanner(connection)
				for scanner.Scan() {
					var request struct {
						ID     int
						Method string
						Params struct{ Terminal string }
					}
					if json.Unmarshal(scanner.Bytes(), &request) != nil {
						return
					}
					var snapshot struct{ Records []json.RawMessage }
					data, err := os.ReadFile(state)
					if err != nil && !os.IsNotExist(err) {
						t.Error(err)
						return
					}
					if len(data) > 0 {
						if err := json.Unmarshal(data, &snapshot); err != nil {
							t.Error(err)
							return
						}
					}
					if snapshot.Records == nil {
						snapshot.Records = []json.RawMessage{}
					}
					var result any
					switch request.Method {
					case "hello":
						result = map[string]any{"generation": 1, "framed": false, "core": map[string]string{"id": "synthetic-cli-core"}}
					case "terminal.list":
						result = map[string]any{"terminals": snapshot.Records}
					case "terminal.get":
						for _, record := range snapshot.Records {
							var identity struct{ Address string }
							if json.Unmarshal(record, &identity) == nil && identity.Address == request.Params.Terminal {
								result = record
								break
							}
						}
					default:
						t.Errorf("unexpected fixture RPC %s", request.Method)
						return
					}
					if err := json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Setenv("KWT_CORE_FIXTURE_SOCKET", socket)
	return state
}

func coreWorkspaceConfig(t *testing.T, root string) *models.Config {
	t.Helper()
	cfg := &models.Config{}
	input, _ := json.Marshal(map[string]any{"Workspace": map[string]string{"Backend": "herdr-core", "CoreSocket": os.Getenv("KWT_CORE_FIXTURE_SOCKET")}})
	if err := json.Unmarshal(input, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Layouts.Default = "none"
	cfg.Workspaces = []models.Workspace{{Name: "fixture", Path: root}}
	return cfg
}

func TestCoreDirectoryOpenDoesNotRequireTmuxAndReusesAddress(t *testing.T) {
	state := installWorkspaceCoreCLI(t)
	root := t.TempDir()
	initCommandTestConfig(t, t.TempDir())
	viper.Set("workspace.backend", "herdr-core")
	viper.Set("workspace.core_socket", os.Getenv("KWT_CORE_FIXTURE_SOCKET"))
	t.Setenv("HC_SOCKET", "/fixture/wrong.sock")
	t.Setenv("HC_SESSION", "wrong-session")
	t.Setenv("HC_TERMINAL_ID", "tforeign")
	cfg := coreWorkspaceConfig(t, root)
	originalLayout, originalSelect := openLayout, openSelectLayout
	openLayout, openSelectLayout = "none", false
	t.Cleanup(func() { openLayout, openSelectLayout = originalLayout, originalSelect })
	var endpoints []tmux.SessionEndpoint
	for range 2 {
		err := openSelectedDirectoryWorkspaceWithResult(t.Context(), &CommandContext{Config: cfg}, cfg.Workspaces[0], nil, true, false, func(endpoint tmux.SessionEndpoint) error { endpoints = append(endpoints, endpoint); return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Launches int                                `json:"launches"`
		Records  []map[string]any                   `json:"records"`
		Routes   []struct{ Socket, Session string } `json:"routes"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	for _, route := range status.Routes {
		if route.Socket != os.Getenv("KWT_CORE_FIXTURE_SOCKET") || route.Session != "kwt" {
			t.Fatalf("workspace followed ambient Core routing: %s", data)
		}
	}
	if len(status.Records) > 0 && status.Records[0]["no_parent"] != true {
		t.Fatalf("workspace inherited a foreign Core parent: %s", data)
	}
	if status.Launches != 1 || len(endpoints) != 2 || endpoints[0] != endpoints[1] {
		t.Fatalf("directory reopen duplicated workspace: %s endpoints=%+v", data, endpoints)
	}
	wire, err := json.Marshal(openSessionResultFromEndpoint(endpoints[0]))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["workspace_runtime"] != "herdr-core" || fields["workspace_address"] != "t91" || fields["tmux_socket_name"] != nil {
		t.Fatalf("Core endpoint was lost or encoded as socket: %s", wire)
	}
}

func TestCoreMultipaneLayoutRejectedBeforeLaunch(t *testing.T) {
	state := installWorkspaceCoreCLI(t)
	cfg := coreWorkspaceConfig(t, t.TempDir())
	backend := newTUIBackend(cfg)
	cfg.Layouts.Presets = []models.Layout{{Name: "two", Arrange: "even-horizontal", Panes: []string{"echo one", "echo two"}}}
	row := workspaceRows(cfg.Workspaces, []tmux.WorkspaceSession{{}})[0]
	_, err := backend.OpenInTmux(t.Context(), row, "two")
	if err == nil || !strings.Contains(err.Error(), "pane") {
		t.Fatalf("unsupported Core geometry accepted: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("unsupported layout touched Core: %v", err)
	}
}

func TestCoreProtectedAttachUsesBlankShellAndCurrentPolicy(t *testing.T) {
	state := installWorkspaceCoreCLI(t)
	t.Setenv("KWT_HOME", t.TempDir())
	cfg := coreWorkspaceConfig(t, t.TempDir())
	cfg.Projects = testPRConfig().Projects
	cfg.Layouts.Default = "dangerous"
	cfg.Layouts.Presets = []models.Layout{{Name: "dangerous", Arrange: "even-horizontal", Panes: []string{"echo forbidden"}}}
	cfg.Fleet.TokenEnv = "CURRENT_PROTECTED_TOKEN"
	withPRCommandDeps(t, cfg, &fakePRService{})
	workspace := pullrequest.Workspace{Path: cfg.Workspaces[0].Path, Branch: "pr-32", Repository: "github.com/acme/widget", SessionName: "kwt-workspace-pr-32", Generation: "0123456789abcdef0123456789abcdef"}
	project := pullrequest.Project{Identity: "github.com/acme/widget", Path: "/repos/widget"}
	if err := pullrequest.NewFileStore(prStorePath()).Update(t.Context(), func(records map[string]pullrequest.Provenance) error {
		records["pr-32"] = pullrequest.Provenance{Project: project, Workspace: workspace}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stubPRWorkspaceGeneration(t, workspace.Path, workspace.Generation)
	inspectPRProjectClone = func(context.Context, pullrequest.Provenance) (pullrequest.Project, []pullrequest.Workspace, error) {
		return project, []pullrequest.Workspace{workspace}, nil
	}
	cmd, _, _ := prTestCommand()
	if err := runPRAttach(cmd, []string{workspace.Path}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Records []struct {
			Address string   `json:"address"`
			Argv    []string `json:"argv"`
			Keep    bool     `json:"keep"`
		} `json:"records"`
		Commands []any  `json:"commands"`
		Attached string `json:"attached"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || len(result.Commands) != 0 || result.Attached != "t91" || result.Records[0].Keep {
		t.Fatalf("protected flow used layout/recovery or failed attach: %s", data)
	}
	argv := result.Records[0].Argv
	if !slices.Contains(argv, "CURRENT_PROTECTED_TOKEN") || !slices.Contains(argv, "--workspace") {
		t.Fatalf("protected helper omitted current credential policy: %v", argv)
	}
	cfg.Fleet.TokenEnv = "NEW_PROTECTED_TOKEN"
	if err := runPRAttach(cmd, []string{workspace.Path}); err == nil {
		t.Fatal("protected live reuse ignored changed credential policy")
	}
}

func TestCoreEndpointSurvivesPublicDashboardProjections(t *testing.T) {
	var entry kwt.Entry
	data := []byte(`{"path":"/fixture/repo","generation":"0123456789abcdef0123456789abcdef","session_name":"repo-main","session_live":true,"workspace_runtime":"herdr-core","workspace_address":"t91","workspace_created_ms":123,"tmux_attach_mode":"direct"}`)
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	listed, err := json.Marshal(listedWorktree(entry))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(listed, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["workspace_runtime"] != "herdr-core" || fields["workspace_address"] != "t91" || fields["workspace_created_ms"] != float64(123) {
		t.Fatalf("public worktree projection lost Core endpoint: %s", listed)
	}
	dashboardEntry := dashboardInventoryEntry(entry)
	encoded, err := json.Marshal(dashboardEntry.TmuxEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	var endpointFields map[string]any
	if err := json.Unmarshal(encoded, &endpointFields); err != nil {
		t.Fatal(err)
	}
	if endpointFields["Runtime"] != "herdr-core" || endpointFields["Address"] != "t91" || endpointFields["CreatedMS"] != float64(123) {
		t.Fatalf("dashboard lost Core endpoint: %s", encoded)
	}
}

func TestCoreAddPreflightUsesSelectedRuntimeAndRejectsGeometry(t *testing.T) {
	repository := newTUITestRepo(t)
	state := installWorkspaceCoreCLI(t)
	t.Setenv("PATH", filepath.Dir(state)+string(os.PathListSeparator)+"/usr/bin:/bin")
	cfg := coreWorkspaceConfig(t, repository)
	oldLayout, oldSelect := addLayout, addSelectLayout
	addLayout, addSelectLayout = "none", false
	t.Cleanup(func() { addLayout, addSelectLayout = oldLayout, oldSelect })
	ctx := &CommandContext{Config: cfg, Git: git.New(repository)}
	if _, _, err := prepareLaunch(ctx); err != nil {
		t.Fatalf("selected Core still requires tmux: %v", err)
	}
	cfg.Layouts.Presets = []models.Layout{{Name: "two", Arrange: "even-horizontal", Panes: []string{"echo one", "echo two"}}}
	addLayout = "two"
	if _, _, err := prepareLaunch(ctx); err == nil || !strings.Contains(err.Error(), "pane") {
		t.Fatalf("add did not reject unsupported geometry before mutation: %v", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("preflight mutated Core: %v", err)
	}
}

func TestCoreDashboardPreparesResidentAttachBeforeStartingChild(t *testing.T) {
	state := installWorkspaceCoreCLI(t)
	initCommandTestConfig(t, t.TempDir())
	t.Setenv("TMUX", "")
	cfg := coreWorkspaceConfig(t, t.TempDir())
	backend := newTUIBackend(cfg)
	row := workspaceRows(cfg.Workspaces, []tmux.WorkspaceSession{{}})[0]
	if !backend.InsideTmux() {
		t.Fatal("Core dashboard does not choose resident child attachment")
	}
	process, err := backend.OpenInTmux(t.Context(), row, "none")
	if err != nil {
		t.Fatal(err)
	}
	if process == nil || process.Process != nil || !slices.Equal(process.Args[1:], []string{"attach", "--", "t91"}) {
		t.Fatalf("attach was not prepared as a child: %+v", process)
	}
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var before map[string]any
	if err := json.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if before["attached"] != nil {
		t.Fatal("attachment started before TUI suspension")
	}
	if err := process.Run(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after["attached"] != "t91" {
		t.Fatalf("prepared attach did not use verified address: %s", data)
	}
}

func TestCoreCachedAttachRefusesReplacementAddress(t *testing.T) {
	initCommandTestConfig(t, t.TempDir())
	var cached, replacement tmux.SessionEndpoint
	if err := json.Unmarshal([]byte(`{"SessionName":"fixture","Runtime":"herdr-core","Address":"t91","CreatedMS":123}`), &cached); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"SessionName":"fixture","Runtime":"herdr-core","Address":"t92","CreatedMS":124}`), &replacement); err != nil {
		t.Fatal(err)
	}
	row := dashboard.Row{Workspace: &dashboard.WorkspaceInfo{Name: "fixture", Path: t.TempDir()}, SessionName: "fixture", SessionLive: true, TmuxEndpoint: cached}
	_, err := resolveExistingWorkspaceEndpoint(t.Context(), row, func(context.Context, tmux.WorkspaceEndpointRequest) (tmux.SessionEndpoint, error) {
		return replacement, nil
	}, func(string) error { return nil })
	if err == nil {
		t.Fatal("cached attach switched to a replacement workspace")
	}
}
