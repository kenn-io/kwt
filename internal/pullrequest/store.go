package pullrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/gofrs/flock"
)

const provenanceVersion = 1

type FileStore struct {
	path string
}

type provenanceFile struct {
	Version int                   `json:"version"`
	Imports map[string]Provenance `json:"imports"`
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (s *FileStore) View(ctx context.Context, fn func(map[string]Provenance) error) error {
	return s.withLock(ctx, false, func() error {
		records, err := s.load()
		if err != nil {
			return err
		}
		return fn(records)
	})
}

func (s *FileStore) Update(ctx context.Context, fn func(map[string]Provenance) error) error {
	return s.withLock(ctx, true, func() error {
		records, err := s.load()
		if err != nil {
			return err
		}
		original := maps.Clone(records)
		if err := fn(records); err != nil {
			return err
		}
		if maps.EqualFunc(
			original,
			records,
			func(left, right Provenance) bool {
				return reflect.DeepEqual(left, right)
			},
		) {
			return nil
		}
		return s.save(records)
	})
}

// RemoveIfMatch deletes key only when its complete persisted value still
// matches the caller's observed provenance.
func (s *FileStore) RemoveIfMatch(
	ctx context.Context, key string, expected Provenance,
) (bool, error) {
	removed := false
	err := s.Update(ctx, func(records map[string]Provenance) error {
		current, ok := records[key]
		if !ok || !reflect.DeepEqual(current, expected) {
			return nil
		}
		delete(records, key)
		removed = true
		return nil
	})
	return removed, err
}

func (s *FileStore) withLock(ctx context.Context, write bool, fn func() error) error {
	if s == nil || s.path == "" {
		return fmt.Errorf("pull-request provenance path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create pull-request provenance directory: %w", err)
	}
	lock := flock.New(s.path+".lock", flock.SetPermissions(0600))
	var (
		locked bool
		err    error
	)
	if write {
		locked, err = lock.TryLockContext(ctx, 10*time.Millisecond)
	} else {
		locked, err = lock.TryRLockContext(ctx, 10*time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("lock pull-request provenance: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock pull-request provenance: lock unavailable")
	}
	defer func() { _ = lock.Unlock() }()
	return fn()
}

func (s *FileStore) load() (map[string]Provenance, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]Provenance), nil
		}
		return nil, fmt.Errorf("read pull-request provenance: %w", err)
	}
	if len(data) == 0 {
		return make(map[string]Provenance), nil
	}
	var state provenanceFile
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode pull-request provenance: %w", err)
	}
	if state.Version != provenanceVersion {
		return nil, fmt.Errorf("decode pull-request provenance: unsupported version %d", state.Version)
	}
	if state.Imports == nil {
		state.Imports = make(map[string]Provenance)
	}
	return state.Imports, nil
}

func (s *FileStore) save(records map[string]Provenance) error {
	data, err := json.MarshalIndent(provenanceFile{Version: provenanceVersion, Imports: records}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pull-request provenance: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(s.path), ".pull-requests-*.tmp")
	if err != nil {
		return fmt.Errorf("create pull-request provenance temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure pull-request provenance temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write pull-request provenance: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync pull-request provenance: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close pull-request provenance: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace pull-request provenance: %w", err)
	}
	return nil
}
