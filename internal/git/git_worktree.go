package git

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofrs/flock"
	gitworktree "go.kenn.io/kit/git/worktree"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/pkg/models"
)

const (
	worktreeMutationLockName        = "kwt-worktree.lock"
	worktreeCreationLockName        = "kwt-worktree-create.lock"
	worktreeCreationReservationName = "kwt-worktree-create.path"
)

type worktreeCreationReservation struct {
	lock       *flock.Flock
	recordPath string
}

type worktreeCreatedError struct {
	err error
}

type incompleteWorktreeRemovalError struct {
	path  string
	cause error
}

func (e *incompleteWorktreeRemovalError) Error() string {
	return fmt.Sprintf(
		"worktree removed, but files remain at %s; inspect the path and remove it only if it contains leftovers from the removed worktree",
		e.path,
	)
}

func (e *incompleteWorktreeRemovalError) Unwrap() error {
	return e.cause
}

func (e *incompleteWorktreeRemovalError) WorktreeRemoved() bool {
	return true
}

// WorktreeWasRemoved reports whether an error was returned after Git had
// already deregistered the selected worktree.
func WorktreeWasRemoved(err error) bool {
	var removed interface{ WorktreeRemoved() bool }
	return errors.As(err, &removed) && removed.WorktreeRemoved()
}

// IncompleteInventoryError reports a repository whose worktree inventory
// cannot be observed completely enough to publish or replace cached state.
type IncompleteInventoryError struct {
	Path string
	Err  error
}

func (e *IncompleteInventoryError) Error() string {
	return fmt.Sprintf("incomplete worktree inventory at %s: %v", e.Path, e.Err)
}

func (e *IncompleteInventoryError) Unwrap() error {
	return e.Err
}

// IsIncompleteInventory reports whether err represents an incomplete
// repository worktree snapshot.
func IsIncompleteInventory(err error) bool {
	var incomplete *IncompleteInventoryError
	return errors.As(err, &incomplete)
}

func (e *worktreeCreatedError) Error() string {
	return e.err.Error()
}

func (e *worktreeCreatedError) Unwrap() error {
	return e.err
}

func (e *worktreeCreatedError) WorktreeCreated() bool {
	return true
}

func markWorktreeCreated(err error) error {
	return &worktreeCreatedError{err: err}
}

// ListWorktrees returns a list of all worktrees in the repository.
func (g *Git) ListWorktrees() ([]models.Worktree, error) {
	var worktrees []models.Worktree
	err := g.withWorktreeMutationLock(nil, func() error {
		reservedPath, creationActive, err := g.activeWorktreeCreation(nil)
		if err != nil {
			return err
		}
		if !creationActive {
			reservedPath = ""
		}
		worktrees, err = g.listWorktrees(reservedPath)
		return err
	})
	return worktrees, err
}

func (g *Git) listWorktrees(excludedPath string) ([]models.Worktree, error) {
	output, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	entries := gitworktree.ParsePorcelain(output)
	worktrees := make([]models.Worktree, 0, len(entries))
	excluded := ""
	if excludedPath != "" {
		excluded = comparableWorktreePath(excludedPath)
	}
	for _, entry := range entries {
		if excluded != "" && comparableWorktreePath(entry.Path) == excluded {
			continue
		}
		worktree := models.Worktree{
			Path: entry.Path, Branch: entry.Branch, CommitHash: entry.Head,
			Prunable: entry.Prunable,
		}
		if worktree.Branch == "" {
			worktree.Branch = g.getCurrentBranch(worktree.Path)
		}
		if info, statErr := os.Stat(worktree.Path); statErr == nil {
			worktree.CreatedAt = info.ModTime()
		}
		generation, generationErr := g.WorktreeGeneration(worktree.Path)
		if generationErr != nil {
			return nil, &IncompleteInventoryError{
				Path: worktree.Path,
				Err: fmt.Errorf(
					"initialize worktree generation: %w",
					generationErr,
				),
			}
		}
		worktree.Generation = generation
		worktrees = append(worktrees, worktree)
	}

	if len(worktrees) > 0 {
		mainDir, err := g.getMainRepoRoot()
		if err == nil {
			for i := range worktrees {
				resolvedPath := worktrees[i].Path
				if resolved, err := filepath.EvalSymlinks(resolvedPath); err == nil {
					resolvedPath = resolved
				}
				if resolvedPath == mainDir {
					worktrees[i].IsMain = true
					break
				}
			}
		}
	}

	return worktrees, nil
}

// AddWorktree creates a new worktree.
func (g *Git) AddWorktree(path, branch string, createBranch bool) error {
	_, err := g.addWorktreeReserved(path, branch, createBranch, false)
	return err
}

// AddWorktreeWithGeneration creates a worktree and returns the durable
// generation captured before the creation reservation is released.
func (g *Git) AddWorktreeWithGeneration(
	path, branch string,
	createBranch bool,
) (string, error) {
	return g.addWorktreeReserved(path, branch, createBranch, true)
}

func (g *Git) addWorktreeReserved(
	path, branch string,
	createBranch bool,
	requireGeneration bool,
) (string, error) {
	reservation, err := g.reserveWorktreeCreation(path, nil)
	if err != nil {
		return "", err
	}
	if err := g.addWorktree(path, branch, createBranch); err != nil {
		_ = reservation.release()
		return "", err
	}
	// Existing callers can recover generation through a later locked listing.
	// Identity-requiring callers fail while preserving the completed checkout.
	generation, generationErr := g.initializeWorktreeGenerationValue(path, nil)
	_ = reservation.release()
	if generationErr != nil && requireGeneration {
		return "", markWorktreeCreated(fmt.Errorf(
			"worktree created at %s but its generation is unavailable; preserved: %w",
			path,
			generationErr,
		))
	}
	return generation, nil
}

func (g *Git) addWorktree(path, branch string, createBranch bool) error {
	args := []string{"worktree", "add"}
	if createBranch {
		base, err := g.defaultWorktreeBase()
		if err != nil {
			return err
		}
		args = append(args, "-b", branch, path, base)
	} else {
		args = append(args, path, branch)
	}
	if _, err := g.run(args...); err != nil {
		return fmt.Errorf("failed to add worktree: %w", err)
	}
	return nil
}

// AddWorktreeExisting checks out an existing branch without allowing the
// checkout to run repository-configured hooks or filters, and without exposing
// protected credentials to Git.
func (g *Git) AddWorktreeExisting(
	path, branch string,
	protectedNames []string,
) error {
	_, err := g.addWorktreeExistingLocked(
		path,
		branch,
		protectedNames,
		false,
	)
	return err
}

// AddWorktreeExistingWithGeneration checks out an existing local branch and
// returns its durable generation from the same mutation-locked operation.
func (g *Git) AddWorktreeExistingWithGeneration(
	path, branch string,
	protectedNames []string,
) (string, error) {
	return g.addWorktreeExistingLocked(
		path,
		branch,
		protectedNames,
		true,
	)
}

func (g *Git) addWorktreeExistingLocked(
	path, branch string,
	protectedNames []string,
	requireGeneration bool,
) (string, error) {
	var generation string
	err := g.withWorktreeMutationLock(protectedNames, func() error {
		if err := g.rejectActiveWorktreeCreation(protectedNames); err != nil {
			return err
		}
		if err := g.addWorktreeExisting(path, branch, protectedNames); err != nil {
			return err
		}
		if !requireGeneration {
			return nil
		}
		var err error
		generation, err = g.WorktreeGeneration(path)
		if err != nil {
			return markWorktreeCreated(fmt.Errorf(
				"worktree created at %s but its generation is unavailable; preserved: %w",
				path,
				err,
			))
		}
		return nil
	})
	return generation, err
}

func (g *Git) addWorktreeExisting(
	path, branch string,
	protectedNames []string,
) error {
	if err := g.validateLocalBranchName(branch, protectedNames); err != nil {
		return err
	}
	hooksDir, err := os.MkdirTemp("", "kwt-empty-hooks-")
	if err != nil {
		return fmt.Errorf("create empty hooks directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(hooksDir) }()

	isolationArgs, err := g.checkoutIsolationArgs(
		protectedNames,
		"",
		hooksDir,
	)
	if err != nil {
		return err
	}
	if err := g.rejectRegisteredWorktreeDestination(
		path,
		protectedNames,
	); err != nil {
		return err
	}
	args := append([]string(nil), isolationArgs...)
	args = append(
		args,
		"worktree", "add", "--no-checkout", "--detach",
		"--", path, "refs/heads/"+branch,
	)
	if _, err := g.runWithoutCredentials(protectedNames, args...); err != nil {
		return fmt.Errorf("failed to add existing-branch worktree: %w", err)
	}
	if err := g.checkoutIsolatedWorktree(
		path,
		branch,
		protectedNames,
		hooksDir,
	); err != nil {
		if cleanupErr := g.removeIsolatedWorktree(
			path,
			protectedNames,
			isolationArgs,
		); cleanupErr != nil {
			return markWorktreeCreated(fmt.Errorf(
				"failed to check out existing-branch worktree: %w (failed to remove incomplete worktree: %v)",
				err,
				cleanupErr,
			))
		}
		return fmt.Errorf("failed to check out existing-branch worktree: %w", err)
	}
	return nil
}

// AddWorktreeTracking creates a local branch and worktree that track a
// specific remote branch.
func (g *Git) AddWorktreeTracking(
	path, branch, remoteBranch string,
	protectedNames []string,
) error {
	_, err := g.addWorktreeTrackingLocked(
		path,
		branch,
		remoteBranch,
		protectedNames,
		false,
	)
	return err
}

// AddWorktreeTrackingWithGeneration creates a tracking worktree and returns
// its durable generation from the same mutation-locked operation.
func (g *Git) AddWorktreeTrackingWithGeneration(
	path, branch, remoteBranch string,
	protectedNames []string,
) (string, error) {
	return g.addWorktreeTrackingLocked(
		path,
		branch,
		remoteBranch,
		protectedNames,
		true,
	)
}

func (g *Git) addWorktreeTrackingLocked(
	path, branch, remoteBranch string,
	protectedNames []string,
	requireGeneration bool,
) (string, error) {
	var generation string
	err := g.withWorktreeMutationLock(protectedNames, func() error {
		if err := g.rejectActiveWorktreeCreation(protectedNames); err != nil {
			return err
		}
		if err := g.addWorktreeTracking(
			path,
			branch,
			remoteBranch,
			protectedNames,
		); err != nil {
			return err
		}
		if !requireGeneration {
			return nil
		}
		var err error
		generation, err = g.WorktreeGeneration(path)
		if err != nil {
			return markWorktreeCreated(fmt.Errorf(
				"worktree created at %s but its generation is unavailable; preserved: %w",
				path,
				err,
			))
		}
		return nil
	})
	return generation, err
}

func (g *Git) addWorktreeTracking(
	path, branch, remoteBranch string,
	protectedNames []string,
) error {
	if err := g.validateLocalBranchName(branch, protectedNames); err != nil {
		return err
	}
	hooksDir, err := os.MkdirTemp("", "kwt-empty-hooks-")
	if err != nil {
		return fmt.Errorf("create empty hooks directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(hooksDir) }()
	isolationArgs, err := g.checkoutIsolationArgs(
		protectedNames,
		"",
		hooksDir,
	)
	if err != nil {
		return err
	}

	if err := g.rejectRegisteredWorktreeDestination(
		path,
		protectedNames,
	); err != nil {
		return err
	}
	reusedBranch, err := g.reusableTrackingBranch(
		branch,
		remoteBranch,
		protectedNames,
		isolationArgs,
	)
	if err != nil {
		return err
	}
	createdBranch := false
	if !reusedBranch {
		if _, err := g.runWithoutCredentials(
			protectedNames,
			append(
				append([]string(nil), isolationArgs...),
				"branch", "--track", "--", branch, remoteBranch,
			)...,
		); err != nil {
			return fmt.Errorf(
				"failed to create branch tracking %s: %w",
				remoteBranch,
				err,
			)
		}
		createdBranch = true
	}
	worktreeArgs := append([]string(nil), isolationArgs...)
	worktreeArgs = append(
		worktreeArgs,
		"worktree", "add", "--no-checkout", "--", path, branch,
	)
	if _, err := g.runWithoutCredentials(
		protectedNames,
		worktreeArgs...,
	); err != nil {
		if !createdBranch {
			return fmt.Errorf(
				"failed to add worktree tracking %s: %w",
				remoteBranch,
				err,
			)
		}
		if _, rollbackErr := g.runWithoutCredentials(
			protectedNames,
			append(
				append([]string(nil), isolationArgs...),
				"branch", "-D", "--", branch,
			)...,
		); rollbackErr != nil {
			return fmt.Errorf(
				"failed to add worktree tracking %s: %w (failed to remove branch %s: %v)",
				remoteBranch,
				err,
				branch,
				rollbackErr,
			)
		}
		return fmt.Errorf(
			"failed to add worktree tracking %s: %w",
			remoteBranch,
			err,
		)
	}
	if err := g.checkoutIsolatedWorktree(
		path,
		"",
		protectedNames,
		hooksDir,
	); err != nil {
		if cleanupErr := g.removeIsolatedWorktree(
			path,
			protectedNames,
			isolationArgs,
		); cleanupErr != nil {
			return markWorktreeCreated(fmt.Errorf(
				"failed to check out worktree tracking %s: %w (failed to remove incomplete worktree: %v)",
				remoteBranch,
				err,
				cleanupErr,
			))
		}
		if createdBranch {
			if _, cleanupErr := g.runWithoutCredentials(
				protectedNames,
				append(
					append([]string(nil), isolationArgs...),
					"branch", "-D", "--", branch,
				)...,
			); cleanupErr != nil {
				return fmt.Errorf(
					"failed to check out worktree tracking %s: %w (failed to remove branch %s: %v)",
					remoteBranch,
					err,
					branch,
					cleanupErr,
				)
			}
		}
		return fmt.Errorf(
			"failed to check out worktree tracking %s: %w",
			remoteBranch,
			err,
		)
	}
	return nil
}

func (g *Git) rejectRegisteredWorktreeDestination(
	path string,
	protectedNames []string,
) error {
	output, err := g.runWithoutCredentials(
		protectedNames,
		"worktree",
		"list",
		"--porcelain",
	)
	if err != nil {
		return err
	}
	want := comparableWorktreePath(path)
	for _, entry := range gitworktree.ParsePorcelain(output) {
		if comparableWorktreePath(entry.Path) != want {
			continue
		}
		if _, generationErr := g.readWorktreeGeneration(path); generationErr != nil {
			return fmt.Errorf(
				"worktree %s is already registered without a generation; refusing to replace it",
				path,
			)
		}
		return fmt.Errorf("worktree %s is already registered", path)
	}
	return nil
}

func (g *Git) reusableTrackingBranch(
	branch string,
	remoteBranch string,
	protectedNames []string,
	isolationArgs []string,
) (bool, error) {
	fullBranch := "refs/heads/" + branch
	expectedSource := remoteBranch
	if !strings.HasPrefix(expectedSource, "refs/") {
		expectedSource = "refs/remotes/" + expectedSource
	}
	args := append([]string(nil), isolationArgs...)
	args = append(
		args,
		"for-each-ref",
		"--format=%(refname)%00%(objectname)%00%(upstream)",
		"--",
		fullBranch,
		expectedSource,
	)
	output, err := g.runWithoutCredentials(protectedNames, args...)
	if err != nil {
		return false, fmt.Errorf("inspect local tracking branch: %w", err)
	}
	output = strings.TrimSpace(output)
	if output == "" {
		return false, nil
	}
	var localOID string
	var upstream string
	var sourceOID string
	for line := range strings.SplitSeq(output, "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			return false, fmt.Errorf("inspect local tracking branch: invalid response")
		}
		switch parts[0] {
		case fullBranch:
			localOID = parts[1]
			upstream = parts[2]
		case expectedSource:
			sourceOID = parts[1]
		}
	}
	if localOID == "" {
		return false, nil
	}
	if upstream != expectedSource {
		return false, fmt.Errorf(
			"local branch %s already exists with a different upstream",
			branch,
		)
	}
	if sourceOID == "" || localOID != sourceOID {
		return false, fmt.Errorf(
			"local branch %s points to a different commit than %s",
			branch,
			expectedSource,
		)
	}
	return true, nil
}

func (g *Git) validateLocalBranchName(
	branch string,
	protectedNames []string,
) error {
	if _, err := g.runWithoutCredentials(
		protectedNames,
		"check-ref-format", "--branch", branch,
	); err != nil {
		return fmt.Errorf("invalid local branch name %q: %w", branch, err)
	}
	return nil
}

func (g *Git) removeIsolatedWorktree(
	path string,
	protectedNames []string,
	isolationArgs []string,
) error {
	args := append([]string(nil), isolationArgs...)
	args = append(args, "worktree", "remove", "--force", "--", path)
	if _, err := g.runWithoutCredentials(protectedNames, args...); err != nil {
		return err
	}
	return nil
}

func (g *Git) checkoutIsolationArgs(
	protectedNames []string,
	workDir string,
	hooksDir string,
) ([]string, error) {
	configArgs := make([]string, 0, 5)
	if workDir != "" {
		configArgs = append(configArgs, "-C", workDir)
	}
	configArgs = append(configArgs, "-c", "submodule.recurse=false")
	configArgs = append(configArgs, "config", "--null", "--list")
	output, err := g.runWithoutCredentials(protectedNames, configArgs...)
	if err != nil {
		return nil, fmt.Errorf("list configured checkout isolation: %w", err)
	}
	drivers := make(map[string]bool)
	hooks := make(map[string]bool)
	for record := range strings.SplitSeq(output, "\x00") {
		key, _, _ := strings.Cut(record, "\n")
		key = strings.TrimSpace(key)
		lowerKey := strings.ToLower(key)
		switch {
		case strings.HasPrefix(lowerKey, "filter."):
			rest := key[len("filter."):]
			propertyAt := strings.LastIndex(rest, ".")
			if propertyAt <= 0 {
				continue
			}
			switch strings.ToLower(rest[propertyAt+1:]) {
			case "smudge", "process", "required":
				drivers[rest[:propertyAt]] = true
			}
		case strings.HasPrefix(lowerKey, "hook."):
			rest := key[len("hook."):]
			propertyAt := strings.LastIndex(rest, ".")
			if propertyAt <= 0 {
				continue
			}
			switch strings.ToLower(rest[propertyAt+1:]) {
			case "command", "enabled", "event", "parallel":
				hooks[rest[:propertyAt]] = true
			}
		}
	}
	hookNames := make([]string, 0, len(hooks))
	for hook := range hooks {
		hookNames = append(hookNames, hook)
	}
	sort.Strings(hookNames)
	driverNames := make([]string, 0, len(drivers))
	for driver := range drivers {
		driverNames = append(driverNames, driver)
	}
	sort.Strings(driverNames)

	args := make([]string, 0, 6+len(hookNames)*2+len(driverNames)*6)
	args = append(
		args,
		"-c", "core.hooksPath="+hooksDir,
		"-c", "core.fsmonitor=false",
		"-c", "submodule.recurse=false",
	)
	for _, hook := range hookNames {
		args = append(args, "-c", "hook."+hook+".enabled=false")
	}
	for _, driver := range driverNames {
		prefix := "filter." + driver + "."
		args = append(
			args,
			"-c", prefix+"smudge=cat",
			"-c", prefix+"process=",
			"-c", prefix+"required=false",
		)
	}
	return args, nil
}

func (g *Git) checkoutIsolatedWorktree(
	path string,
	branch string,
	protectedNames []string,
	hooksDir string,
) error {
	isolationArgs, err := g.checkoutIsolationArgs(
		protectedNames,
		path,
		hooksDir,
	)
	if err != nil {
		return err
	}
	args := []string{"-C", path}
	args = append(args, isolationArgs...)
	args = append(args, "checkout", "--force", "--no-recurse-submodules")
	if branch != "" {
		args = append(args, branch, "--")
	}
	if _, err := g.runWithoutCredentials(protectedNames, args...); err != nil {
		return err
	}
	return nil
}

func (g *Git) defaultWorktreeBase() (string, error) {
	remoteBase, remoteErr := g.remoteDefaultWorktreeBase()
	if remoteErr == nil {
		return remoteBase, nil
	}
	for _, branch := range []string{"main", "master"} {
		ref := "refs/heads/" + branch
		if g.refExists(ref) {
			return ref, nil
		}
	}
	root, rootErr := g.getMainRepoRoot()
	if rootErr == nil {
		output, branchErr := g.run(
			"-C", root, "symbolic-ref", "--quiet", "--short", "HEAD",
		)
		if branchErr == nil {
			ref := "refs/heads/" + strings.TrimSpace(output)
			if g.refExists(ref) {
				return ref, nil
			}
		}
	}
	return "", fmt.Errorf(
		"could not resolve default worktree base: remote default unavailable (%v); no local main, master, or primary worktree branch",
		remoteErr,
	)
}

func (g *Git) remoteDefaultWorktreeBase() (string, error) {
	const ref = "refs/kwt/origin/default"
	if _, err := g.run("fetch", "origin", "+HEAD:"+ref); err != nil {
		return "", fmt.Errorf("fetch origin default branch: %w", err)
	}
	if !g.refExists(ref) {
		return "", fmt.Errorf("fetched origin default ref does not exist")
	}
	return ref, nil
}

func (g *Git) refExists(ref string) bool {
	_, err := g.run("show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// AddWorktreeFromBase creates a new worktree with a branch from a specific base branch.
func (g *Git) AddWorktreeFromBase(path, branch, baseBranch string) error {
	reservation, err := g.reserveWorktreeCreation(path, nil)
	if err != nil {
		return err
	}
	if err := g.addWorktreeFromBase(path, branch, baseBranch); err != nil {
		_ = reservation.release()
		return err
	}
	_ = g.initializeWorktreeGeneration(path, nil)
	_ = reservation.release()
	return nil
}

func (g *Git) addWorktreeFromBase(path, branch, baseBranch string) error {
	args := []string{"worktree", "add", "-b", branch, path}
	if baseBranch != "" {
		args = append(args, baseBranch)
	}
	if _, err := g.run(args...); err != nil {
		return fmt.Errorf("failed to add worktree from base branch %s: %w", baseBranch, err)
	}
	return nil
}

// RemoveWorktree removes a worktree.
func (g *Git) RemoveWorktree(
	path string,
	force bool,
	ifGeneration string,
) error {
	return g.withWorktreeMutationLock(nil, func() error {
		if err := g.rejectActiveWorktreeCreation(nil); err != nil {
			return err
		}
		if ifGeneration != "" {
			if err := g.requireWorktreeGeneration(path, ifGeneration); err != nil {
				return err
			}
		}
		return g.removeWorktree(path, force, ifGeneration != "")
	})
}

// WithWorktreeGeneration runs operation while the selected worktree still
// names expected and repository worktree mutations are excluded.
func (g *Git) WithWorktreeGeneration(
	path string,
	expected string,
	operation func() error,
) error {
	return g.withWorktreeMutationLock(nil, func() error {
		if err := g.rejectActiveWorktreeCreation(nil); err != nil {
			return err
		}
		if err := g.requireWorktreeGeneration(path, expected); err != nil {
			return err
		}
		return operation()
	})
}

func (g *Git) removeWorktree(path string, force bool, conditional bool) error {
	canonicalPath := utils.CanonicalPath(path)
	registryGit := g
	if mainRoot, err := g.getMainRepoRoot(); err == nil {
		registryGit = New(mainRoot)
	}
	wasRegistered, _ := registryGit.hasRegisteredWorktree(canonicalPath)

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := registryGit.run(args...); err != nil {
		stillRegistered, listErr := registryGit.hasRegisteredWorktree(canonicalPath)
		if wasRegistered && listErr == nil && !stillRegistered {
			if conditional {
				return &incompleteWorktreeRemovalError{
					path:  path,
					cause: err,
				}
			}
			if removeErr := os.RemoveAll(path); removeErr != nil {
				return &incompleteWorktreeRemovalError{
					path: path,
					cause: errors.Join(
						err,
						fmt.Errorf(
							"remove residual worktree directory: %w",
							removeErr,
						),
					),
				}
			}
			return nil
		}
		return fmt.Errorf("failed to remove worktree: %w", err)
	}
	return nil
}

func (g *Git) requireWorktreeGeneration(path string, expected string) error {
	generation, err := g.readWorktreeGeneration(path)
	if err != nil {
		return fmt.Errorf("worktree generation changed for %s", path)
	}
	if generation != expected {
		return fmt.Errorf("worktree generation changed for %s", path)
	}
	return nil
}

func (g *Git) initializeWorktreeGeneration(
	path string,
	protectedNames []string,
) error {
	_, err := g.initializeWorktreeGenerationValue(path, protectedNames)
	return err
}

func (g *Git) initializeWorktreeGenerationValue(
	path string,
	protectedNames []string,
) (string, error) {
	var generation string
	err := g.withWorktreeMutationLock(protectedNames, func() error {
		var err error
		generation, err = g.WorktreeGeneration(path)
		if err != nil {
			return fmt.Errorf("initialize worktree generation: %w", err)
		}
		return nil
	})
	return generation, err
}

// WorktreeGeneration returns a durable identity stored in the worktree's Git
// administrative directory. Git removes that directory with the worktree, so
// recreating the same path receives a different identity.
func (g *Git) WorktreeGeneration(path string) (string, error) {
	if generation, err := g.readWorktreeGeneration(path); err == nil {
		return generation, nil
	}

	generationBytes := make([]byte, 16)
	if _, err := rand.Read(generationBytes); err != nil {
		return "", fmt.Errorf("generate worktree identity: %w", err)
	}
	generation := hex.EncodeToString(generationBytes)
	gitDir, err := g.worktreeGitDir(path)
	if err != nil {
		return "", err
	}
	generationPath := filepath.Join(gitDir, "kwt-generation")
	file, err := os.CreateTemp(gitDir, ".kwt-generation-")
	if err != nil {
		return "", fmt.Errorf("create worktree identity temporary file: %w", err)
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("secure worktree identity: %w", err)
	}
	if _, err := fmt.Fprintln(file, generation); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("persist worktree identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("sync worktree identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("persist worktree identity: %w", err)
	}
	if err := os.Rename(tempPath, generationPath); err != nil {
		return "", fmt.Errorf("install worktree identity: %w", err)
	}
	return g.readWorktreeGeneration(path)
}

// ReadWorktreeGeneration returns an existing durable identity without
// initializing one. Recovery can finalize a present identity without making
// a generation-less registered worktree eligible for checkout or cleanup.
func (g *Git) ReadWorktreeGeneration(path string) (string, error) {
	return g.readWorktreeGeneration(path)
}

func (g *Git) readWorktreeGeneration(path string) (string, error) {
	gitDir, err := g.worktreeGitDir(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "kwt-generation"))
	if err != nil {
		return "", fmt.Errorf("read worktree identity: %w", err)
	}
	generation := strings.TrimSpace(string(data))
	if err := ValidateWorktreeGeneration(generation); err != nil {
		return "", fmt.Errorf("read worktree identity: invalid generation")
	}
	return generation, nil
}

// ValidateWorktreeGeneration rejects values that cannot identify one persisted
// kwt worktree registration.
func ValidateWorktreeGeneration(generation string) error {
	decoded, err := hex.DecodeString(generation)
	if err != nil || len(decoded) != 16 {
		return fmt.Errorf(
			"worktree generation must be a 32-character hexadecimal value",
		)
	}
	return nil
}

func (g *Git) worktreeGitDir(path string) (string, error) {
	commonDir, commonDirErr := g.worktreeCommonDir(nil)
	if commonDirErr != nil {
		return "", fmt.Errorf(
			"resolve worktree Git directory: %w",
			commonDirErr,
		)
	}

	dotGitPath := filepath.Join(path, ".git")
	info, err := os.Stat(dotGitPath)
	if err == nil {
		if info.IsDir() {
			if utils.PathKey(dotGitPath) == utils.PathKey(commonDir) {
				return commonDir, nil
			}
			return "", fmt.Errorf(
				"resolve worktree Git directory: %s belongs to a different repository",
				path,
			)
		}
		data, readErr := os.ReadFile(dotGitPath)
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
					if worktreeGitDirBelongsToCommon(gitDir, commonDir) {
						return gitDir, nil
					}
					return "", fmt.Errorf(
						"resolve worktree Git directory: %s belongs to a different repository",
						path,
					)
				}
			}
		}
	}

	entries, readDirErr := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if readDirErr != nil {
		return "", fmt.Errorf("resolve worktree Git directory: %w", readDirErr)
	}
	canonicalPath := comparableWorktreePath(path)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		adminDir := filepath.Join(commonDir, "worktrees", entry.Name())
		gitDirFile, readErr := os.ReadFile(filepath.Join(adminDir, "gitdir"))
		if readErr != nil {
			continue
		}
		registeredDotGit := strings.TrimSpace(string(gitDirFile))
		if !filepath.IsAbs(registeredDotGit) {
			registeredDotGit = filepath.Join(adminDir, registeredDotGit)
		}
		registeredPath := filepath.Dir(registeredDotGit)
		if comparableWorktreePath(registeredPath) == canonicalPath {
			return adminDir, nil
		}
	}
	return "", fmt.Errorf("resolve worktree Git directory: worktree not found")
}

func worktreeGitDirBelongsToCommon(gitDir string, commonDir string) bool {
	if utils.PathKey(gitDir) == utils.PathKey(commonDir) {
		return true
	}
	return utils.PathKey(filepath.Dir(gitDir)) ==
		utils.PathKey(filepath.Join(commonDir, "worktrees"))
}

func comparableWorktreePath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return utils.PathKey(path)
	}
	return utils.PathKey(filepath.Join(
		utils.CanonicalPath(filepath.Dir(path)),
		filepath.Base(path),
	))
}

func (g *Git) reserveWorktreeCreation(
	path string,
	protectedNames []string,
) (*worktreeCreationReservation, error) {
	commonDir, err := g.worktreeCommonDir(protectedNames)
	if err != nil {
		return nil, err
	}
	creationLock := flock.New(
		filepath.Join(commonDir, worktreeCreationLockName),
		flock.SetPermissions(0600),
	)
	recordPath := filepath.Join(commonDir, worktreeCreationReservationName)
	var reservation *worktreeCreationReservation
	err = g.withWorktreeMutationLock(protectedNames, func() error {
		// Try rather than wait while holding the mutation lock: the active
		// checkout's hook may be waiting to acquire that mutation lock for a
		// reservation-aware list.
		locked, lockErr := creationLock.TryLock()
		if lockErr != nil {
			return fmt.Errorf("reserve worktree creation: %w", lockErr)
		}
		if !locked {
			return fmt.Errorf("worktree creation already in progress")
		}
		_ = os.Remove(recordPath)
		if writeErr := os.WriteFile(
			recordPath,
			[]byte(path+"\n"),
			0600,
		); writeErr != nil {
			_ = creationLock.Unlock()
			return fmt.Errorf("record worktree creation: %w", writeErr)
		}
		reservation = &worktreeCreationReservation{
			lock:       creationLock,
			recordPath: recordPath,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

func (r *worktreeCreationReservation) release() error {
	removeErr := os.Remove(r.recordPath)
	if os.IsNotExist(removeErr) {
		removeErr = nil
	}
	unlockErr := r.lock.Unlock()
	if removeErr != nil {
		return fmt.Errorf("clear worktree creation reservation: %w", removeErr)
	}
	if unlockErr != nil {
		return fmt.Errorf("release worktree creation reservation: %w", unlockErr)
	}
	return nil
}

func (g *Git) activeWorktreeCreation(
	protectedNames []string,
) (string, bool, error) {
	commonDir, err := g.worktreeCommonDir(protectedNames)
	if err != nil {
		return "", false, err
	}
	creationLock := flock.New(
		filepath.Join(commonDir, worktreeCreationLockName),
		flock.SetPermissions(0600),
	)
	available, err := creationLock.TryLock()
	if err != nil {
		return "", false, fmt.Errorf("inspect worktree creation: %w", err)
	}
	recordPath := filepath.Join(commonDir, worktreeCreationReservationName)
	if available {
		_ = creationLock.Unlock()
		_ = os.Remove(recordPath)
		return "", false, nil
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return "", true, fmt.Errorf("worktree creation in progress")
	}
	path := strings.TrimSpace(string(data))
	if path == "" {
		return "", true, fmt.Errorf("worktree creation in progress")
	}
	return path, true, nil
}

func (g *Git) rejectActiveWorktreeCreation(
	protectedNames []string,
) error {
	_, active, err := g.activeWorktreeCreation(protectedNames)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("worktree creation in progress")
	}
	return nil
}

func (g *Git) worktreeCommonDir(
	protectedNames []string,
) (string, error) {
	commonDirOutput, err := g.runWithoutCredentials(
		protectedNames,
		"rev-parse",
		"--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf("resolve worktree mutation lock: %w", err)
	}
	commonDir := strings.TrimSpace(commonDirOutput)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(g.workDir, commonDir)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(commonDir); resolveErr == nil {
		commonDir = resolved
	}
	return commonDir, nil
}

func (g *Git) withWorktreeMutationLock(
	protectedNames []string,
	operation func() error,
) error {
	commonDir, err := g.worktreeCommonDir(protectedNames)
	if err != nil {
		return err
	}
	lock := flock.New(
		filepath.Join(commonDir, worktreeMutationLockName),
		flock.SetPermissions(0600),
	)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock worktree mutations: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	return operation()
}

func (g *Git) hasRegisteredWorktree(canonicalPath string) (bool, error) {
	output, err := g.run("worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	for _, entry := range gitworktree.ParsePorcelain(output) {
		if utils.CanonicalPath(entry.Path) == canonicalPath {
			return true, nil
		}
	}
	return false, nil
}

// PruneWorktrees removes worktree information for deleted directories.
func (g *Git) PruneWorktrees() error {
	return g.withWorktreeMutationLock(nil, func() error {
		if err := g.rejectActiveWorktreeCreation(nil); err != nil {
			return err
		}
		if _, err := g.run("worktree", "prune"); err != nil {
			return fmt.Errorf("failed to prune worktrees: %w", err)
		}
		return nil
	})
}
