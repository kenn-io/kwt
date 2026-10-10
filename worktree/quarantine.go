package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	managed "go.kenn.io/kit/git/managed"
)

// QuarantineOptions supplies an unused recovery name and execution policy.
// The application owns name selection; no repository need exist at Path.
type QuarantineOptions struct {
	Path, Destination string
	Runner            gitcmd.Runner
	RunGit            managed.GitRunner
}

// Quarantine moves orphaned files intact to Destination. A valid checkout root,
// including a symlink to one in another repository, is left alone.
func Quarantine(ctx context.Context, opts QuarantineOptions) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !filepath.IsAbs(opts.Path) || !filepath.IsAbs(opts.Destination) {
		return false, errors.New("quarantine paths must be absolute")
	}
	info, err := os.Lstat(opts.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(opts.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	if info != nil && info.IsDir() {
		r := &Repository{runner: opts.Runner, runGit: opts.RunGit}

		inside, err := r.run(ctx, opts.Path, "rev-parse", "--is-inside-work-tree")
		if err != nil && !IsCheckoutAbsent(err) {
			return false, err
		}
		if err == nil && strings.TrimSpace(string(inside)) == "true" {
			top, err := r.revParseAbsolute(ctx, opts.Path, "--show-toplevel")
			if err != nil {
				return false, err
			}
			if pathKey(top[0]) == pathKey(opts.Path) {
				return false, nil
			}
		}
	}
	if _, err := os.Lstat(opts.Destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return false, fmt.Errorf("quarantine destination: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := os.Rename(opts.Path, opts.Destination); err != nil {
		return false, err
	}
	return true, nil
}

// IsCheckoutAbsent recognizes Git's missing or interrupted-checkout errors.
// Other failures remain errors; callers must not treat them as missing paths.
func IsCheckoutAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is not a working tree") ||
		strings.Contains(msg, "is not a worktree") ||
		strings.Contains(msg, "not a git repository") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "invalid gitfile format") ||
		strings.Contains(msg, "is not a .git file") ||
		(strings.Contains(msg, "failed to read worktrees/") && strings.Contains(msg, "/commondir: success"))
}
