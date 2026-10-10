package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

type BaseSyncRequest struct {
	Branch, SourceRef, BackupPrefix string
	Managed                         bool
}

// SyncBase follows the supplied ref without rewriting a checked-out branch or
// losing divergent user history. Managed clones retain rewritten tips in refs.
func (s *Scope) SyncBase(ctx context.Context, req BaseSyncRequest) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := s.rejectCreation(); err != nil {
		return err
	}
	if req.Branch == "" || req.SourceRef == "" {
		return errors.New("base branch and source ref are required")
	}
	if _, err := s.repo.run(ctx, s.repo.path, "check-ref-format", "refs/heads/"+req.Branch); err != nil {
		return err
	}
	remote, exists, err := s.repo.refOID(ctx, req.SourceRef)
	if err != nil || !exists {
		return err
	}
	local, exists, err := s.repo.refOID(ctx, "refs/heads/"+req.Branch)
	if err != nil {
		return err
	}
	if exists {
		if local == remote {
			return nil
		}
		checked, err := s.branchCheckedOut(ctx, req.Branch)
		if err != nil || checked {
			return err
		}
		_, err = s.repo.run(ctx, s.repo.path, "merge-base", "--is-ancestor", local, remote)
		if err != nil {
			if !gitcmd.IsExitCode(err, 1) {
				return err
			}
			if !req.Managed {
				return nil
			}
			if req.BackupPrefix == "" || !strings.HasPrefix(req.BackupPrefix, "refs/") || !strings.HasSuffix(req.BackupPrefix, "/") {
				return errors.New("managed base rewind requires a backup ref prefix")
			}
			backup := req.BackupPrefix + local
			if _, err = s.repo.run(ctx, s.repo.path, "check-ref-format", backup); err != nil {
				return err
			}
			if _, err = s.repo.run(ctx, s.repo.path, "update-ref", backup, local); err != nil {
				return fmt.Errorf("preserve base branch %q: %w", req.Branch, err)
			}
		}
	}
	_, err = s.repo.run(ctx, s.repo.path, "branch", "--force", "--", req.Branch, remote)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if checked, checkErr := s.branchCheckedOut(ctx, req.Branch); checkErr == nil && checked {
		return nil
	}
	if !exists {
		if refs, checkErr := s.repo.branchRefs(ctx); checkErr == nil {
			if available, _ := branchAvailable(refs, req.Branch); !available {
				return nil
			}
		}
	}
	return fmt.Errorf("update local base branch %q: %w", req.Branch, err)
}

func (r *Repository) refOID(ctx context.Context, ref string) (string, bool, error) {
	out, err := r.run(ctx, r.path, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if gitcmd.IsExitCode(err, 1) {
		return "", false, nil
	}
	return strings.TrimSpace(string(out)), err == nil, err
}

func (s *Scope) branchCheckedOut(ctx context.Context, branch string) (bool, error) {
	entries, err := s.inspect(ctx, false)
	if err != nil {
		return false, err
	}
	for _, entry := range entries.Entries {
		if entry.Branch == branch && !entry.Detached {
			return true, nil
		}
	}
	return false, nil
}
