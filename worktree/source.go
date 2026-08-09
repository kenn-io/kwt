package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/pullrequest"
	"go.kenn.io/kwt/internal/tmux"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	internalworktree "go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
)

const maxDashboardRepositoryScans = 8

type repositoryScanResult struct {
	entries []Entry
	err     error
}

type SourceOptions struct {
	Home string
}

type currentSource struct {
	home string
}

func NewSource(options SourceOptions) Source {
	return &currentSource{home: options.Home}
}

func (s *currentSource) Load(ctx context.Context, request Request) (result Result, err error) {
	defer func() {
		err = classifyInventoryError(err)
	}()
	if err := request.validate(); err != nil {
		return Result{}, service.NewError(service.InvalidRequest, err.Error(), false, nil, err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	expansion := request.Expansion
	snapshot, err := config.LoadGlobalSnapshotAtWithExpansion(s.home, expansion.expandPath)
	if err != nil {
		return Result{}, err
	}
	result = Result{Snapshot: Snapshot{
		Config:     snapshot.Config,
		Workspaces: append([]models.Workspace(nil), snapshot.Config.Workspaces...),
	}}
	protectedNames := credentials.ProtectedNames(snapshot.Config)

	switch request.View {
	case ViewProjects:
		result.Snapshot.Projects, err = CanonicalProjects(ctx, snapshot.Config.Projects, protectedNames...)
		return result, err
	case ViewGlobal:
		result.Snapshot.Projects, err = CanonicalProjects(ctx, snapshot.Config.Projects, protectedNames...)
		if err == nil {
			result.Snapshot.Entries, err = s.loadGlobal(ctx, snapshot.Config)
		}
	case ViewRepository:
		result, err = s.loadRepository(ctx, request, snapshot.Config, expansion.expandPath)
	case ViewDashboard:
		result.Snapshot.Projects, err = CanonicalProjects(ctx, snapshot.Config.Projects, protectedNames...)
		if err == nil {
			result.Snapshot.Entries, result.Snapshot.LaunchEntries, err =
				s.loadDashboard(ctx, request, snapshot.Config)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if request.IncludeProtectedSockets {
		if err := s.annotateProtectedSockets(ctx, result.Snapshot.Entries); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

func classifyInventoryError(err error) error {
	if err == nil {
		return nil
	}
	var typed *service.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return service.NewError(
			service.Busy, "inventory refresh timed out", true, nil, err,
		)
	}
	if errors.Is(err, context.Canceled) {
		return service.NewError(
			service.Busy, "inventory refresh canceled", true, nil, err,
		)
	}
	return service.NewError(
		service.Internal, boundedDiagnostic(err), false, nil, err,
	)
}

func (s *currentSource) ApproveConfig(_ context.Context, approval ConfigApproval) error {
	return config.ApproveWorkingDirectory(config.Approval{
		Home: s.home, Path: approval.Path, Digest: approval.Digest,
	})
}

func (s *currentSource) loadRepository(
	ctx context.Context,
	request Request,
	global *models.Config,
	expandPath func(string) (string, error),
) (Result, error) {
	policy := config.RequireInteraction
	if request.UntrustedConfig == IgnoreUntrustedConfig {
		policy = config.IgnoreUntrusted
	}
	resolved, err := config.ResolveWorkingDirectory(config.ResolveRequest{
		Home: s.home, WorkingDirectory: request.WorkingDirectory, UntrustedPolicy: policy,
		ExpandPath: expandPath,
	})
	if err != nil {
		var trust *config.TrustRequiredError
		if errors.As(err, &trust) {
			return Result{}, service.NewError(
				service.InteractionRequired,
				"repository configuration trust is required",
				false,
				map[string]any{
					"kind": "repository_config_trust", "path": trust.Path,
					"digest": trust.Digest, "size": trust.Size,
					"preview": trust.Preview, "truncated": trust.Truncated,
				},
				err,
			)
		}
		return Result{}, err
	}
	if request.ForceGlobal {
		entries, loadErr := s.loadGlobal(ctx, resolved.Config)
		projects, projectsErr := CanonicalProjects(
			ctx,
			resolved.Config.Projects,
			credentials.ProtectedNames(resolved.Config)...,
		)
		if loadErr == nil {
			loadErr = projectsErr
		}
		return Result{Snapshot: Snapshot{
			Projects: projects, Entries: entries,
			Workspaces: append([]models.Workspace(nil), resolved.Config.Workspaces...),
		}, Notes: publicNotes(resolved.Notes)}, loadErr
	}

	workingDirectory, err := filepath.EvalSymlinks(request.WorkingDirectory)
	if err != nil {
		return Result{}, fmt.Errorf("resolve repository path: %w", err)
	}
	isRepository, err := hasGitMarker(workingDirectory)
	if err != nil {
		return Result{}, err
	}
	if !isRepository {
		entries, loadErr := s.loadGlobal(ctx, resolved.Config)
		projects, projectsErr := CanonicalProjects(
			ctx,
			resolved.Config.Projects,
			credentials.ProtectedNames(resolved.Config)...,
		)
		if loadErr == nil {
			loadErr = projectsErr
		}
		return Result{Snapshot: Snapshot{
			Projects: projects, Entries: entries,
			Workspaces: append([]models.Workspace(nil), resolved.Config.Workspaces...),
		}, Notes: publicNotes(resolved.Notes)}, loadErr
	}

	protectedNames := credentials.ProtectedNames(resolved.Config)
	g := git.NewForInventory(ctx, workingDirectory, protectedNames)
	worktrees, listErr := g.ListWorktrees()
	if listErr != nil {
		return Result{}, listErr
	}
	info, infoErr := internalworktree.RepositoryInfoWithProjects(g, resolved.Config.Projects)
	if infoErr != nil {
		return Result{}, infoErr
	}
	entries := make([]Entry, 0, len(worktrees))
	for _, worktree := range worktrees {
		entries = append(entries, modelEntry(worktree, "", info))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	projects, err := CanonicalProjects(ctx, resolved.Config.Projects, protectedNames...)
	if err != nil {
		return Result{}, err
	}
	return Result{Snapshot: Snapshot{
		Projects: projects, Entries: entries,
		Workspaces: append([]models.Workspace(nil), resolved.Config.Workspaces...),
	}, Notes: publicNotes(resolved.Notes)}, nil
}

func hasGitMarker(path string) (bool, error) {
	current := filepath.Clean(path)
	for {
		_, err := os.Lstat(filepath.Join(current, ".git"))
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, os.ErrNotExist):
			return false, fmt.Errorf("inspect repository marker: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func (s *currentSource) loadGlobal(ctx context.Context, cfg *models.Config) ([]Entry, error) {
	entries, err := discovery.DiscoverGlobalWorktreesContext(
		ctx,
		cfg.Worktree.BaseDir,
		cfg.Projects,
		credentials.ProtectedNames(cfg)...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to discover worktrees: %w", err)
	}
	return publicEntries(entries), nil
}

func (s *currentSource) loadDashboard(
	ctx context.Context,
	request Request,
	cfg *models.Config,
) ([]Entry, []Entry, error) {
	protectedNames := credentials.ProtectedNames(cfg)
	entries, err := s.loadGlobal(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, len(cfg.Projects))
	for index, project := range cfg.Projects {
		paths[index] = project.Path
	}
	projectResults, err := scanRepositories(ctx, paths, func(
		ctx context.Context,
		path string,
	) ([]Entry, error) {
		return entriesForRepository(ctx, path, cfg.Projects, protectedNames)
	})
	if err != nil {
		return nil, nil, err
	}
	for _, result := range projectResults {
		if result.err != nil {
			if git.IsIncompleteInventory(result.err) {
				return nil, nil, result.err
			}
			continue
		}
		entries = mergeEntries(entries, result.entries)
	}
	var launchEntries []Entry
	if request.LaunchDirectory != "" {
		var loadErr error
		launchEntries, loadErr = entriesForRepository(
			ctx,
			request.LaunchDirectory,
			cfg.Projects,
			protectedNames,
		)
		if loadErr != nil {
			if errors.Is(loadErr, context.Canceled) || errors.Is(loadErr, context.DeadlineExceeded) {
				return nil, nil, loadErr
			}
			if git.IsIncompleteInventory(loadErr) {
				return nil, nil, loadErr
			}
		}
		entries = mergeEntries(entries, launchEntries)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return entries, launchEntries, nil
}

func scanRepositories(
	ctx context.Context,
	paths []string,
	scan func(context.Context, string) ([]Entry, error),
) ([]repositoryScanResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	results := make([]repositoryScanResult, len(paths))
	jobs := make(chan int, len(paths))
	for index := range paths {
		jobs <- index
	}
	close(jobs)
	workerCount := min(len(paths), maxDashboardRepositoryScans)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				results[index].entries, results[index].err = scan(ctx, paths[index])
			}
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func entriesForRepository(
	ctx context.Context,
	path string,
	projects []models.Project,
	protectedNames []string,
) ([]Entry, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	g := git.NewForInventory(ctx, path, protectedNames)
	worktrees, err := g.ListWorktrees()
	if err != nil {
		return nil, err
	}
	info, err := internalworktree.RepositoryInfoWithProjects(g, projects)
	if err != nil {
		return nil, err
	}
	repositoryURL, _ := g.GetRepositoryURL()
	entries := make([]Entry, 0, len(worktrees))
	for _, worktree := range worktrees {
		entries = append(entries, modelEntry(worktree, repositoryURL, info))
	}
	return entries, nil
}

func publicEntries(entries []*discovery.GlobalWorktreeEntry) []Entry {
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		public := Entry{
			Path: entry.Path, Branch: entry.Branch, CommitHash: entry.CommitHash,
			IsMain: entry.IsMain, CreatedAt: entry.CreatedAt, Generation: entry.Generation,
			Repository: Repository{URL: entry.RepositoryURL},
		}
		if entry.RepositoryInfo != nil {
			public.Repository.Host = entry.RepositoryInfo.Host
			public.Repository.Owner = entry.RepositoryInfo.Owner
			public.Repository.Name = entry.RepositoryInfo.Repository
			public.Repository.FullPath = entry.RepositoryInfo.FullPath
			public.SessionName = tmux.WorkspaceSessionName(entry.RepositoryInfo, entry.Branch, entry.Path)
		}
		result = append(result, public)
	}
	return result
}

func modelEntry(worktree models.Worktree, repositoryURL string, info *repositoryurl.RepositoryInfo) Entry {
	entry := Entry{
		Path: worktree.Path, Branch: worktree.Branch, CommitHash: worktree.CommitHash,
		IsMain: worktree.IsMain, CreatedAt: worktree.CreatedAt, Generation: worktree.Generation,
		Repository: Repository{URL: repositoryURL},
	}
	if info != nil {
		entry.Repository.Host = info.Host
		entry.Repository.Owner = info.Owner
		entry.Repository.Name = info.Repository
		entry.Repository.FullPath = info.FullPath
		entry.SessionName = tmux.WorkspaceSessionName(info, entry.Branch, entry.Path)
	}
	return entry
}

// CanonicalProjects returns accessible registrations with their resolved
// repository identities. It never mutates the supplied registry slice.
func CanonicalProjects(
	ctx context.Context,
	projects []models.Project,
	protectedNames ...string,
) ([]models.Project, error) {
	result := make([]models.Project, 0, len(projects))
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.TrimSpace(project.Path) == "" {
			continue
		}
		g := internalworktree.NewCachedIdentityGit(
			git.NewForInventory(ctx, project.Path, protectedNames),
		)
		mainPath, err := g.GetMainRepositoryPath()
		if err != nil || utils.PathKey(mainPath) != utils.PathKey(project.Path) {
			continue
		}
		info, err := internalworktree.RepositoryInfoWithProjects(g, []models.Project{project})
		if err != nil {
			continue
		}
		project.Repository = info.FullPath
		result = append(result, project)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func mergeEntries(existing, incoming []Entry) []Entry {
	result := append([]Entry(nil), existing...)
	seen := make(map[string]struct{}, len(result))
	for _, entry := range result {
		seen[utils.PathKey(entry.Path)] = struct{}{}
	}
	for _, entry := range incoming {
		key := utils.PathKey(entry.Path)
		if _, ok := seen[key]; ok {
			continue
		}
		result = append(result, entry)
		seen[key] = struct{}{}
	}
	return result
}

func publicNotes(notes []config.ConfigNote) []Note {
	result := make([]Note, len(notes))
	for index, note := range notes {
		result[index] = Note{Code: note.Code, Path: note.Path}
	}
	return result
}

func (s *currentSource) annotateProtectedSockets(ctx context.Context, entries []Entry) error {
	records := make(map[string]pullrequest.Provenance)
	err := pullrequest.NewFileStore(filepath.Join(s.home, "pull-requests.json")).View(
		ctx,
		func(current map[string]pullrequest.Provenance) error {
			for key, record := range current {
				records[key] = record
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("failed to read pull-request provenance: %w", err)
	}
	for index := range entries {
		for _, record := range records {
			workspace := record.Workspace
			repository := workspace.Repository
			if repository == "" {
				repository = record.Project.Identity
			}
			if utils.PathKey(workspace.Path) != utils.PathKey(entries[index].Path) ||
				workspace.Branch != entries[index].Branch ||
				workspace.SessionName != entries[index].SessionName ||
				pullrequest.NormalizeRepositoryIdentity(repository) != pullrequest.NormalizeRepositoryIdentity(entries[index].Repository.FullPath) {
				continue
			}
			if workspace.Generation != "" && workspace.Generation != entries[index].Generation {
				continue
			}
			entries[index].TmuxSocketName = tmux.ProtectedWorkspaceSocketName(workspace.SessionName, workspace.Path)
			break
		}
	}
	return nil
}
