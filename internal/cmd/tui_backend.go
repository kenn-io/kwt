package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/credentials"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/fleet"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/status"
	"go.kenn.io/kwt/internal/tmux"
	dashboard "go.kenn.io/kwt/internal/tui"
	"go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

type tuiBackend struct {
	// mu serializes List and MergeFleet: List mutates cfg (launch project and
	// workspace registration) while MergeFleet's manifest publish reads it,
	// and the TUI runs the two as concurrent commands.
	mu                        sync.Mutex
	cfg                       *models.Config
	tmux                      *tmux.TmuxCommand
	protectedNames            []string
	launchDir                 string
	launchProjectRegistered   bool
	launchWorkspaceRegistered bool
	discoverGlobalWorktrees   func(string) ([]*discovery.GlobalWorktreeEntry, error)
	discoverProjectWorktrees  func(string) ([]*discovery.GlobalWorktreeEntry, error)
	discoverLaunchWorktrees   func(string) ([]*discovery.GlobalWorktreeEntry, error)
	collectStatuses           func(context.Context, string, []*discovery.GlobalWorktreeEntry) (map[string]*models.WorktreeStatus, error)
	listSessions              func() ([]string, error)
	ensureAndAttach           func(context.Context, string, string, models.Layout, bool) error
	registerProject           func(models.Project) error
	registerWorkspace         func(models.Workspace) (models.Workspace, error)
	unregisterWorkspace       func(name string) error
	readFleetState            func(context.Context, *models.Config) (fleet.FleetState, error)
	loadTargetConfig          func(string, bool) (*models.Config, error)
	acknowledgeRemoteSource   func(string) error
	now                       func() time.Time
}

func newTUIBackend(cfg *models.Config) *tuiBackend {
	launchDir, _ := os.Getwd()
	return newTUIBackendWithLaunchDir(cfg, launchDir)
}

func newTUIBackendWithLaunchDir(cfg *models.Config, launchDir string) *tuiBackend {
	protectedNames := credentials.ProtectedNames(cfg)
	tmuxCmd := tmux.NewTmuxCommandWithStripNames("", protectedNames)
	return &tuiBackend{
		cfg:            cfg,
		tmux:           tmuxCmd,
		protectedNames: protectedNames,
		launchDir:      launchDir,
		// Registered projects flow into base-dir discovery so a registered
		// canonical identity wins over a fork origin (the same precedence
		// applyProjectIdentityFallback applies to per-project discovery).
		discoverGlobalWorktrees: func(baseDir string) ([]*discovery.GlobalWorktreeEntry, error) {
			return discovery.DiscoverGlobalWorktrees(baseDir, cfg.Projects)
		},
		discoverProjectWorktrees: discoverLaunchRepoWorktrees,
		discoverLaunchWorktrees:  discoverLaunchRepoWorktrees,
		collectStatuses:          collectTUIStatuses,
		listSessions:             tmuxCmd.ListSessions,
		ensureAndAttach: tmux.NewWorkspaceRunner(
			tmuxCmd,
			protectedNames,
		).EnsureAndAttach,
		registerProject:         config.RegisterProject,
		registerWorkspace:       config.RegisterWorkspace,
		unregisterWorkspace:     config.UnregisterWorkspace,
		readFleetState:          readTUIFleetState,
		loadTargetConfig:        config.LoadForTarget,
		acknowledgeRemoteSource: acknowledgeRemoteSourcePath,
		now:                     time.Now,
	}
}

func (b *tuiBackend) ListFast(ctx context.Context) ([]dashboard.Row, []string, error) {
	return b.list(ctx, false)
}

func (b *tuiBackend) List(ctx context.Context) ([]dashboard.Row, []string, error) {
	return b.list(ctx, true)
}

func (b *tuiBackend) list(ctx context.Context, includeStatuses bool) ([]dashboard.Row, []string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var (
		entries           []*discovery.GlobalWorktreeEntry
		registeredEntries []*discovery.GlobalWorktreeEntry
		launchEntries     []*discovery.GlobalWorktreeEntry
		sessions          []string
		discoveryErr      error
		registeredErr     error
		launchErr         error
		sessionsErr       error
		startup           sync.WaitGroup
	)
	startup.Add(4)
	go func() {
		defer startup.Done()
		entries, discoveryErr = b.discoverGlobalWorktrees(
			b.cfg.Worktree.BaseDir,
		)
	}()
	go func() {
		defer startup.Done()
		registeredEntries, registeredErr =
			b.discoverRegisteredProjectWorktrees()
	}()
	go func() {
		defer startup.Done()
		launchEntries, launchErr = b.discoverLaunchWorktrees(b.launchDir)
	}()
	go func() {
		defer startup.Done()
		sessions, sessionsErr = b.listSessions()
	}()
	startup.Wait()

	if discoveryErr != nil {
		return nil, nil, fmt.Errorf("failed to discover worktrees: %w", discoveryErr)
	}
	if registeredErr != nil {
		return nil, nil, fmt.Errorf(
			"failed to discover registered project worktrees: %w",
			registeredErr,
		)
	}
	if launchErr != nil {
		return nil, nil, fmt.Errorf("failed to discover launch repository worktrees: %w", launchErr)
	}
	if sessionsErr != nil {
		return nil, nil, sessionsErr
	}

	entries = mergeTUIEntries(entries, registeredEntries)
	b.registerLaunchProject(launchEntries)
	b.registerLaunchWorkspace(launchEntries)
	entries = mergeTUIEntries(entries, launchEntries)

	var statusByPath map[string]*models.WorktreeStatus
	if includeStatuses {
		statusByPath, discoveryErr = b.collectStatuses(
			ctx,
			b.cfg.Worktree.BaseDir,
			entries,
		)
		if discoveryErr != nil {
			return nil, nil, discoveryErr
		}
	}

	liveSessions := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		liveSessions[session] = true
	}

	rows := make([]dashboard.Row, 0, len(entries))
	for _, entry := range entries {
		st := statusByPath[entry.Path]
		if st == nil {
			st = unknownStatusForEntry(entry)
		}
		rows = append(rows, buildTUIRow(entry, st, liveSessions))
	}
	rows = append(rows, b.workspaceRows(sessions)...)
	return rows, nil, nil
}

// MergeFleet overlays hub state onto locally discovered rows. It publishes
// this host's manifest and reads the hub synchronously, so callers must keep
// it off the first-paint path and cancel ctx when the result is no longer
// wanted.
func (b *tuiBackend) MergeFleet(ctx context.Context, rows []dashboard.Row) ([]dashboard.Row, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mergeFleetRows(ctx, rows)
}

func (b *tuiBackend) mergeFleetRows(ctx context.Context, rows []dashboard.Row) ([]dashboard.Row, []string) {
	if b.cfg == nil || !b.cfg.Fleet.Enabled || b.readFleetState == nil {
		return rows, nil
	}
	state, err := b.readFleetState(ctx, b.cfg)
	if err != nil {
		return rows, nil
	}
	currentHost, err := currentFleetHostID(b.cfg)
	if err != nil {
		return rows, nil
	}
	warnings := make([]string, 0, len(state.Warnings))
	for _, warning := range state.Warnings {
		warnings = append(warnings, warning.String())
	}

	byKey := make(map[string]int, len(rows))
	for i := range rows {
		if key := dashboard.FleetKeyForRow(rows[i]); key != "" {
			byKey[key] = i
		}
	}

	now := b.now()
	for _, fleetRow := range state.Rows {
		// Local presence is decided by the rows just discovered on disk, not
		// by hub observations for this host, which can be stale when a
		// publish fails.
		rowIndex, local := byKey[dashboard.FleetKey(fleetRow.ProjectIdentity, fleetRow.Kind, fleetRowRef(fleetRow))]
		var localRow dashboard.Row
		if local {
			localRow = rows[rowIndex]
		}
		fleetRow = localAwareFleetRow(fleetRow, currentHost, local, localRow, now)
		info := dashboardFleetInfo(fleetRow, fleet.BuildStatusRow(fleetRow, currentHost, now), currentHost, local)
		if info == nil {
			continue
		}
		if local {
			rows[rowIndex].Fleet = info
			continue
		}
		if info.CanMaterialize {
			// MaterializeWorktree needs a registered local project to add
			// the worktree from; do not advertise sync without one.
			if _, ok := b.projectForFleetInfo(info); !ok {
				info.CanMaterialize = false
			}
		}
		if len(info.Hosts) == 0 {
			// Only this host's own stale observation backs the row and the
			// worktree is gone locally; nothing real to show.
			continue
		}
		rows = append(rows, dashboard.Row{Fleet: info})
	}
	return rows, warnings
}

func fleetRowRef(row fleet.FleetRow) string {
	ref := strings.TrimSpace(row.Ref)
	if ref == "" {
		ref = strings.TrimSpace(row.Branch)
	}
	return ref
}

// localAwareFleetRow rebuilds a hub row's observations around local
// discovery: this host's hub observation is dropped (it may be stale when a
// publish failed) and, when the row exists locally, replaced with what is on
// disk right now, so Sync/Dirty/Freshness reflect reality.
func localAwareFleetRow(
	row fleet.FleetRow,
	currentHost string,
	local bool,
	localRow dashboard.Row,
	now time.Time,
) fleet.FleetRow {
	observations := make([]fleet.Observation, 0, len(row.Observations)+1)
	for _, observation := range row.Observations {
		if strings.TrimSpace(observation.HostID) == currentHost {
			continue
		}
		observations = append(observations, observation)
	}
	if local {
		if observation, ok := localFleetObservation(currentHost, localRow, now); ok {
			observations = append(observations, observation)
		}
	}
	row.Observations = observations
	return row
}

func localFleetObservation(currentHost string, localRow dashboard.Row, now time.Time) (fleet.Observation, bool) {
	if localRow.Entry == nil {
		return fleet.Observation{}, false
	}
	head := strings.TrimSpace(localRow.Entry.CommitHash)
	if head == "" {
		return fleet.Observation{}, false
	}
	observation := fleet.Observation{
		HostID:     currentHost,
		Path:       localRow.Entry.Path,
		Head:       head,
		ObservedAt: now,
	}
	if localRow.Status != nil {
		observation.LastActivity = localRow.Status.LastActivity
	}
	// Status is deliberately left zero: local dirt renders from the on-disk
	// worktree status, keeping host-labeled fleet dirt scoped to other hosts.
	return observation, true
}

func (b *tuiBackend) discoverRegisteredProjectWorktrees() (
	[]*discovery.GlobalWorktreeEntry,
	error,
) {
	// Each project discovery spawns git subprocesses; run them concurrently
	// and merge in config order so results stay deterministic.
	results := make([][]*discovery.GlobalWorktreeEntry, len(b.cfg.Projects))
	projectErrors := make([]error, len(b.cfg.Projects))
	var wg sync.WaitGroup
	for i, project := range b.cfg.Projects {
		if project.Path == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			projectEntries, err := b.discoverProjectWorktrees(project.Path)
			if err != nil {
				if git.IsIncompleteInventory(err) {
					projectErrors[i] = err
				}
				return
			}
			results[i] = applyProjectIdentityFallback(projectEntries, project)
		}()
	}
	wg.Wait()

	var entries []*discovery.GlobalWorktreeEntry
	for _, projectEntries := range results {
		entries = mergeTUIEntries(entries, projectEntries)
	}
	return entries, errors.Join(projectErrors...)
}

// registerLaunchProject persists the launch repository at most once per TUI
// run. Both stages discover it, but rewriting config during each load would
// put bookkeeping back on the startup and refresh paths.
func (b *tuiBackend) registerLaunchProject(entries []*discovery.GlobalWorktreeEntry) {
	if b.launchProjectRegistered || b.registerProject == nil {
		return
	}
	project, ok := projectFromEntries(entries, b.launchDir)
	if !ok {
		return
	}
	b.launchProjectRegistered = true
	if existing, found := b.projectByPath(project.Path); found {
		if reusable, ok := reusableExistingProject(existing, project); ok {
			project = reusable
		}
	}
	if err := b.registerProject(project); err != nil {
		return
	}
	b.upsertProject(project)
}

func (b *tuiBackend) workspaceRows(sessions []string) []dashboard.Row {
	if b.cfg == nil || len(b.cfg.Workspaces) == 0 {
		return nil
	}
	rows := make([]dashboard.Row, 0, len(b.cfg.Workspaces))
	for _, workspace := range b.cfg.Workspaces {
		sessionName := tmux.DirWorkspaceSessionName(workspace.Name, workspace.Path)
		sessionLive := false
		// Match by path hash so a renamed workspace still finds (and later
		// attaches to) its live session created under the old name.
		if live, ok := tmux.MatchDirWorkspaceSession(sessions, workspace.Path); ok {
			sessionName = live
			sessionLive = true
		}
		rows = append(rows, dashboard.Row{
			Workspace:   &dashboard.WorkspaceInfo{Name: workspace.Name, Path: workspace.Path},
			SessionName: sessionName,
			SessionLive: sessionLive,
		})
	}
	return rows
}

// registerLaunchWorkspace records a non-git launch directory as a workspace,
// best-effort, mirroring launch-repo project registration. The home directory
// is never auto-registered: running kwt from ~ is common and would create a
// junk entry. Registration is attempted at most once per backend lifetime, so
// a later refresh never re-registers a workspace the user just unregistered
// in the TUI, and the global config is not rewritten on every List call.
func (b *tuiBackend) registerLaunchWorkspace(launchEntries []*discovery.GlobalWorktreeEntry) {
	if b.launchWorkspaceRegistered {
		return
	}
	b.launchWorkspaceRegistered = true

	if b.registerWorkspace == nil || b.launchDir == "" || len(launchEntries) > 0 {
		return
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(home, b.launchDir) {
		return
	}
	if launchDirAlreadyRegistered(b.cfg, b.launchDir) {
		// Preserve a custom name set via `kwt workspace add --name`: the
		// same-path branch of config.RegisterWorkspace would otherwise
		// overwrite it with the directory's defaulted base name.
		return
	}
	stored, err := b.registerWorkspace(models.Workspace{Path: b.launchDir})
	if err != nil {
		return
	}
	b.cfg.Workspaces = upsertWorkspace(b.cfg.Workspaces, stored)
}

func launchDirAlreadyRegistered(cfg *models.Config, launchDir string) bool {
	if cfg == nil {
		return false
	}
	for _, workspace := range cfg.Workspaces {
		if samePath(workspace.Path, launchDir) {
			return true
		}
	}
	return false
}

func upsertWorkspace(workspaces []models.Workspace, workspace models.Workspace) []models.Workspace {
	for i := range workspaces {
		if samePath(workspaces[i].Path, workspace.Path) {
			workspaces[i] = workspace
			return workspaces
		}
	}
	return append(workspaces, workspace)
}

func (b *tuiBackend) projectByPath(projectPath string) (models.Project, bool) {
	if b.cfg == nil || projectPath == "" {
		return models.Project{}, false
	}
	for _, project := range b.cfg.Projects {
		if project.Path != "" && samePath(project.Path, projectPath) {
			return project, true
		}
	}
	return models.Project{}, false
}

func projectFromEntries(entries []*discovery.GlobalWorktreeEntry, fallbackPath string) (models.Project, bool) {
	var info *url.RepositoryInfo
	remoteURL := ""
	projectPath := fallbackPath
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if info == nil && entry.RepositoryInfo != nil {
			info = entry.RepositoryInfo
			remoteURL = entry.RepositoryURL
		}
		if entry.IsMain && entry.Path != "" {
			projectPath = entry.Path
		}
	}
	if info == nil || projectPath == "" {
		return models.Project{}, false
	}
	info, ok := registrableProjectIdentity(info, remoteURL, projectPath)
	if !ok {
		return models.Project{}, false
	}
	repository := info.FullPath
	if repository == "" {
		repository = path.Join(info.Host, info.Owner, info.Repository)
	}
	return models.Project{
		Repository: repository,
		Name:       info.Repository,
		Path:       projectPath,
	}, true
}

// registrableProjectIdentity gates a git-derived identity behind the
// remote-provenance canonical bar before it is persisted as
// project.Repository. Stored registry identities ride the relaxed CONFIGURED
// bar (url.CanonicalRepositoryIdentity) on every later manifest build, so
// persisting raw origin-derived output would launder a relative dotless
// remote ("cache/team/repo.git" — a machine-local filesystem path git happily
// serves) into a publishable "cache/team/repo" fleet identity. When the
// remote bar rejects the origin, the canonical local-path fallback is
// persisted instead; entries without a remote URL already carry it.
func registrableProjectIdentity(
	info *url.RepositoryInfo, remoteURL, projectPath string,
) (*url.RepositoryInfo, bool) {
	if strings.TrimSpace(remoteURL) == "" {
		return info, true
	}
	if _, ok := url.CanonicalRepositoryIdentityFromRemote(remoteURL); ok {
		return info, true
	}
	localInfo, err := worktree.RepositoryInfoFromLocalPath(projectPath)
	if err != nil {
		return nil, false
	}
	return localInfo, true
}

func (b *tuiBackend) upsertProject(project models.Project) {
	if b.cfg == nil {
		return
	}
	for i := range b.cfg.Projects {
		if sameRegisteredProject(b.cfg.Projects[i], project) {
			b.cfg.Projects[i] = project
			return
		}
	}
	b.cfg.Projects = append(b.cfg.Projects, project)
}

func sameRegisteredProject(a, b models.Project) bool {
	// A project row represents one local checkout. Matching paths should share
	// a registry slot so path-fallback identities can be upgraded to remotes.
	if a.Path != "" && b.Path != "" && samePath(a.Path, b.Path) {
		return true
	}
	if a.Repository != "" && b.Repository != "" {
		return equalRepositoryIdentity(a.Repository, b.Repository)
	}
	return false
}

func reusableExistingProject(existing, discovered models.Project) (models.Project, bool) {
	// A publish-canonical identity is authoritative: manifest publishing
	// prefers project.Repository over origin, so launch registration must
	// not replace it with an origin-derived identity (e.g. a fork remote).
	// Weaker identities (path-like, local/...) never reach the hub, so an
	// origin-derived identity may upgrade them — but a path fallback must
	// not downgrade a stable one.
	if info, ok := url.CanonicalRepositoryInfo(existing.Repository); ok {
		existing.Repository = info.FullPath
		return existing, true
	}
	return existing, hasStableProjectIdentity(existing) && !hasStableProjectIdentity(discovered)
}

func hasStableProjectIdentity(project models.Project) bool {
	repository := strings.TrimSpace(project.Repository)
	return repository != "" && !url.IsPathFallbackIdentity(repository)
}

func discoverLaunchRepoWorktrees(launchDir string) ([]*discovery.GlobalWorktreeEntry, error) {
	if launchDir == "" {
		return nil, nil
	}

	g := git.New(launchDir)
	worktrees, err := g.ListWorktrees()
	if err != nil {
		if git.IsIncompleteInventory(err) {
			return nil, err
		}
		return nil, nil
	}

	rootPath := launchDir
	if repoRoot, err := g.GetMainRepositoryPath(); err == nil {
		rootPath = repoRoot
	}
	repoURL := ""
	repoInfo := repositoryInfoFromRootPath(rootPath)
	if gotURL, err := g.GetRepositoryURL(); err == nil {
		repoURL = gotURL
		// The remote-derived bar keeps a relative dotless filesystem remote
		// from surfacing as an identity; barred remotes keep the local/...
		// fallback.
		if parsedInfo, ok := url.CanonicalRepositoryInfoFromRemote(gotURL); ok {
			repoInfo = parsedInfo
		}
	}

	entries := make([]*discovery.GlobalWorktreeEntry, 0, len(worktrees))
	for _, wt := range worktrees {
		entries = append(entries, &discovery.GlobalWorktreeEntry{
			RepositoryURL:  repoURL,
			RepositoryInfo: repoInfo,
			Branch:         wt.Branch,
			Path:           wt.Path,
			CommitHash:     wt.CommitHash,
			IsMain:         wt.IsMain,
			CreatedAt:      wt.CreatedAt,
			Generation:     wt.Generation,
		})
	}
	return entries, nil
}

func applyProjectIdentityFallback(
	entries []*discovery.GlobalWorktreeEntry,
	project models.Project,
) []*discovery.GlobalWorktreeEntry {
	info := repositoryInfoFromProject(project)
	if info == nil {
		return entries
	}
	// Manifest publishing prefers the configured project.Repository over the
	// origin URL only when it is publish-canonical. Mirror that precedence
	// here so local rows and hub rows agree on identity: a canonical
	// configured identity overrides a fork origin, while path-like or
	// local/... identities never reach the hub and must not displace the
	// origin-derived identity that does.
	configuredInfo, override := url.CanonicalRepositoryInfo(project.Repository)
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		if override {
			entry.RepositoryInfo = configuredInfo
		} else if entry.RepositoryURL == "" {
			entry.RepositoryInfo = info
		}
	}
	return entries
}

func repositoryInfoFromProject(project models.Project) *url.RepositoryInfo {
	repository := strings.TrimSpace(project.Repository)
	// Path fallbacks are not URLs: parsing a canonical "local/..." identity
	// or retaining a legacy absolute identity produces a different
	// WorkspaceSessionName than the canonical local-path resolver. Reconstruct
	// every path fallback through the same resolver instead.
	if repository != "" && url.IsPathFallbackIdentity(repository) && project.Path != "" {
		if info, err := worktree.RepositoryInfoFromLocalPath(project.Path); err == nil {
			return info
		}
	}
	if repository != "" && !url.IsPathFallbackIdentity(repository) {
		if info, ok := url.CanonicalRepositoryInfo(repository); ok {
			return info
		}
	}

	name := strings.TrimSpace(project.Name)
	if name == "" && project.Path != "" {
		name = filepath.Base(project.Path)
	}
	fullPath := repository
	if fullPath == "" {
		fullPath = project.Path
	}
	if name == "" || fullPath == "" {
		return nil
	}
	return &url.RepositoryInfo{
		Repository: name,
		FullPath:   filepath.ToSlash(fullPath),
	}
}

// repositoryInfoFromRootPath builds the local fallback identity for a
// registered project's root path. It delegates to the single canonical
// resolver so a no-remote project gets the same "local/..." identity that kwt
// list and kwt list -g discovery report, keeping the JSON surfaces joinable.
func repositoryInfoFromRootPath(rootPath string) *url.RepositoryInfo {
	info, err := worktree.RepositoryInfoFromLocalPath(rootPath)
	if err != nil {
		return nil
	}
	return info
}

func mergeTUIEntries(
	globalEntries []*discovery.GlobalWorktreeEntry,
	launchEntries []*discovery.GlobalWorktreeEntry,
) []*discovery.GlobalWorktreeEntry {
	entries := append([]*discovery.GlobalWorktreeEntry(nil), globalEntries...)
	seen := make(map[string]int, len(entries))
	for i, entry := range entries {
		if entry != nil && entry.Path != "" {
			seen[entryPathKey(entry.Path)] = i
		}
	}
	for _, entry := range launchEntries {
		if entry == nil || entry.Path == "" {
			continue
		}
		key := entryPathKey(entry.Path)
		if existing, ok := seen[key]; ok {
			if shouldReplaceTUIEntry(entries[existing], entry) {
				entries[existing] = entry
			}
			continue
		}
		entries = append(entries, entry)
		seen[key] = len(entries) - 1
	}
	return entries
}

func entryPathKey(entryPath string) string {
	return cleanComparablePath(entryPath)
}

func shouldReplaceTUIEntry(
	existing *discovery.GlobalWorktreeEntry,
	incoming *discovery.GlobalWorktreeEntry,
) bool {
	if existing == nil {
		return incoming != nil
	}
	if incoming == nil {
		return false
	}
	if incoming.RepositoryURL != "" && existing.RepositoryURL == "" {
		return true
	}
	return usesPathFallbackIdentity(existing) && hasStableNonPathIdentity(incoming)
}

func usesPathFallbackIdentity(entry *discovery.GlobalWorktreeEntry) bool {
	if entry == nil || entry.RepositoryURL != "" || entry.RepositoryInfo == nil {
		return false
	}
	return url.IsPathFallbackIdentity(entry.RepositoryInfo.FullPath)
}

func hasStableNonPathIdentity(entry *discovery.GlobalWorktreeEntry) bool {
	if entry == nil || entry.RepositoryInfo == nil {
		return false
	}
	info := entry.RepositoryInfo
	if info.FullPath != "" && !url.IsPathFallbackIdentity(info.FullPath) {
		return true
	}
	return info.Host != "" && info.Repository != ""
}

func collectTUIStatuses(
	ctx context.Context,
	baseDir string,
	entries []*discovery.GlobalWorktreeEntry,
) (map[string]*models.WorktreeStatus, error) {
	worktrees := make([]*models.Worktree, 0, len(entries))
	for _, entry := range entries {
		worktrees = append(worktrees, &models.Worktree{
			Path:       entry.Path,
			Branch:     entry.Branch,
			CommitHash: entry.CommitHash,
			IsMain:     entry.IsMain,
		})
	}

	collector := status.NewStatusCollectorWithOptions(tuiStatusCollectorOptions(baseDir))
	statuses, err := collector.CollectAll(ctx, worktrees)
	if err != nil {
		return nil, err
	}
	statusByPath := make(map[string]*models.WorktreeStatus, len(statuses))
	for _, st := range statuses {
		statusByPath[st.Path] = st
	}
	return statusByPath, nil
}

func tuiStatusCollectorOptions(baseDir string) status.StatusCollectorOptions {
	return status.StatusCollectorOptions{
		FetchRemote: true,
		BaseDir:     baseDir,
	}
}

func buildTUIRow(
	entry *discovery.GlobalWorktreeEntry,
	st *models.WorktreeStatus,
	liveSessions map[string]bool,
) dashboard.Row {
	sessionName := ""
	sessionLive := false
	if entry.RepositoryInfo != nil {
		sessionName = tmux.WorkspaceSessionName(entry.RepositoryInfo, entry.Branch, entry.Path)
		sessionLive = liveSessions[sessionName]
	}
	return dashboard.Row{
		Entry:       entry,
		Status:      st,
		SessionName: sessionName,
		SessionLive: sessionLive,
	}
}

func readTUIFleetState(ctx context.Context, cfg *models.Config) (fleet.FleetState, error) {
	// Create the client first: when the hub is unusable (bad URL, missing
	// token) this returns immediately instead of paying for a manifest build
	// whose publish would fail anyway.
	client, err := newFleetClientFromConfig(cfg)
	if err != nil {
		return fleet.FleetState{}, err
	}
	if cfg != nil && cfg.Fleet.Enabled {
		var publishWarning bytes.Buffer
		_ = publishFleetBestEffort(ctx, cfg, newFleetManifestBuilder(), &publishWarning)
	}
	state, _, notModified, err := client.State(ctx, "")
	if err != nil {
		return fleet.FleetState{}, err
	}
	if notModified {
		return fleet.FleetState{}, nil
	}
	return state, nil
}

func dashboardFleetInfo(row fleet.FleetRow, rendered fleet.StatusRow, currentHost string, local bool) *dashboard.FleetInfo {
	ref := fleetRowRef(row)
	if row.ProjectIdentity == "" || row.Kind == "" || ref == "" {
		return nil
	}
	info := &dashboard.FleetInfo{
		ProjectIdentity: row.ProjectIdentity,
		ProjectName:     rendered.Project,
		Kind:            row.Kind,
		Ref:             ref,
		Branch:          strings.TrimSpace(row.Branch),
		Local:           local,
		Hosts:           fleetDisplayHosts(row.Observations, currentHost, local),
		Sync:            rendered.Sync,
		Dirty:           rendered.Dirty,
		Freshness:       rendered.Freshness,
		AllPrimary:      allRemoteFleetObservationsPrimary(row.Observations, currentHost),
	}
	if info.ProjectName == "" {
		info.ProjectName = strings.TrimSpace(row.ProjectName)
	}
	if info.ProjectName == "" {
		info.ProjectName = row.ProjectIdentity
	}
	if info.Branch == "" && row.Kind == "branch" {
		info.Branch = ref
	}
	info.MaterializeLabel = info.Branch
	if info.MaterializeLabel == "" {
		info.MaterializeLabel = info.Ref
	}
	if !info.Local {
		if observation, ok := fleetMaterializeObservation(row.Observations, currentHost); ok {
			info.MaterializeHost = observation.HostID
			info.RemotePath = observation.Path
			info.RemoteHead = observation.Head
			info.RemoteUpstream = observation.Upstream
			info.RemoteAhead = observation.Ahead
		}
		info.CanMaterialize = row.Kind == "branch" && info.Branch != ""
	}
	return info
}

func allRemoteFleetObservationsPrimary(observations []fleet.Observation, currentHost string) bool {
	found := false
	for _, observation := range observations {
		hostID := strings.TrimSpace(observation.HostID)
		if hostID == "" || hostID == currentHost {
			continue
		}
		found = true
		if !observation.IsMain {
			return false
		}
	}
	return found
}

func fleetDisplayHosts(observations []fleet.Observation, currentHost string, local bool) []string {
	seen := make(map[string]struct{}, len(observations)+1)
	for _, observation := range observations {
		hostID := strings.TrimSpace(observation.HostID)
		if hostID == "" || hostID == currentHost {
			// This host's hub observation may be stale; the local flag from
			// on-disk discovery decides whether "local" is listed.
			continue
		}
		seen[hostID] = struct{}{}
	}
	if local {
		seen["local"] = struct{}{}
	}
	hosts := make([]string, 0, len(seen))
	for hostID := range seen {
		hosts = append(hosts, hostID)
	}
	sort.Strings(hosts)
	return hosts
}

func fleetMaterializeObservation(observations []fleet.Observation, currentHost string) (fleet.Observation, bool) {
	for _, observation := range observations {
		hostID := strings.TrimSpace(observation.HostID)
		if hostID != "" && hostID != currentHost {
			return observation, true
		}
	}
	return fleet.Observation{}, false
}

// CreateWorktree resolves the destination itself rather than accepting the path
// PreviewWorktree computed. A custom destination is treated as user input and
// gets environment expansion, which would turn a repository-generated name
// containing an environment reference into the referenced value.
func (b *tuiBackend) CreateWorktree(
	ctx context.Context,
	row dashboard.Row,
	branch string,
	source string,
) (string, error) {
	if row.Entry == nil {
		return "", fmt.Errorf("no worktree selected")
	}
	manager, err := b.worktreeManager(row)
	if err != nil {
		return "", err
	}
	var path string
	switch source {
	case "":
		path, err = manager.Add(branch, "", true)
	case branch:
		path, err = manager.Add(branch, "", false)
	default:
		path, err = manager.AddTracking(branch, source, "")
	}
	if err != nil {
		return "", err
	}
	publishTUIFleetBestEffort(ctx, b.cfg)
	return path, nil
}

func (b *tuiBackend) ListBranches(
	_ context.Context,
	row dashboard.Row,
) ([]models.Branch, error) {
	if row.Entry == nil {
		return nil, fmt.Errorf("no worktree selected")
	}
	return git.New(row.Entry.Path).ListAvailableBranches()
}

func (b *tuiBackend) PreviewWorktree(row dashboard.Row, branch string) (dashboard.Row, error) {
	if row.Entry == nil {
		return dashboard.Row{}, fmt.Errorf("no worktree selected")
	}
	manager, err := b.worktreeManager(row)
	if err != nil {
		return dashboard.Row{}, err
	}
	worktreePath, err := manager.PreparePath("", branch)
	if err != nil {
		return dashboard.Row{}, err
	}

	entry := *row.Entry
	entry.Branch = branch
	entry.Path = worktreePath
	entry.CommitHash = ""
	entry.IsMain = false
	entry.CreatedAt = time.Time{}
	entry.Generation = ""
	return dashboard.Row{
		Entry:  &entry,
		Status: unknownStatusForEntry(&entry),
	}, nil
}

func (b *tuiBackend) worktreeManager(row dashboard.Row) (*worktree.Manager, error) {
	repositoryGit := git.New(row.Entry.Path)
	repoRoot, err := repositoryGit.GetMainRepositoryPath()
	if err != nil {
		return nil, fmt.Errorf("resolve selected repository root: %w", err)
	}
	cfg, err := b.loadTargetConfig(repoRoot, false)
	if err != nil {
		return nil, fmt.Errorf("load selected repository config: %w", err)
	}
	return worktree.New(repositoryGit, cfg), nil
}

func (b *tuiBackend) MaterializeWorktree(ctx context.Context, row dashboard.Row) (string, error) {
	if row.Fleet == nil {
		return "", fmt.Errorf("no fleet worktree selected")
	}
	if row.Fleet.Local {
		return "", fmt.Errorf("worktree already synced")
	}
	if row.Fleet.Kind != "branch" || strings.TrimSpace(row.Fleet.Branch) == "" {
		return "", fmt.Errorf("only branch worktrees can be synced")
	}
	project, ok := b.projectForFleetInfo(row.Fleet)
	if !ok {
		return "", fmt.Errorf("no local project configured for %s", row.Fleet.ProjectIdentity)
	}
	repo := git.New(project.Path)
	branchExisted := localBranchExists(ctx, repo, row.Fleet.Branch)
	cfg, err := b.loadTargetConfig(project.Path, false)
	if err != nil {
		return "", fmt.Errorf("load selected project config: %w", err)
	}
	manager := worktree.New(repo, cfg)
	var (
		path               string
		worktreeGeneration string
		branchCreated      bool
	)
	if branchExisted {
		path, worktreeGeneration, err = manager.AddWithGeneration(
			row.Fleet.Branch,
			"",
			false,
			worktree.AddOptions{SkipSetup: true},
		)
	} else {
		var source string
		source, err = resolveFetchedRemoteSource(repo, row.Fleet.Branch)
		if err == nil {
			path, worktreeGeneration, err = manager.AddTrackingWithGeneration(
				row.Fleet.Branch,
				source,
				"",
			)
			branchCreated = err == nil
		}
	}
	if err != nil {
		syncErr := fmt.Errorf(
			"could not sync %s: branch must exist locally or on a fetched remote; push or fetch it first: %w",
			row.Fleet.Branch,
			err,
		)
		return "", syncErr
	}
	if err := b.verifyMaterializedHead(
		ctx,
		project.Path,
		path,
		worktreeGeneration,
		row.Fleet,
	); err != nil {
		if branchCreated {
			err = b.rollbackMaterializedBranch(
				repo,
				row.Fleet.Branch,
				err,
			)
		}
		return "", err
	}
	publishTUIFleetBestEffort(ctx, b.cfg)
	return path, nil
}

func resolveFetchedRemoteSource(repo *git.Git, branch string) (string, error) {
	branches, err := repo.ListAvailableBranches()
	if err != nil {
		return "", fmt.Errorf("list fetched branches: %w", err)
	}
	var source string
	for _, candidate := range branches {
		if !candidate.IsRemote || candidate.Name != branch {
			continue
		}
		if source != "" {
			return "", fmt.Errorf(
				"branch %s matches multiple fetched remotes",
				branch,
			)
		}
		source = candidate.Source
	}
	if source == "" {
		return "", fmt.Errorf(
			"branch %s has no matching fetched remote",
			branch,
		)
	}
	return source, nil
}

func (b *tuiBackend) rollbackMaterializedBranch(
	repo *git.Git,
	branch string,
	materializeErr error,
) error {
	if cleanupErr := repo.DeleteBranchIsolated(
		branch,
		b.protectedNames,
	); cleanupErr != nil {
		return fmt.Errorf(
			"%w (failed to remove auto-created branch %s; an incomplete worktree may remain: %v)",
			materializeErr,
			branch,
			cleanupErr,
		)
	}
	return materializeErr
}

func (b *tuiBackend) verifyMaterializedHead(
	ctx context.Context,
	repoRoot string,
	worktreePath string,
	worktreeGeneration string,
	info *dashboard.FleetInfo,
) error {
	if info == nil || strings.TrimSpace(info.RemoteHead) == "" {
		return nil
	}
	want := strings.TrimSpace(info.RemoteHead)
	got, err := git.New(worktreePath).RunWithContext(ctx, "rev-parse", "HEAD")
	if err != nil {
		return b.failMaterializedHeadVerification(
			repoRoot,
			worktreePath,
			worktreeGeneration,
			fmt.Errorf(
				"could not verify synced head for %s; push or fetch it first: %w",
				info.Branch,
				err,
			),
		)
	}
	got = strings.TrimSpace(got)
	if strings.EqualFold(got, want) {
		return nil
	}
	return b.failMaterializedHeadVerification(
		repoRoot,
		worktreePath,
		worktreeGeneration,
		fmt.Errorf(
			"synced %s at %s, but hub reported head %s; push or fetch the reported commit first",
			info.Branch,
			shortCommit(got),
			shortCommit(want),
		),
	)
}

func (b *tuiBackend) failMaterializedHeadVerification(
	repoRoot string,
	worktreePath string,
	worktreeGeneration string,
	verificationErr error,
) error {
	if err := git.ValidateWorktreeGeneration(worktreeGeneration); err != nil {
		return fmt.Errorf(
			"%w (rejected worktree preserved because its generation is unavailable)",
			verificationErr,
		)
	}
	if err := worktree.New(git.New(repoRoot), b.cfg).Remove(
		worktreePath,
		true,
		worktreeGeneration,
	); err != nil {
		return fmt.Errorf(
			"%w (failed to remove rejected worktree: %v)",
			verificationErr,
			err,
		)
	}
	reg, err := registry.New()
	if err != nil {
		return fmt.Errorf(
			"%w (worktree removed, but failed to open registry: %v)",
			verificationErr,
			err,
		)
	}
	if _, err := reg.UnregisterIfGeneration(
		worktreePath,
		worktreeGeneration,
	); err != nil {
		return fmt.Errorf(
			"%w (worktree removed, but failed to unregister it: %v)",
			verificationErr,
			err,
		)
	}
	return verificationErr
}

func localBranchExists(ctx context.Context, g *git.Git, branch string) bool {
	_, err := g.RunWithContext(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	if commit == "" {
		return "unknown"
	}
	return commit
}

func publishTUIFleetBestEffort(ctx context.Context, cfg *models.Config) {
	if cfg == nil || !cfg.Fleet.Enabled {
		return
	}
	var publishWarning bytes.Buffer
	_ = publishFleetBestEffort(ctx, cfg, newFleetManifestBuilder(), &publishWarning)
}

func (b *tuiBackend) projectForFleetInfo(info *dashboard.FleetInfo) (models.Project, bool) {
	if b == nil || b.cfg == nil || info == nil {
		return models.Project{}, false
	}
	projectIdentity := strings.TrimSpace(info.ProjectIdentity)
	if projectIdentity == "" {
		return models.Project{}, false
	}
	for _, project := range b.cfg.Projects {
		if strings.TrimSpace(project.Path) == "" {
			continue
		}
		if sameRepositoryIdentity(project.Repository, projectIdentity) {
			return project, true
		}
	}
	return models.Project{}, false
}

// equalRepositoryIdentity compares two repository identities with the host-aware
// fold. A plain EqualFold would let a case-sensitive server's two repositories
// match each other, and this decides which checkout a mutation runs against.
func equalRepositoryIdentity(left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	return url.FoldRepositoryIdentity(left) == url.FoldRepositoryIdentity(right)
}

func sameRepositoryIdentity(left string, right string) bool {
	if equalRepositoryIdentity(left, right) {
		return true
	}
	normalizedLeft, leftErr := fleet.NormalizeRepositoryIdentity(left)
	normalizedRight, rightErr := fleet.NormalizeRepositoryIdentity(right)
	return leftErr == nil && rightErr == nil &&
		equalRepositoryIdentity(normalizedLeft, normalizedRight)
}

func (b *tuiBackend) RemoveWorktree(ctx context.Context, row dashboard.Row, force bool) error {
	if row.Entry == nil {
		return fmt.Errorf("no worktree selected")
	}
	if row.Entry.IsMain {
		return fmt.Errorf("refusing to remove a main worktree")
	}
	generation := row.Entry.Generation
	if strings.TrimSpace(generation) == "" {
		return fmt.Errorf("worktree generation unavailable; refresh before removing")
	}
	registryRecord := registeredWorktreeForRemoval(row.Entry.Path)

	repoRoot, err := b.repositoryRootForRow(row)
	if err != nil {
		return err
	}
	if err := b.removeWorktreeFromRoot(
		repoRoot,
		row.Entry.Path,
		force,
		generation,
	); err != nil {
		if strings.Contains(err.Error(), "contains modified or untracked files") ||
			strings.Contains(err.Error(), "has local changes") {
			return fmt.Errorf("worktree has uncommitted changes")
		}
		return err
	}

	unregisterWorktreeRecord(registryRecord)

	publishTUIFleetBestEffort(ctx, b.cfg)

	if row.SessionLive && row.SessionName != "" {
		return b.tmux.KillSession(row.SessionName)
	}
	return nil
}

func (b *tuiBackend) repositoryRootForRow(row dashboard.Row) (string, error) {
	if row.Entry == nil {
		return "", fmt.Errorf("no worktree selected")
	}

	repoRoot, err := git.New(row.Entry.Path).GetMainRepositoryPath()
	if err == nil {
		if identityErr := b.validateRepositoryRootForRow(
			repoRoot,
			row,
		); identityErr != nil {
			return "", identityErr
		}
		return repoRoot, nil
	}
	directErr := err

	if b.cfg != nil {
		for _, project := range b.cfg.Projects {
			if !projectMatchesRow(project, row) {
				continue
			}
			repoRoot, err := git.New(project.Path).GetMainRepositoryPath()
			if err == nil {
				if identityErr := b.validateRepositoryRootForRow(
					repoRoot,
					row,
				); identityErr != nil {
					return "", identityErr
				}
				return repoRoot, nil
			}
		}
	}

	return "", fmt.Errorf("failed to find repository root: %w", directErr)
}

func (b *tuiBackend) validateRepositoryRootForRow(
	repoRoot string,
	row dashboard.Row,
) error {
	if row.Entry == nil || len(rowRepositoryIdentityCandidates(
		row.Entry.RepositoryInfo,
	)) == 0 {
		return nil
	}
	if b.cfg != nil {
		for _, project := range b.cfg.Projects {
			if !projectMatchesRow(project, row) {
				continue
			}
			projectRoot, err := git.New(project.Path).GetMainRepositoryPath()
			if err == nil &&
				utils.PathKey(projectRoot) == utils.PathKey(repoRoot) {
				return nil
			}
		}
	}

	var projects []models.Project
	if b.cfg != nil {
		projects = b.cfg.Projects
	}
	liveInfo, err := worktree.RepositoryInfoWithProjects(
		git.New(repoRoot),
		projects,
	)
	if err == nil {
		for _, candidate := range rowRepositoryIdentityCandidates(
			row.Entry.RepositoryInfo,
		) {
			if sameRepositoryIdentity(liveInfo.FullPath, candidate) {
				return nil
			}
		}
	}
	return fmt.Errorf(
		"repository identity changed for %s; refresh before removing",
		row.Entry.Path,
	)
}

func projectMatchesRow(project models.Project, row dashboard.Row) bool {
	if project.Path == "" || row.Entry == nil || row.Entry.RepositoryInfo == nil {
		return false
	}

	info := row.Entry.RepositoryInfo
	stableCandidates := rowRepositoryIdentityCandidates(info)
	for _, candidate := range stableCandidates {
		if equalRepositoryIdentity(project.Repository, candidate) {
			return true
		}
	}
	if len(stableCandidates) > 0 {
		return false
	}
	// Below here the row has no stable identity, so these compare bare
	// repository names rather than identities; host case-sensitivity does not
	// apply to a name with no host attached.
	if project.Repository != "" && info.Repository != "" && strings.EqualFold(project.Repository, info.Repository) {
		return true
	}
	return project.Name != "" && strings.EqualFold(project.Name, info.Repository)
}

func rowRepositoryIdentityCandidates(info *url.RepositoryInfo) []string {
	if info == nil {
		return nil
	}
	var candidates []string
	if info.FullPath != "" {
		candidates = append(candidates, info.FullPath)
	}
	if info.Host != "" && info.Owner != "" && info.Repository != "" {
		candidates = append(candidates, path.Join(info.Host, info.Owner, info.Repository))
	}
	return candidates
}

func (b *tuiBackend) removeWorktreeFromRoot(
	repoRoot string,
	worktreePath string,
	force bool,
	generation string,
) error {
	return worktree.New(git.New(repoRoot), b.cfg).Remove(
		worktreePath,
		force,
		generation,
	)
}

func samePath(a string, b string) bool {
	a = cleanComparablePath(a)
	b = cleanComparablePath(b)
	return a == b
}

func cleanComparablePath(path string) string {
	return utils.PathKey(path)
}

func (b *tuiBackend) KillSession(row dashboard.Row) error {
	if row.SessionName == "" {
		return fmt.Errorf("no live workspace")
	}
	return b.tmux.KillSession(row.SessionName)
}

func (b *tuiBackend) OpenInTmux(ctx context.Context, row dashboard.Row, layoutName string) error {
	return b.attachWorkspace(ctx, row, layoutName, true, false)
}

func (b *tuiBackend) AttachOutsideTmux(row dashboard.Row, layoutName string) error {
	return b.attachWorkspace(context.Background(), row, layoutName, false, config.StdinInteractive())
}

// LayoutNames returns the names the TUI layout cycler offers: the reserved
// blank session first, then the configured presets.
func (b *tuiBackend) LayoutNames() []string {
	names := make([]string, 0, len(b.cfg.Layouts.Presets)+1)
	names = append(names, tmux.BlankLayoutName)
	for _, layout := range b.cfg.Layouts.Presets {
		names = append(names, layout.Name)
	}
	return names
}

func (b *tuiBackend) InsideTmux() bool {
	return os.Getenv("TMUX") != ""
}

func (b *tuiBackend) attachWorkspace(ctx context.Context, row dashboard.Row, layoutName string, insideTmux bool, interactive bool) error {
	if row.Entry == nil && row.Workspace == nil {
		return fmt.Errorf("no worktree selected")
	}
	if err := rejectProtectedWorkspaceOpen(ctx, rowPaneRoot(row)); err != nil {
		return err
	}
	if err := b.acknowledgeRemoteSource(rowPaneRoot(row)); err != nil {
		return err
	}
	sessionName, err := b.sessionName(row)
	if err != nil {
		return err
	}
	layout, err := b.resolveLayout(row, layoutName, interactive)
	if err != nil {
		return err
	}
	return b.ensureAndAttach(
		ctx, sessionName, rowPaneRoot(row), layout, insideTmux,
	)
}

func rowPaneRoot(row dashboard.Row) string {
	if row.Workspace != nil {
		return row.Workspace.Path
	}
	if row.Entry != nil {
		return row.Entry.Path
	}
	return ""
}

func (b *tuiBackend) resolveLayout(row dashboard.Row, layoutName string, interactive bool) (models.Layout, error) {
	var layout models.Layout
	var err error
	if layoutName != "" {
		layout, err = tmux.ResolveLayout(b.cfg.Layouts, layoutName, false, "", nil)
	} else {
		var layoutRoot string
		if row.Workspace != nil {
			layoutRoot = row.Workspace.Path
		} else {
			layoutRoot, err = b.repositoryRootForRow(row)
			if err != nil {
				return models.Layout{}, err
			}
		}
		var targetDefault string
		targetDefault, err = config.LoadRepoLayoutDefault(layoutRoot, interactive)
		if err != nil {
			return models.Layout{}, err
		}
		layout, err = tmux.ResolveLayout(b.cfg.Layouts, "", false, targetDefault, nil)
	}
	if err != nil {
		return models.Layout{}, err
	}
	return tmux.ResolvePaneCommands(layout, b.cfg.Agents)
}

func (b *tuiBackend) sessionName(row dashboard.Row) (string, error) {
	if row.SessionName != "" {
		return row.SessionName, nil
	}
	if row.Workspace != nil {
		return tmux.DirWorkspaceSessionName(row.Workspace.Name, row.Workspace.Path), nil
	}
	if row.Entry == nil || row.Entry.RepositoryInfo == nil {
		return "", fmt.Errorf("could not resolve repository info for %s", rowPathForHandoff(row))
	}
	return tmux.WorkspaceSessionName(row.Entry.RepositoryInfo, row.Entry.Branch, row.Entry.Path), nil
}

func (b *tuiBackend) UnregisterWorkspace(row dashboard.Row) error {
	if row.Workspace == nil {
		return fmt.Errorf("no workspace selected")
	}
	// The TUI runs this concurrently with the load commands, which read
	// cfg.Workspaces to build workspace rows and write the config file when
	// they register the launch directory.
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.unregisterWorkspace(row.Workspace.Name); err != nil {
		return err
	}
	if b.cfg != nil {
		// A new slice, not a filter in place: rewriting the backing array is
		// what turns a concurrent read into duplicated rows.
		kept := make([]models.Workspace, 0, len(b.cfg.Workspaces))
		for _, workspace := range b.cfg.Workspaces {
			if !samePath(workspace.Path, row.Workspace.Path) {
				kept = append(kept, workspace)
			}
		}
		b.cfg.Workspaces = kept
	}
	return nil
}

func unknownStatusForEntry(entry *discovery.GlobalWorktreeEntry) *models.WorktreeStatus {
	repo := ""
	if entry.RepositoryInfo != nil {
		repo = entry.RepositoryInfo.Repository
	}
	return &models.WorktreeStatus{
		Path:       entry.Path,
		Branch:     entry.Branch,
		Repository: repo,
		Status:     models.WorktreeStatusUnknown,
	}
}
