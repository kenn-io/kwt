package herdrcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

type workspaceEstablisher interface {
	EstablishWorkspace(context.Context, RunOptions, string) (*Terminal, error)
}
type workspaceResolver interface {
	ResolveWorkspace(context.Context, RunOptions, string) (*Terminal, error)
}

// Only the hc process boundary is replaced; identity/reuse decisions stay real.
type workspaceCoreFixture struct {
	mu                 sync.Mutex
	records            []map[string]any
	launches, sends    int
	calls              []string
	launchErr, sendErr error
	compactRun         bool
	onGet              func(map[string]any)
}

func (f *workspaceCoreFixture) client() *Client { return &Client{run: f.run} }
func (f *workspaceCoreFixture) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "ls":
		entries := []map[string]string{}
		for _, r := range f.records {
			entries = append(entries, map[string]string{"address": r["address"].(string)})
		}
		return json.Marshal(map[string]any{"terminals": entries})
	case "get":
		for _, r := range f.records {
			if r["address"] == args[len(args)-1] {
				if f.onGet != nil {
					f.onGet(r)
				}
				return json.Marshal(r)
			}
		}
		return nil, &commandError{kind: "not_found", err: errors.New("not found")}
	case "run":
		f.launches++
		if f.launchErr != nil {
			return nil, f.launchErr
		}
		labels := map[string]string{}
		name, cwd := "", ""
		var argv []string
		for i := 1; i < len(args)-1; i++ {
			switch args[i] {
			case "--label":
				k, v, ok := strings.Cut(args[i+1], "=")
				if ok {
					labels[k] = v
				}
				i++
			case "--name":
				name = args[i+1]
				i++
			case "--cwd":
				cwd = args[i+1]
				i++
			case "--":
				argv = slices.Clone(args[i+1:])
				i = len(args)
			}
		}
		if name != "" {
			for _, r := range f.records {
				if r["name"] == name && r["state"] == "live" {
					return nil, &commandError{kind: "name_taken", err: errors.New("name taken")}
				}
			}
		}
		record := map[string]any{"address": fmt.Sprintf("t%d", 90+len(f.records)+1), "name": name, "cwd": cwd, "state": "live", "created_ms": int64(123 + len(f.records)), "labels": labels, "argv": argv, "keep": false, "reason": ""}
		f.records = append(f.records, record)
		if f.compactRun {
			return json.Marshal(map[string]string{"address": record["address"].(string)})
		}
		return json.Marshal(record)
	case "send":
		f.sends++
		if f.sendErr != nil {
			return nil, f.sendErr
		}
		return []byte(`{}`), nil
	case "label":
		for _, r := range f.records {
			if r["address"] == args[1] {
				labels := r["labels"].(map[string]string)
				for _, label := range args[2:] {
					k, v, ok := strings.Cut(label, "=")
					if ok {
						labels[k] = v
					}
				}
				return []byte(`{}`), nil
			}
		}
		return nil, errors.New("unknown label target")
	default:
		return nil, fmt.Errorf("unexpected Core operation %v", args)
	}
}
func workspaceAPI(t *testing.T, c *Client) workspaceEstablisher {
	t.Helper()
	workspace, ok := any(c).(workspaceEstablisher)
	if !ok {
		t.Fatal("Core client cannot establish and reuse a workspace")
	}
	return workspace
}
func workspaceOptions() RunOptions {
	return RunOptions{Executable: "/fixture/kwt", Directory: "/fixture/repo", Identifier: "repo-main", Context: "workspace", Command: "echo ready", ProtectedNames: []string{"KWT_TOKEN"}}
}

func TestWorkspaceNameTakenWithoutLiveMatchFails(t *testing.T) {
	fixture := &workspaceCoreFixture{launchErr: &commandError{kind: "name_taken", err: errors.New("name taken")}}
	terminal, err := workspaceAPI(t, fixture.client()).EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatalf("name conflict without a verified live workspace succeeded: %+v", terminal)
	}
	if fixture.launches != 1 || fixture.sends != 0 {
		t.Fatalf("retried a conflicting launch: launches=%d sends=%d", fixture.launches, fixture.sends)
	}
}

// Missing reuse would run the command twice on an ordinary live reopen.
func TestWorkspaceEstablishmentReusesVerifiedLiveTerminal(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	workspace := workspaceAPI(t, fixture.client())
	first, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	second, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != "t91" || second.Address != "t91" || fixture.launches != 1 || fixture.sends != 1 {
		t.Fatalf("reopen duplicated workspace: first=%v second=%v launches=%d sends=%d", first, second, fixture.launches, fixture.sends)
	}
	if !slices.Contains(first.Argv, "--workspace") || first.Directory != "/fixture/repo" {
		t.Fatalf("workspace bypassed login-shell helper: %+v", first)
	}
}

func TestWorkspaceFinalExitAllowsOneNewTerminal(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	workspace := workspaceAPI(t, fixture.client())
	if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	fixture.records[0]["state"] = "finished"
	fixture.records[0]["exit_code"] = 0
	terminal, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Address != "t92" || fixture.launches != 2 || len(fixture.records) != 2 {
		t.Fatalf("history prevented explicit reopen: terminal=%+v launches=%d records=%d", terminal, fixture.launches, len(fixture.records))
	}
}

func TestWorkspaceUncertainDeliveryIsNeverRepeated(t *testing.T) {
	fixture := &workspaceCoreFixture{sendErr: errors.New("connection lost after submission")}
	workspace := workspaceAPI(t, fixture.client())
	if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("uncertain initial submission succeeded")
	}
	fixture.sendErr = nil
	if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("reopen ignored uncertain initialization")
	}
	if fixture.launches != 1 || fixture.sends != 1 {
		t.Fatalf("retried uncertain mutation: launches=%d sends=%d", fixture.launches, fixture.sends)
	}
}

func TestWorkspaceUncertainLaunchIsNotRetried(t *testing.T) {
	fixture := &workspaceCoreFixture{launchErr: errors.New("connection lost")}
	_, err := workspaceAPI(t, fixture.client()).EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err == nil || fixture.launches != 1 {
		t.Fatalf("uncertain launch retried: launches=%d err=%v", fixture.launches, err)
	}
}

func TestWorkspaceRejectsChangedAuthority(t *testing.T) {
	for _, change := range []string{"argv", "keep", "created", "duplicate", "credentials", "pending"} {
		t.Run(change, func(t *testing.T) {
			fixture := &workspaceCoreFixture{}
			workspace := workspaceAPI(t, fixture.client())
			opts := workspaceOptions()
			if _, err := workspace.EstablishWorkspace(t.Context(), opts, "0123456789abcdef0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "argv":
				fixture.records[0]["argv"] = []string{"/bin/sh"}
			case "keep":
				fixture.records[0]["keep"] = true
			case "created":
				fixture.records[0]["created_ms"] = 0
			case "duplicate":
				copy := map[string]any{}
				for k, v := range fixture.records[0] {
					copy[k] = v
				}
				copy["address"] = "t93"
				fixture.records = append(fixture.records, copy)
			case "credentials":
				opts.ProtectedNames = append(opts.ProtectedNames, "NEW_TOKEN")
			case "pending":
				fixture.records[0]["state"] = "finished"
				fixture.records[0]["reason"] = "spawn_pending"
			}
			if _, err := workspace.EstablishWorkspace(t.Context(), opts, "0123456789abcdef0123456789abcdef"); err == nil {
				t.Fatal("changed authority accepted")
			}
			if fixture.launches != 1 || fixture.sends != 1 {
				t.Fatalf("changed identity caused mutation: launches=%d sends=%d", fixture.launches, fixture.sends)
			}
		})
	}
}

func TestWorkspaceGenerationSurvivesVerifiedMove(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	workspace := workspaceAPI(t, fixture.client())
	opts := workspaceOptions()
	if _, err := workspace.EstablishWorkspace(t.Context(), opts, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	opts.Directory = "/fixture/moved"
	opts.Identifier = "repo-renamed"
	terminal, err := workspace.EstablishWorkspace(t.Context(), opts, "0123456789abcdef0123456789abcdef")
	if err != nil || terminal == nil || terminal.Address != "t91" || fixture.launches != 1 {
		t.Fatalf("generation move lost workspace: terminal=%+v err=%v launches=%d", terminal, err, fixture.launches)
	}
}

func TestWorkspaceRefreshDoesNotCreate(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	resolver, ok := any(fixture.client()).(workspaceResolver)
	if !ok {
		t.Fatal("Core has no observation-only workspace resolver")
	}
	terminal, err := resolver.ResolveWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err != nil || terminal != nil || fixture.launches != 0 {
		t.Fatalf("refresh created workspace: %+v %v launches=%d", terminal, err, fixture.launches)
	}
}

func TestWorkspaceConcurrentDirectoryOpen(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	workspace := workspaceAPI(t, fixture.client())
	opts := workspaceOptions()
	opts.Command = ""
	results := make(chan *Terminal, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			terminal, err := workspace.EstablishWorkspace(t.Context(), opts, "")
			results <- terminal
			errs <- err
		}()
	}
	for range 2 {
		terminal, err := <-results, <-errs
		if err != nil || terminal == nil || terminal.Address != "t91" {
			t.Fatalf("concurrent directory open: terminal=%+v err=%v", terminal, err)
		}
	}
	live := 0
	for _, record := range fixture.records {
		if record["state"] == "live" {
			live++
		}
	}
	if live != 1 || fixture.sends != 0 {
		t.Fatalf("duplicate directory workspace: live=%d sends=%d", live, fixture.sends)
	}
}

// Core labels cannot turn a different executable into a verified workspace.
// Seeds also exercise shell punctuation without executing it.
func FuzzWorkspaceRejectsForgedHelper(f *testing.F) {
	f.Add("/bin/sh")
	f.Add("/fixture/kwt; echo forged")
	f.Add("$(false)")
	f.Fuzz(func(t *testing.T, executable string) {
		if executable == "/fixture/kwt" {
			return
		}
		fixture := &workspaceCoreFixture{}
		workspace := workspaceAPI(t, fixture.client())
		if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err != nil {
			t.Fatal(err)
		}
		argv := slices.Clone(fixture.records[0]["argv"].([]string))
		argv[0] = executable
		fixture.records[0]["argv"] = argv
		if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err == nil {
			t.Fatalf("adopted forged helper %q", executable)
		}
		if fixture.launches != 1 || fixture.sends != 1 {
			t.Fatal("identity refusal mutated Core")
		}
	})
}

func TestWorkspaceGetsFullLaunchRecord(t *testing.T) {
	fixture := &workspaceCoreFixture{compactRun: true}
	terminal, err := workspaceAPI(t, fixture.client()).EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef")
	if err != nil || terminal == nil || terminal.CreatedMS != 123 || len(terminal.Argv) == 0 {
		t.Fatalf("compact run response was trusted without full get: terminal=%+v err=%v", terminal, err)
	}
}

func TestWorkspaceRejectsIdentityChangingOnFreshGet(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	workspace := workspaceAPI(t, fixture.client())
	if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	gets := 0
	fixture.onGet = func(record map[string]any) {
		gets++
		if gets == 2 {
			record["created_ms"] = int64(987)
		}
	}
	if _, err := workspace.EstablishWorkspace(t.Context(), workspaceOptions(), "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("workspace creation identity changed after selection")
	}
	if fixture.sends != 1 || fixture.launches != 1 {
		t.Fatal("changed creation identity caused mutation")
	}
}

func TestWorkspaceProtectedLaunchRejectsCommand(t *testing.T) {
	fixture := &workspaceCoreFixture{}
	opts := workspaceOptions()
	opts.Context = "protected-workspace"
	if _, err := workspaceAPI(t, fixture.client()).EstablishWorkspace(t.Context(), opts, "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("protected workspace ran a layout command")
	}
	if fixture.launches != 0 || fixture.sends != 0 {
		t.Fatal("protected layout refusal touched Core")
	}
}
