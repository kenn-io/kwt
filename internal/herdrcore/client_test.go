package herdrcore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunUsesNativeCoreFlagsAndProtectedHelper(t *testing.T) {
	var got []string
	c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(`{"address":"t9","state":"live","labels":{"kwt.managed":"true"}}`), nil
	}}
	result, err := c.Run(t.Context(), RunOptions{Executable: "/opt/kwt", Directory: "/repo with spaces", Identifier: "auth", Context: "build", Command: "printf '%s' '$HOME; hi'", ProtectedNames: []string{"CUSTOM_TOKEN"}, AutoCleanup: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Address != "t9" {
		t.Fatal(result)
	}
	want := []string{"run", "--json", "--bg", "--outlive-parent", "--no-notify", "--cwd", "/repo with spaces", "--label", "kwt.managed=true", "--label", "kwt.identifier=auth", "--label", "kwt.context=build", "--rm", "--", "/opt/kwt", "herdr", "_run-command", "--strip", "CUSTOM_TOKEN", "--", "printf '%s' '$HOME; hi'"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args\ngot  %q\nwant %q", got, want)
	}
}

func TestLaunchEnvironmentStripsConfiguredCredential(t *testing.T) {
	t.Setenv("KWT_GITHUB_TOKEN", "synthetic-builtin")
	t.Setenv("CUSTOM_TOKEN", "synthetic-custom")
	for _, entry := range launchEnvironment([]string{"CUSTOM_TOKEN"}) {
		name, _, _ := strings.Cut(entry, "=")
		if name == "CUSTOM_TOKEN" || name == "KWT_GITHUB_TOKEN" {
			t.Fatalf("protected variable inherited: %s", name)
		}
	}
}

func TestListGetsFullMetadataAndExcludesUnmanagedAndRemote(t *testing.T) {
	var calls [][]string
	c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		switch args[0] {
		case "ls":
			return []byte(`{"terminals":[{"address":"t1"},{"address":"t2"},{"address":"remote/t3"}]}`), nil
		case "get":
			if args[len(args)-1] == "t1" {
				return []byte(`{"address":"t1","name":"dev","cwd":"/repo","state":"live","labels":{"kwt.managed":"true","kwt.identifier":"auth"},"status":{"state":"working"}}`), nil
			}
			return []byte(`{"address":"t2","labels":{}}`), nil
		}
		t.Fatalf("unexpected %q", args)
		return nil, nil
	}}
	sessions, err := c.List(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Identifier() != "auth" {
		t.Fatal(sessions)
	}
	if !reflect.DeepEqual(calls[0], []string{"ls", "--json", "--label", "kwt.managed=true", "--all"}) {
		t.Fatal(calls)
	}
	if len(calls) != 3 {
		t.Fatal(calls)
	}
}

func TestResolveExactAddressWinsAndAmbiguityFails(t *testing.T) {
	sessions := []Terminal{{Address: "t1", Labels: map[string]string{"kwt.identifier": "auth"}}, {Address: "t2", Labels: map[string]string{"kwt.identifier": "auth-tests"}}}
	got, err := Resolve(sessions, "t1")
	if err != nil || got.Address != "t1" {
		t.Fatal(got, err)
	}
	_, err = Resolve(sessions, "auth")
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatal(err)
	}
	_, err = Resolve(sessions, "")
	if err == nil {
		t.Fatal("empty must not select all")
	}
}

func TestKillChecksOwnershipAndDoesNotRemoveAfterFailure(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged", true: "failed-kill"}[managed], func(t *testing.T) {
			var calls []string
			c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args[0])
				if args[0] == "get" {
					data, _ := json.Marshal(Terminal{Address: "t1", Labels: map[string]string{"kwt.managed": map[bool]string{true: "true", false: "false"}[managed]}})
					return data, nil
				}
				return nil, errors.New("denied")
			}}
			if err := c.Kill(t.Context(), "t1"); err == nil {
				t.Fatal("expected error")
			}
			want := []string{"get"}
			if managed {
				want = append(want, "kill")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls)
			}
		})
	}
}

func TestRunUnknownOutcomeNeverRetries(t *testing.T) {
	calls := 0
	c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		return nil, errors.New("connection lost")
	}}
	_, err := c.Run(t.Context(), RunOptions{Executable: "/kwt", Directory: "/repo", Command: "echo hi"})
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "inspect") {
		t.Fatal(calls, err)
	}
}

func FuzzResolveNeverSelectsUnmatchedTerminal(f *testing.F) {
	f.Add("auth")
	f.Add("t1")
	f.Add("")
	f.Fuzz(func(t *testing.T, pattern string) {
		sessions := []Terminal{{Address: "t1", Labels: map[string]string{"kwt.identifier": "auth", "kwt.context": "run"}}}
		got, err := Resolve(sessions, pattern)
		if err == nil && (pattern == "" || got.Address != "t1" || (pattern != "t1" && !strings.Contains(strings.ToLower("auth"), strings.ToLower(pattern)) && !strings.Contains("run", strings.ToLower(pattern)))) {
			t.Fatalf("unsafe match %q: %+v", pattern, got)
		}
	})
}
