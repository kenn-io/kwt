package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/fslink"
)

var (
	ErrWorktreeNotFound           = errors.New("worktree not found")
	ErrWorktreeRepositoryMismatch = errors.New("worktree belongs to a different repository")
	ErrWorktreeGenerationNotFound = errors.New("worktree generation not found")
)

// IdentityPolicy chooses one registration marker; an empty FileName disables it.
// Generate uses the existing 32-hex-digit generation format; Value selects an
// exact caller-owned identity. The two choices are mutually exclusive.
type IdentityPolicy struct {
	FileName, Value string
	Generate        bool
}

// ErrIdentityUnavailable means creation completed but its required marker could not be established.
var ErrIdentityUnavailable = errors.New("worktree created but its identity is unavailable; preserved")

func (p IdentityPolicy) validate() error {
	if p.FileName == "" && p.Value == "" && !p.Generate {
		return nil
	}
	if !validFileName(p.FileName) || p.Generate == (p.Value != "") || strings.TrimSpace(p.Value) != p.Value {
		return errors.New("identity requires a filename and either generation or an explicit value")
	}
	return nil
}

// EnsureIdentity reads or initializes the selected registration identity.
func (r *Repository) EnsureIdentity(ctx context.Context, path string, identity IdentityPolicy) (value string, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { value, err = s.EnsureIdentity(ctx, path, identity); return err })
	return value, err
}

func (s *Scope) EnsureIdentity(ctx context.Context, path string, identity IdentityPolicy) (string, error) {
	if err := s.check(ctx); err != nil {
		return "", err
	}
	if err := identity.validate(); err != nil {
		return "", err
	}
	if identity.FileName == "" {
		return "", nil
	}
	reserved, err := s.activeCreation()
	if err != nil {
		return "", err
	}
	if reserved != "" && comparableWorktreePath(path) == comparableWorktreePath(reserved) {
		return "", errors.New("worktree creation in progress")
	}
	dir, err := s.repo.registration(path)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(dir, identity.FileName)
	data, readErr := fslink.ReadFile(marker)
	value := strings.TrimSpace(string(data))
	if readErr == nil {
		if identity.Generate && ValidateWorktreeGeneration(value) == nil {
			return value, nil
		}
		if !identity.Generate {
			if value != identity.Value {
				return "", fmt.Errorf("worktree identity changed for %s", path)
			}
			return value, nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read worktree identity: %w", readErr)
	}
	value = identity.Value
	contents := []byte(value + "\n")
	if identity.Generate {
		var bytes [16]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return "", err
		}
		value = hex.EncodeToString(bytes[:])
		contents = []byte(value + "\n")
	}
	if err := atomicfile.WriteFile(marker, contents, atomicfile.WithPrivate()); err != nil {
		return "", fmt.Errorf("persist worktree identity: %w", err)
	}
	return value, nil
}

// ReadIdentity never creates a marker, including for a missing checkout whose
// administrative registration still exists.
func (r *Repository) ReadIdentity(ctx context.Context, path, fileName string) (value string, err error) {
	err = r.WithLock(ctx, func(s *Scope) error { value, err = s.ReadIdentity(ctx, path, fileName); return err })
	return value, err
}

func (s *Scope) ReadIdentity(ctx context.Context, path, fileName string) (string, error) {
	if err := s.check(ctx); err != nil {
		return "", err
	}
	if !validFileName(fileName) {
		return "", errors.New("identity filename is required")
	}
	dir, err := s.repo.registration(path)
	if err != nil {
		return "", err
	}
	data, err := fslink.ReadFile(filepath.Join(dir, fileName))
	if os.IsNotExist(err) {
		return "", errors.Join(ErrWorktreeGenerationNotFound, err)
	}
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if fileName == "kwt-generation" {
		if err := ValidateWorktreeGeneration(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

// WithIdentity retains the lock across validation and the caller's operation.
func (r *Repository) WithIdentity(ctx context.Context, path string, expected IdentityPolicy, fn func(*Scope) error) error {
	return r.WithLock(ctx, func(s *Scope) error { return s.WithIdentity(ctx, path, expected, fn) })
}

func (s *Scope) WithIdentity(ctx context.Context, path string, expected IdentityPolicy, fn func(*Scope) error) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := expected.validate(); err != nil {
		return err
	}
	if expected.FileName == "" || expected.Generate || fn == nil {
		return errors.New("identity guard requires an explicit identity and callback")
	}
	reserved, err := s.activeCreation()
	if err != nil {
		return err
	}
	if reserved != "" {
		return errors.New("worktree creation in progress")
	}
	value, err := s.ReadIdentity(ctx, path, expected.FileName)
	if err != nil || value != expected.Value {
		return &ConditionError{Reason: ReasonGenerationChanged, Path: path}
	}
	return fn(s)
}

// ValidateWorktreeGeneration checks the existing persisted generation format.
func ValidateWorktreeGeneration(generation string) error {
	decoded, err := hex.DecodeString(generation)
	if err != nil || len(decoded) != 16 {
		return errors.New("worktree generation must be a 32-character hexadecimal value")
	}
	return nil
}
