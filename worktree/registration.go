package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/fslink"
)

func (r *Repository) registration(path string) (string, error) {
	commonDir := r.commonDir

	dotGitPath := filepath.Join(path, ".git")
	directCommonClaim := false
	pointsOutsideCommon := false
	info, err := os.Stat(dotGitPath)
	if err == nil {
		if info.IsDir() {
			if pathKey(dotGitPath) == pathKey(commonDir) {
				directCommonClaim = true
			} else {
				return "", fmt.Errorf(
					"resolve worktree Git directory for %s: %w",
					path, ErrWorktreeRepositoryMismatch,
				)
			}
		}
		data, readErr := fslink.ReadFile(dotGitPath)
		if readErr == nil {
			gitDir := strings.TrimSpace(string(data))
			if strings.HasPrefix(gitDir, "gitdir: ") {
				gitDir = strings.TrimSpace(
					strings.TrimPrefix(gitDir, "gitdir: "),
				)
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(path, gitDir)
				}
				gitDir = filepath.Clean(gitDir)
				if gitDirInfo, statErr := os.Stat(gitDir); statErr == nil &&
					gitDirInfo.IsDir() {
					if pathKey(gitDir) == pathKey(commonDir) {
						directCommonClaim = true
					} else {
						matchesPath, matchErr := worktreeAdminDirMatchesPath(
							gitDir, commonDir, dotGitPath,
						)
						if matchErr != nil {
							return "", fmt.Errorf(
								"resolve worktree Git directory: incomplete administrative inventory: %w",
								matchErr,
							)
						}
						if !matchesPath {
							pointsOutsideCommon = pathKey(filepath.Dir(gitDir)) !=
								pathKey(filepath.Join(commonDir, "worktrees"))
						}
					}
				}
			}
		}
	}
	if pointsOutsideCommon {
		return "", fmt.Errorf(
			"resolve worktree Git directory for %s: %w",
			path, ErrWorktreeRepositoryMismatch,
		)
	}

	matches, err := r.linkedRegistrations(path)
	if err != nil {
		return "", err
	}
	if directCommonClaim {
		if len(matches) != 0 {
			return "", fmt.Errorf(
				"resolve worktree Git directory: multiple Git directory claims map to %s",
				path,
			)
		}
		return commonDir, nil
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf(
			"resolve worktree Git directory: multiple administrative directories map to %s",
			path,
		)
	}
	return "", fmt.Errorf(
		"resolve worktree Git directory: %w",
		ErrWorktreeNotFound,
	)
}

func (r *Repository) linkedRegistrations(path string) ([]string, error) {
	entries, readDirErr := os.ReadDir(filepath.Join(r.commonDir, "worktrees"))
	if readDirErr != nil && !os.IsNotExist(readDirErr) {
		return nil, fmt.Errorf("resolve worktree Git directory: %w", readDirErr)
	}
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf(
				"resolve worktree Git directory: incomplete administrative inventory: unexpected entry %s",
				filepath.Join(r.commonDir, "worktrees", entry.Name()),
			)
		}
		adminDir := filepath.Join(r.commonDir, "worktrees", entry.Name())
		matchesPath, matchErr := worktreeAdminDirMatchesPath(
			adminDir, r.commonDir, filepath.Join(path, ".git"),
		)
		if matchErr != nil {
			return nil, fmt.Errorf(
				"resolve worktree Git directory: incomplete administrative inventory: %w",
				matchErr,
			)
		}
		if matchesPath {
			matches = append(matches, adminDir)
		}
	}
	return matches, nil
}

func worktreeAdminDirMatchesPath(
	adminDir string,
	commonDir string,
	dotGitPath string,
) (bool, error) {
	if pathKey(filepath.Dir(adminDir)) !=
		pathKey(filepath.Join(commonDir, "worktrees")) {
		return false, nil
	}
	gitDirPath := filepath.Join(adminDir, "gitdir")
	gitDirFile, err := fslink.ReadFile(gitDirPath)
	if err != nil {
		return false, fmt.Errorf("read administrative backlink %s: %w", gitDirPath, err)
	}
	registeredDotGit := strings.TrimSpace(string(gitDirFile))
	if registeredDotGit == "" {
		return false, fmt.Errorf("administrative backlink %s is empty", gitDirPath)
	}
	if !filepath.IsAbs(registeredDotGit) {
		registeredDotGit = filepath.Join(adminDir, registeredDotGit)
	}
	if filepath.Base(registeredDotGit) != ".git" {
		return false, fmt.Errorf(
			"administrative backlink %s does not identify a worktree .git file",
			gitDirPath,
		)
	}
	return comparableWorktreePath(filepath.Dir(registeredDotGit)) ==
		comparableWorktreePath(filepath.Dir(dotGitPath)), nil
}

// pathKey preserves the existing inventory identity comparison; lock recipes
// deliberately resolve paths separately.
func pathKey(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	}
	return path
}

func comparableWorktreePath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return pathKey(path)
	}
	return pathKey(filepath.Join(pathKey(filepath.Dir(path)), filepath.Base(path)))
}

// ReadWorktreeBacklink reads the direct .git claim without consulting Git.
func ReadWorktreeBacklink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Clean(dotGit), nil
	}
	data, err := fslink.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(data))
	if !strings.HasPrefix(target, "gitdir: ") {
		return "", nil
	}
	target = strings.TrimSpace(strings.TrimPrefix(target, "gitdir: "))
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	return filepath.Clean(target), nil
}
