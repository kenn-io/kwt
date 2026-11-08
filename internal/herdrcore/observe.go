package herdrcore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func coreStateRoot() string {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "herdr-core")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "herdr-core")
}
func (c *Client) observationRoute() (socket, state string, explicit bool, err error) {
	if c.pinned {
		if !filepath.IsAbs(c.socket) {
			return "", "", true, fmt.Errorf("workspace.core_socket must be absolute")
		}
		return c.socket, "", true, nil
	}
	if socket = os.Getenv("HC_SOCKET"); socket != "" {
		return socket, "", true, nil
	}
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if configRoot == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", "", false, e
		}
		configRoot = filepath.Join(home, ".config")
	}
	data, e := os.ReadFile(filepath.Join(configRoot, "herdr-core", "config.toml"))
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", "", false, e
	}
	var cfg struct {
		Socket string `toml:"socket"`
	}
	if len(data) > 0 {
		if e = toml.Unmarshal(data, &cfg); e != nil {
			return "", "", false, e
		}
	}
	if cfg.Socket != "" {
		return cfg.Socket, "", true, nil
	}
	session := os.Getenv("HC_SESSION")
	if session == "" {
		session = "default"
	}
	if filepath.Base(session) != session || session == "." || session == ".." {
		return "", "", false, fmt.Errorf("invalid Core session")
	}
	state = filepath.Join(coreStateRoot(), session)
	if runtimeRoot := os.Getenv("XDG_RUNTIME_DIR"); runtimeRoot != "" {
		socket = filepath.Join(runtimeRoot, "herdr-core", session+".sock")
	} else {
		socket = filepath.Join(state, session+".sock")
	}
	return socket, state, false, nil
}

// coreObservation retains one bounded RPC stream. Mutating cleanup never
// reconnects: a replacement server cannot inherit this connection's authority.
type coreObservation struct {
	connection net.Conn
	scanner    *bufio.Scanner
	id         int
	stop       func() bool
	generation int
	coreID     string
}

type observationHello struct {
	Generation int  `json:"generation"`
	Framed     bool `json:"framed"`
	Core       struct {
		ID string `json:"id"`
	} `json:"core"`
}

func (o *coreObservation) close() { o.stop(); _ = o.connection.Close() }
func (o *coreObservation) call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := o.connection.SetDeadline(deadline); err != nil {
		return err
	}
	o.id++
	if err := json.NewEncoder(o.connection).Encode(map[string]any{"jsonrpc": "2.0", "id": o.id, "method": method, "params": params}); err != nil {
		return err
	}
	if !o.scanner.Scan() {
		if err := o.scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("Core closed the observation stream")
	}
	var reply struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(o.scanner.Bytes(), &reply); err != nil {
		return err
	}
	if reply.ID != o.id || len(reply.Result) == 0 || string(reply.Result) == "null" || (len(reply.Error) > 0 && string(reply.Error) != "null") {
		return fmt.Errorf("invalid Core RPC response for %s", method)
	}
	return json.Unmarshal(reply.Result, result)
}
func (o *coreObservation) hello(ctx context.Context) (observationHello, error) {
	var hello observationHello
	err := o.call(ctx, "hello", map[string]any{"framed": false}, &hello)
	if err == nil && (hello.Generation != 1 || hello.Framed) {
		err = fmt.Errorf("unsupported Core observation protocol")
	}
	return hello, err
}
func (c *Client) openObservation(ctx context.Context) (*coreObservation, error) {
	socket, state, explicit, err := c.observationRoute()
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socket)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			if _, e := os.Stat(state); errors.Is(e, os.ErrNotExist) {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("cannot observe configured Core at %s; explicitly start Core before retrying: %w", socket, err)
	}
	o := &coreObservation{connection: connection, scanner: bufio.NewScanner(connection)}
	o.scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	o.stop = context.AfterFunc(ctx, func() { _ = connection.Close() })
	hello, err := o.hello(ctx)
	if err != nil {
		o.close()
		return nil, err
	}
	o.generation = hello.Generation
	o.coreID = hello.Core.ID
	return o, nil
}
func (o *coreObservation) managed(ctx context.Context) ([]Terminal, error) {
	var list struct {
		Terminals *[]Terminal `json:"terminals"`
	}
	if err := o.call(ctx, "terminal.list", map[string]any{"all": true, "labels": map[string]string{"kwt.managed": "true"}}, &list); err != nil {
		return nil, err
	}
	if list.Terminals == nil {
		return nil, fmt.Errorf("Core omitted terminal inventory")
	}
	result := make([]Terminal, 0, len(*list.Terminals))
	for _, selected := range *list.Terminals {
		if selected.Labels["kwt.managed"] != "true" {
			continue
		}
		if !localAddress(selected.Address) || selected.CreatedMS <= 0 {
			return nil, fmt.Errorf("Core returned uncertain terminal identity")
		}
		var fresh Terminal
		if err := o.call(ctx, "terminal.get", map[string]any{"terminal": selected.Address}, &fresh); err != nil {
			return nil, err
		}
		if fresh.Address != selected.Address || fresh.CreatedMS != selected.CreatedMS || fresh.Labels["kwt.managed"] != "true" {
			return nil, fmt.Errorf("Core terminal changed during observation")
		}
		result = append(result, fresh)
	}
	return result, nil
}

// ObserveManaged uses only bounded read-only RPC. hc ls/get/status may start
// Core, so they cannot prove absence for inventory or worktree removal.
func (c *Client) ObserveManaged(ctx context.Context) ([]Terminal, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	o, err := c.openObservation(ctx)
	if err != nil || o == nil {
		return nil, err
	}
	defer o.close()
	return o.managed(ctx)
}

// RemovalObservation holds the original Core connection across native removal.
// It can remove only captured, freshly verified finished workspace identities.
type RemovalObservation struct {
	observation *coreObservation
	Terminals   []Terminal
}

func (c *Client) ObserveRemoval(ctx context.Context) (*RemovalObservation, error) {
	o, err := c.openObservation(ctx)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("Core removal requires an explicit live connection")
	}
	terminals, err := o.managed(ctx)
	if err != nil {
		o.close()
		return nil, err
	}
	return &RemovalObservation{observation: o, Terminals: terminals}, nil
}
func (o *RemovalObservation) Close() { o.observation.close() }
func (o *RemovalObservation) VerifyFinished(t Terminal, opts RunOptions, generation string) error {
	if o.observation.coreID == "" {
		return fmt.Errorf("Core omitted immutable server identity")
	}
	if t.State != "finished" {
		return fmt.Errorf("refusing to remove a live Core workspace")
	}
	return VerifyWorkspace(t, opts, generation)
}
func (o *RemovalObservation) RemoveFinished(ctx context.Context, expected Terminal, opts RunOptions, generation string) error {
	if err := o.VerifyFinished(expected, opts, generation); err != nil {
		return err
	}
	hello, err := o.observation.hello(ctx)
	if err != nil {
		return err
	}
	if hello.Generation != o.observation.generation || hello.Core.ID != o.observation.coreID {
		return fmt.Errorf("Core server identity changed before cleanup")
	}
	var fresh Terminal
	if err := o.observation.call(ctx, "terminal.get", map[string]any{"terminal": expected.Address}, &fresh); err != nil {
		return err
	}
	if fresh.Address != expected.Address || fresh.CreatedMS != expected.CreatedMS || fresh.Directory != expected.Directory || !slices.Equal(fresh.Argv, expected.Argv) {
		return fmt.Errorf("Core workspace creation identity changed before cleanup")
	}
	if err := o.VerifyFinished(fresh, opts, generation); err != nil {
		return err
	}
	var result struct {
		Removed string `json:"removed"`
	}
	if err := o.observation.call(ctx, "terminal.rm", map[string]any{"terminal": expected.Address}, &result); err != nil {
		return err
	}
	if result.Removed != expected.Address {
		return fmt.Errorf("Core returned uncertain removal result")
	}
	return nil
}
