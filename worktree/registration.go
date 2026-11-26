package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/fslink"
)

type registrationIndex struct {
	commonDir       string
	commonDirKey    string
	worktreesDirKey string
	adminDirs       map[string]struct{}
	byPath          map[string][]string
	err             error
}

func (r *Repository) buildRegistrationIndex() *registrationIndex {
	worktreesDir := filepath.Join(r.commonDir, "worktrees")
	idx := &registrationIndex{
		commonDir:       r.commonDir,
		commonDirKey:    pathKey(r.commonDir),
		worktreesDirKey: pathKey(worktreesDir),
		adminDirs:       make(map[string]struct{}),
		byPath:          make(map[string][]string),
	}
	entries, readDirErr := os.ReadDir(worktreesDir)
	if readDirErr != nil {
		if !os.IsNotExist(readDirErr) {
			idx.err = fmt.Errorf("resolve worktree Git directory: %w", readDirErr)
		}
		return idx
	}
	for _, entry := range entries {
		// A stray file cannot be a registration; Git's own prune removes it.
		if !entry.IsDir() {
			continue
		}
		adminDir := filepath.Join(worktreesDir, entry.Name())
		registeredWorktreePath, ok, err := readWorktreeAdminBacklink(adminDir)
		if err != nil {
			idx.err = fmt.Errorf(
				"resolve worktree Git directory: incomplete administrative inventory: %w",
				err,
			)
			return idx
		}
		idx.adminDirs[pathKey(adminDir)] = struct{}{}
		if !ok {
			continue
		}
		key := comparableWorktreePath(registeredWorktreePath)
		idx.byPath[key] = append(idx.byPath[key], adminDir)
	}
	return idx
}

func (idx *registrationIndex) resolve(path, pathComparableKey string) (string, error) {
	dotGitPath := filepath.Join(path, ".git")
	directCommonClaim := false
	pointsOutsideCommon := false
	info, err := os.Stat(dotGitPath)
	if err == nil {
		if info.IsDir() {
			if pathKey(dotGitPath) == idx.commonDirKey {
				directCommonClaim = true
			} else {
				return "", fmt.Errorf(
					"resolve worktree Git directory for %s: %w",
					path, ErrWorktreeRepositoryMismatch,
				)
			}
		} else {
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
						gitDirKey := pathKey(gitDir)
						if gitDirKey == idx.commonDirKey {
							directCommonClaim = true
						} else if pathKey(filepath.Dir(gitDir)) != idx.worktreesDirKey {
							pointsOutsideCommon = true
						} else if _, indexed := idx.adminDirs[gitDirKey]; !indexed {
							if _, _, matchErr := readWorktreeAdminBacklink(gitDir); matchErr != nil {
								return "", fmt.Errorf(
									"resolve worktree Git directory: incomplete administrative inventory: %w",
									matchErr,
								)
							}
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
	if idx.err != nil {
		return "", idx.err
	}

	matches := idx.byPath[pathComparableKey]
	if directCommonClaim {
		if len(matches) != 0 {
			return "", fmt.Errorf(
				"resolve worktree Git directory: multiple Git directory claims map to %s",
				path,
			)
		}
		return idx.commonDir, nil
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

func (r *Repository) registration(path string) (string, error) {
	commonDir := r.commonDir
	commonDirKey := pathKey(commonDir)
	worktreesDirKey := pathKey(filepath.Join(commonDir, "worktrees"))
	targetKey := comparableWorktreePath(path)

	dotGitPath := filepath.Join(path, ".git")
	directCommonClaim := false
	pointsOutsideCommon := false
	info, err := os.Stat(dotGitPath)
	if err == nil {
		if info.IsDir() {
			if pathKey(dotGitPath) == commonDirKey {
				directCommonClaim = true
			} else {
				return "", fmt.Errorf(
					"resolve worktree Git directory for %s: %w",
					path, ErrWorktreeRepositoryMismatch,
				)
			}
		} else {
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
						if pathKey(gitDir) == commonDirKey {
							directCommonClaim = true
						} else {
							matchesPath, matchErr := worktreeAdminDirMatchesPathKey(
								gitDir, worktreesDirKey, targetKey,
							)
							if matchErr != nil {
								return "", fmt.Errorf(
									"resolve worktree Git directory: incomplete administrative inventory: %w",
									matchErr,
								)
							}
							if !matchesPath {
								pointsOutsideCommon = pathKey(filepath.Dir(gitDir)) !=
									worktreesDirKey
							}
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
	targetKey := comparableWorktreePath(path)
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		// A stray file cannot be a registration; Git's own prune removes it.
		if !entry.IsDir() {
			continue
		}
		adminDir := filepath.Join(r.commonDir, "worktrees", entry.Name())
		registeredWorktreePath, ok, matchErr := readWorktreeAdminBacklink(adminDir)
		if matchErr != nil {
			return nil, fmt.Errorf(
				"resolve worktree Git directory: incomplete administrative inventory: %w",
				matchErr,
			)
		}
		if ok && comparableWorktreePath(registeredWorktreePath) == targetKey {
			matches = append(matches, adminDir)
		}
	}
	return matches, nil
}

func worktreeAdminDirMatchesPathKey(
	adminDir string,
	worktreesDirKey string,
	targetKey string,
) (bool, error) {
	if pathKey(filepath.Dir(adminDir)) != worktreesDirKey {
		return false, nil
	}
	registeredWorktreePath, ok, err := readWorktreeAdminBacklink(adminDir)
	if err != nil || !ok {
		return false, err
	}
	return comparableWorktreePath(registeredWorktreePath) == targetKey, nil
}

func readWorktreeAdminBacklink(adminDir string) (string, bool, error) {
	gitDirPath := filepath.Join(adminDir, "gitdir")
	gitDirFile, err := fslink.ReadFile(gitDirPath)
	// A registration without a backlink names no checkout, for example after
	// a crash interrupted git worktree add. Git prunes it; it matches no path.
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read administrative backlink %s: %w", gitDirPath, err)
	}
	registeredDotGit := strings.TrimSpace(string(gitDirFile))
	if registeredDotGit == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(registeredDotGit) {
		registeredDotGit = filepath.Join(adminDir, registeredDotGit)
	}
	if filepath.Base(registeredDotGit) != ".git" {
		return "", false, fmt.Errorf(
			"administrative backlink %s does not identify a worktree .git file",
			gitDirPath,
		)
	}
	return filepath.Dir(registeredDotGit), true, nil
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
