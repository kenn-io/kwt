package herdrcore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestKillRemovesFinishedWithoutKillingAgain(t *testing.T) {
	for _, state := range []string{"live", "finished"} {
		t.Run(state, func(t *testing.T) {
			var calls []string
			c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args[0])
				return json.Marshal(Terminal{Address: "t1", State: state, Labels: map[string]string{"kwt.managed": "true"}})
			}}
			if err := c.Kill(t.Context(), "t1"); err != nil {
				t.Fatal(err)
			}
			want := []string{"get", "rm"}
			if state == "live" {
				want = []string{"get", "kill", "rm"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatal(calls)
			}
		})
	}
}

func TestAttachChecksOwnershipAndLiveState(t *testing.T) {
	for _, tc := range []struct {
		name, state         string
		managed, wantAttach bool
	}{
		{"owned-live", "live", true, true}, {"owned-finished", "finished", true, false}, {"personal", "live", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attached := false
			c := &Client{
				run: func(ctx context.Context, args ...string) ([]byte, error) {
					return json.Marshal(Terminal{Address: "t1", State: tc.state, Labels: map[string]string{"kwt.managed": map[bool]string{true: "true", false: "false"}[tc.managed]}})
				},
				attach: func(ctx context.Context, address string) error {
					attached = true
					if address != "t1" {
						t.Fatal(address)
					}
					return nil
				},
			}
			err := c.Attach(t.Context(), "t1")
			if tc.wantAttach && err != nil {
				t.Fatal(err)
			}
			if !tc.wantAttach && err == nil {
				t.Fatal("expected refusal")
			}
			if attached != tc.wantAttach {
				t.Fatal(attached)
			}
		})
	}
}

func TestListHandlesRemovalRaceButPropagatesOtherErrors(t *testing.T) {
	for _, kind := range []string{"not_found", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == "ls" {
					return []byte(`{"terminals":[{"address":"t1"}]}`), nil
				}
				return nil, &commandError{kind: kind, err: errors.New(kind)}
			}}
			terminals, err := c.List(t.Context(), true)
			if kind == "not_found" {
				if err != nil || len(terminals) != 0 {
					t.Fatal(terminals, err)
				}
			} else if err == nil {
				t.Fatal("must report unavailable Core")
			}
		})
	}
}

func TestRunRejectsInvalidResponseWithoutRetry(t *testing.T) {
	for _, output := range []string{`{}`, `{"address":"remote/t1"}`, `not json`} {
		t.Run(output, func(t *testing.T) {
			calls := 0
			c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) { calls++; return []byte(output), nil }}
			_, err := c.Run(t.Context(), RunOptions{Executable: "/kwt", Directory: "/repo", Command: "echo hi"})
			if err == nil || calls != 1 {
				t.Fatal(calls, err)
			}
		})
	}
}

func TestClientRejectsRemoteTargetsBeforeExecution(t *testing.T) {
	c := &Client{run: func(ctx context.Context, args ...string) ([]byte, error) {
		t.Fatal("must not call Core for remote targets")
		return nil, nil
	}}
	if err := c.Kill(t.Context(), "remote/t1"); err == nil {
		t.Fatal("remote kill accepted")
	}
	if err := c.Attach(t.Context(), "remote/t1"); err == nil {
		t.Fatal("remote attach accepted")
	}
}
