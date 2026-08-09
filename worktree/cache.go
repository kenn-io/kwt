package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kit/safefileio"
)

const cacheVersion = 1

type Cache interface {
	Current() (Result, bool)
	Store(Result) error
}

type cacheFile struct {
	Version    int       `json:"version"`
	ObservedAt time.Time `json:"observed_at"`
	Snapshot   Snapshot  `json:"snapshot"`
}

type FileCache struct {
	mu      sync.RWMutex
	path    string
	current Result
	loaded  bool
}

func NewFileCache(home string) (*FileCache, *Diagnostic, error) {
	dir := filepath.Join(home, "cache")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, nil, fmt.Errorf("secure inventory cache directory: %w", err)
	}
	cache := &FileCache{path: filepath.Join(dir, "inventory-v1.json")}
	file, err := safefileio.OpenCurrentUserFile(cache.path)
	if err != nil {
		if os.IsNotExist(err) {
			return cache, nil, nil
		}
		return cache, &Diagnostic{At: time.Now(), Message: boundedDiagnostic(err)}, nil
	}
	defer func() { _ = file.Close() }()
	var stored cacheFile
	if err := json.NewDecoder(file).Decode(&stored); err != nil {
		return cache, &Diagnostic{At: time.Now(), Message: boundedDiagnostic(err)}, nil
	}
	if stored.Version != cacheVersion {
		return cache, &Diagnostic{At: time.Now(), Message: "unsupported inventory cache version"}, nil
	}
	cache.current = Result{Snapshot: stored.Snapshot, ObservedAt: stored.ObservedAt, Freshness: Stale}
	cache.loaded = true
	return cache, nil, nil
}

func (c *FileCache) Current() (Result, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.loaded {
		return Result{}, false
	}
	return cloneResult(c.current), true
}

func (c *FileCache) Store(result Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := cacheFile{Version: cacheVersion, ObservedAt: result.ObservedAt, Snapshot: cloneResult(result).Snapshot}
	file, err := os.CreateTemp(filepath.Dir(c.path), ".inventory-*.tmp")
	if err != nil {
		return fmt.Errorf("create inventory cache temporary file: %w", err)
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(stored); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode inventory cache: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync inventory cache: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close inventory cache: %w", err)
	}
	if err := replaceCacheFile(temporary, c.path); err != nil {
		return fmt.Errorf("publish inventory cache: %w", err)
	}
	c.current = cloneResult(result)
	c.loaded = true
	return nil
}

func boundedDiagnostic(err error) string {
	const maximumBytes = 512
	var result strings.Builder
	for _, character := range err.Error() {
		if unicode.IsControl(character) {
			character = ' '
		}
		if result.Len()+utf8.RuneLen(character) > maximumBytes {
			break
		}
		result.WriteRune(character)
	}
	message := strings.TrimSpace(result.String())
	if message == "" {
		return "inventory operation failed"
	}
	return message
}
