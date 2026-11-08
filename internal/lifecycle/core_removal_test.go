package lifecycle

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/service"
	shared "go.kenn.io/kwt/worktree"
)

// Even direct/force removal must not delete a checkout under a live Core
// workspace. The synthetic RPC server supplies the terminal record.
func TestRemovalServiceRefusesLiveCoreWithoutSessionCondition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture executable")
	}
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "force"}[force], func(t *testing.T) {
			repository, path := removalRepository(t, "core-live")
			generation, err := openSharedWorktrees(t, git.New(repository)).EnsureIdentity(t.Context(), path, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			sum := sha256.Sum256([]byte("generation:" + generation))
			name := "kwt-ws-" + hex.EncodeToString(sum[:])[:24]
			record := map[string]any{"address": "t91", "name": name, "cwd": path, "state": "live", "created_ms": 123, "keep": false, "labels": map[string]string{"kwt.managed": "true", "kwt.workspace": name, "kwt.generation": generation, "kwt.path": path, "kwt.context": "workspace", "kwt.identifier": "fixture-core", "kwt.initialized": "true"}, "argv": []string{"/fixture/kwt", "herdr", "_run-command", "--workspace", "--workspace-path", path, "--workspace-generation", generation, "--", ""}}
			socketRoot, err := os.MkdirTemp("/tmp", "kwt-remove-rpc-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
			socketPath := filepath.Join(socketRoot, "core.sock")
			listener, err := net.Listen("unix", socketPath)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			t.Cleanup(func() {
				_ = listener.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("RPC fixture did not retire")
				}
			})
			go func() {
				defer close(done)
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
				scanner := bufio.NewScanner(connection)
				for scanner.Scan() {
					var request struct {
						ID     int    `json:"id"`
						Method string `json:"method"`
					}
					if json.Unmarshal(scanner.Bytes(), &request) != nil {
						return
					}
					var result any
					switch request.Method {
					case "hello":
						result = map[string]any{"generation": 1, "version": "0.1.18", "framed": false}
					case "terminal.list":
						result = map[string]any{"terminals": []any{record}}
					case "terminal.get":
						result = record
					default:
						return
					}
					if json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
						return
					}
				}
			}()
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[workspace]\ncore_socket="+strconv.Quote(socketPath)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HC_SOCKET", "/fixture/wrong-daemon-startup.sock")
			t.Setenv("HC_SESSION", "wrong-daemon-session")
			script := "#!/bin/sh\nexit 73\n"

			if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			result, err := newFixtureRemovalService(RemovalServiceOptions{Home: home}).Remove(t.Context(), RemovalRequest{RepositoryPath: repository, Path: path, ExpectedGeneration: generation, Force: force})
			if err == nil || result.WorktreeRemoved {
				t.Fatalf("removed under live Core workspace: result=%+v err=%v", result, err)
			}
			if !service.IsCode(err, service.ProtectedSessionLive) {
				t.Fatalf("expected typed live workspace refusal, got %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("refusal lost checkout: %v", err)
			}
		})
	}
}

// A missing explicitly selected Core cannot establish absence, even when tmux
// is selected now and the caller requests force without a session condition.
func TestRemovalServiceMissingConfiguredCoreFailsClosed(t *testing.T) {
	repository, path := removalRepository(t, "core-unknown")
	generation, err := openSharedWorktrees(t, git.New(repository)).EnsureIdentity(t.Context(), path, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	socket := filepath.Join(home, "missing.sock")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[workspace]\nbackend='tmux'\ncore_socket="+strconv.Quote(socket)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := newFixtureRemovalService(RemovalServiceOptions{Home: home}).Remove(t.Context(), RemovalRequest{RepositoryPath: repository, Path: path, ExpectedGeneration: generation, Force: true})
	if err == nil || result.WorktreeRemoved {
		t.Fatalf("unreachable Core allowed removal: %+v %v", result, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// Cleanup is allowed only after native removal and only for the same finished
// creation. A Core reset/replacement must remain untouched and report partial
// success instead of hiding a completed worktree removal.
func TestRemovalServiceCleansOnlyVerifiedFinishedCore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	for _, change := range []string{"same creation", "creation replacement", "server replacement", "protocol replacement", "helper replacement", "path-bound creation", "path-bound helper replacement"} {
		t.Run(change, func(t *testing.T) {
			replacement := change != "same creation" && change != "path-bound creation"
			repository, path := removalRepository(t, "core-finished")
			generation, err := openSharedWorktrees(t, git.New(repository)).EnsureIdentity(t.Context(), path, shared.IdentityPolicy{FileName: "kwt-generation", Generate: true})
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte("generation:" + generation))
			name := "kwt-ws-" + hex.EncodeToString(sum[:])[:24]
			record := map[string]any{"address": "t91", "name": name, "cwd": path, "state": "finished", "created_ms": 123, "keep": false, "exit_code": 0, "labels": map[string]string{"kwt.managed": "true", "kwt.workspace_key": "generation:" + generation, "kwt.generation": generation, "kwt.workspace_path": path, "kwt.context": "workspace", "kwt.identifier": "fixture-core", "kwt.initialized": "true"}, "argv": []string{executable, "herdr", "_run-command", "--workspace", "--workspace-path", path, "--workspace-generation", generation, "--strip", "KWT_FLEET_TOKEN", "--strip", "KWT_GITHUB_TOKEN", "--", ""}}
			if strings.HasPrefix(change, "path-bound") {
				pathKey := "path:" + filepath.Clean(path)
				pathSum := sha256.Sum256([]byte(pathKey))
				record["name"] = "kwt-ws-" + hex.EncodeToString(pathSum[:])[:24]
				labels := record["labels"].(map[string]string)
				labels["kwt.workspace_key"], labels["kwt.generation"] = pathKey, ""
				record["argv"] = []string{executable, "herdr", "_run-command", "--workspace", "--workspace-path", path, "--workspace-generation", "", "--strip", "KWT_FLEET_TOKEN", "--strip", "KWT_GITHUB_TOKEN", "--", ""}
			}
			socketRoot, err := os.MkdirTemp("/tmp", "kwt-finished-rpc-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
			socket := filepath.Join(socketRoot, "core.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			removed, removedBeforeCheckout := false, false
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
						_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
						scanner := bufio.NewScanner(connection)
						for scanner.Scan() {
							var request struct {
								ID     int    `json:"id"`
								Method string `json:"method"`
							}
							if json.Unmarshal(scanner.Bytes(), &request) != nil {
								return
							}
							mu.Lock()
							var result any
							switch request.Method {
							case "hello":
								coreID, protocol := "synthetic-core", 1
								if _, err := os.Stat(path); os.IsNotExist(err) {
									if change == "server replacement" {
										coreID = "replacement-core"
									}
									if change == "protocol replacement" {
										protocol = 2
									}
								}
								result = map[string]any{"generation": protocol, "version": "0.1.18", "framed": false, "core": map[string]string{"id": coreID, "name": "fixture"}}
							case "terminal.list":
								result = map[string]any{"terminals": []any{record}}
							case "terminal.get":
								if _, err := os.Stat(path); os.IsNotExist(err) {
									if change == "creation replacement" {
										record["created_ms"] = 124
									}
									if change == "helper replacement" || change == "path-bound helper replacement" {
										record["argv"] = []string{"/replacement/helper"}
									}
								}
								result = record
							case "terminal.rm":
								if _, err := os.Stat(path); err == nil {
									removedBeforeCheckout = true
								}
								removed = true
								result = map[string]string{"removed": "t91"}
							default:
								mu.Unlock()
								return
							}
							err := json.NewEncoder(connection).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
							mu.Unlock()
							if err != nil {
								return
							}
						}
					}()
				}
			}()
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[workspace]\ncore_socket="+strconv.Quote(socket)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			result, removalErr := newFixtureRemovalService(RemovalServiceOptions{Home: home}).Remove(t.Context(), RemovalRequest{RepositoryPath: repository, Path: path, ExpectedGeneration: generation})
			if !result.WorktreeRemoved {
				t.Fatalf("native removal did not finish: %+v %v", result, removalErr)
			}
			mu.Lock()
			wasRemoved, tooSoon := removed, removedBeforeCheckout
			mu.Unlock()
			if tooSoon {
				t.Fatal("Core history removed before checkout")
			}
			if replacement {
				if removalErr == nil || wasRemoved {
					t.Fatalf("replacement cleanup accepted: removed=%v err=%v", wasRemoved, removalErr)
				}
			} else if removalErr != nil || !wasRemoved {
				t.Fatalf("verified finished record retained: removed=%v err=%v", wasRemoved, removalErr)
			}
		})
	}
}
