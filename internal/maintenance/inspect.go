package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	gitadapter "go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

// Inspector gathers local facts and classifies consistency findings. Its
// injectable functions keep tests deterministic and the package provider-free.
type Inspector struct {
	Config               *models.Config
	ProjectRegistrations []config.ProjectRegistration
	RegistryEntries      []*registry.WorktreeEntry
	InspectRepository    func(string) (RepositorySnapshot, error)
	FindGlobalPaths      func(string) ([]string, error)
	ReadDotGitTarget     func(string) (string, error)
	PathExists           func(string) (bool, error)
	CreationActive       func(string) (bool, error)
}

// NewInspector creates an inspector backed by the repository's real local Git
// and filesystem implementations.
func NewInspector(
	cfg *models.Config,
	entries []*registry.WorktreeEntry,
	registrations []config.ProjectRegistration,
) *Inspector {
	inspector := &Inspector{
		Config: cfg, ProjectRegistrations: registrations, RegistryEntries: entries,
	}
	inspector.setDefaults()
	return inspector
}

// Inspect returns a stable report without changing Git, registry, or
// provenance state.
func (i *Inspector) Inspect(ctx context.Context) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	i.setDefaults()
	reports := make(map[string]*RepositoryReport)
	liveRepositoryIdentities := make(map[string]string)
	configuredClaims := make(map[string]*configuredRepositoryClaims)
	inventoryComplete := true

	registrations := append([]config.ProjectRegistration(nil), i.ProjectRegistrations...)
	sort.Slice(registrations, func(left, right int) bool {
		if registrations[left].Effective.Path == registrations[right].Effective.Path {
			return registrations[left].Effective.Name < registrations[right].Effective.Name
		}
		return registrations[left].Effective.Path < registrations[right].Effective.Path
	})
	missingProjects := make([]config.ProjectRegistration, 0)
	for _, registration := range registrations {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		project := registration.Effective
		exists, statErr := i.PathExists(project.Path)
		if statErr != nil {
			inventoryComplete = false
			i.addUnreachableProject(reports, project, fmt.Sprintf("configured project path could not be inspected: %v", statErr))
			continue
		}
		if !exists {
			missingProjects = append(missingProjects, registration)
			continue
		}
		snapshot, err := i.InspectRepository(project.Path)
		if err != nil || snapshot.CommonDir == "" {
			inventoryComplete = false
			message := "configured project could not be inspected"
			if err != nil {
				message = fmt.Sprintf("configured project could not be inspected: %v", err)
			}
			i.addUnreachableProject(reports, project, message)
			continue
		}
		if !gitadapter.HasExactWorktreeRoot(snapshot.Worktrees, project.Path) {
			inventoryComplete = false
			i.addUnreachableProject(
				reports,
				project,
				"configured project path is not an exact Git worktree root",
			)
			continue
		}
		report := mergeSnapshot(reports, snapshot)
		mergeLiveRepositoryIdentities(liveRepositoryIdentities, snapshot)
		addConfiguredRepositoryClaim(
			configuredClaims,
			pathKey(snapshot.CommonDir),
			project.Repository,
		)
		appendProjectName(report, project.Name)
		if report.RepositoryIdentity == "" {
			report.RepositoryIdentity = configuredRepositoryIdentity(project.Repository)
		}
	}
	classifyConfiguredRepositoryClaims(reports, configuredClaims)

	globalPaths := []string(nil)
	if strings.TrimSpace(i.Config.Worktree.BaseDir) != "" {
		var err error
		globalPaths, err = i.FindGlobalPaths(i.Config.Worktree.BaseDir)
		if err != nil {
			return Report{}, fmt.Errorf("discover global worktree paths: %w", err)
		}
	}
	claims := make(map[string][]string)
	for _, path := range globalPaths {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		target, targetErr := i.ReadDotGitTarget(path)
		if targetErr != nil || target == "" {
			inventoryComplete = false
			report := reportForKey(reports, "unreachable:"+pathKey(path))
			report.Root = path
			message := "global worktree .git entry could not be inspected"
			if targetErr != nil {
				message = fmt.Sprintf("global worktree .git entry could not be inspected: %v", targetErr)
			}
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: path,
				Message: message, Fixable: false,
				Remediation: "Restore access to the linked worktree metadata and rerun kwt doctor.",
			})
			continue
		}
		addClaim(claims, target, path)
		snapshot, err := i.InspectRepository(path)
		if err != nil || snapshot.CommonDir == "" {
			inventoryComplete = false
			report := reportForKey(reports, "unreachable:"+pathKey(path))
			report.Root = path
			message := "global worktree repository could not be inspected"
			if err != nil {
				message = fmt.Sprintf("global worktree repository could not be inspected: %v", err)
			}
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: path,
				Message: message, Fixable: false,
				Remediation: "Restore access to the linked repository and rerun kwt doctor.",
			})
			continue
		}
		if !gitadapter.HasExactWorktreeRoot(snapshot.Worktrees, path) {
			inventoryComplete = false
			report := reportForKey(reports, "unreachable:"+pathKey(path))
			report.Root = path
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: path,
				Message:     "global inventory path is not an exact Git worktree root",
				Remediation: "Review or remove the invalid path and rerun kwt doctor.",
			})
			continue
		}
		mergeSnapshot(reports, snapshot)
		mergeLiveRepositoryIdentities(liveRepositoryIdentities, snapshot)
	}
	for _, entry := range i.RegistryEntries {
		if entry == nil {
			continue
		}
		active, activeErr := i.registryCreationActive(entry)
		if activeErr != nil {
			inventoryComplete = false
			continue
		}
		if active {
			inventoryComplete = false
			continue
		}
		exists, statErr := i.PathExists(entry.Path)
		if statErr != nil {
			inventoryComplete = false
			continue
		}
		if !exists {
			continue
		}
		snapshot, err := i.InspectRepository(entry.Path)
		if err != nil || snapshot.CommonDir == "" {
			inventoryComplete = false
			report := reportForKey(reports, "registry-path:"+pathKey(entry.Path))
			report.Root = entry.Path
			message := "live registry path could not be inspected as a Git repository"
			if err != nil {
				message = fmt.Sprintf("live registry path could not be inspected as a Git repository: %v", err)
			}
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: entry.Path,
				Message:     message,
				Remediation: "Restore access to the registered worktree and rerun kwt doctor.",
			})
			continue
		}
		if !gitadapter.HasExactWorktreeRoot(snapshot.Worktrees, entry.Path) {
			inventoryComplete = false
			continue
		}
		mergeSnapshot(reports, snapshot)
		mergeLiveRepositoryIdentities(liveRepositoryIdentities, snapshot)
	}
	for _, report := range reports {
		for _, inspection := range report.Worktrees {
			if inspection.DotGitTarget != "" {
				addClaim(claims, inspection.DotGitTarget, inspection.Path)
			}
		}
	}
	i.classifyMissingProjects(reports, missingProjects, inventoryComplete)

	for _, report := range reports {
		i.classifyRepository(report, claims)
	}
	i.classifyRegistry(reports, liveRepositoryIdentities)

	result := Report{SchemaVersion: SchemaVersion, Command: "doctor"}
	for _, report := range reports {
		sort.Strings(report.ProjectNames)
		sort.Slice(report.Worktrees, func(left, right int) bool {
			return pathKey(report.Worktrees[left].Path) < pathKey(report.Worktrees[right].Path)
		})
		sort.Slice(report.Findings, func(left, right int) bool {
			if pathKey(report.Findings[left].Path) == pathKey(report.Findings[right].Path) {
				return report.Findings[left].Code < report.Findings[right].Code
			}
			return pathKey(report.Findings[left].Path) < pathKey(report.Findings[right].Path)
		})
		result.Repositories = append(result.Repositories, *report)
	}
	sort.Slice(result.Repositories, func(left, right int) bool {
		leftKey := result.Repositories[left].CommonDir + "\x00" + result.Repositories[left].Root
		rightKey := result.Repositories[right].CommonDir + "\x00" + result.Repositories[right].Root
		return leftKey < rightKey
	})
	result.Summary.Repositories = len(result.Repositories)
	for _, report := range result.Repositories {
		for _, finding := range report.Findings {
			result.Summary.Findings++
			if finding.Fixable {
				result.Summary.FixableFindings++
			} else {
				result.Summary.ManualFindings++
			}
		}
	}
	result.Summary.Healthy = result.Summary.Findings == 0
	return result, nil
}

type configuredRepositoryClaims struct {
	identities map[string]string
	invalid    int
}

func addConfiguredRepositoryClaim(
	claimsByRepository map[string]*configuredRepositoryClaims,
	repositoryKey string,
	raw string,
) {
	claims := claimsByRepository[repositoryKey]
	if claims == nil {
		claims = &configuredRepositoryClaims{identities: make(map[string]string)}
		claimsByRepository[repositoryKey] = claims
	}
	identity, ok := repairableProjectIdentity(raw)
	if !ok {
		claims.invalid++
		return
	}
	claims.identities[repositoryurl.FoldRepositoryIdentity(identity)] = identity
}

func classifyConfiguredRepositoryClaims(
	reports map[string]*RepositoryReport,
	claimsByRepository map[string]*configuredRepositoryClaims,
) {
	for repositoryKey, claims := range claimsByRepository {
		if claims.invalid == 0 && len(claims.identities) < 2 {
			continue
		}
		report := reports[repositoryKey]
		if report == nil {
			continue
		}
		identities := make([]string, 0, len(claims.identities))
		for _, identity := range claims.identities {
			identities = append(identities, identity)
		}
		sort.Strings(identities)
		evidence := make(map[string]string)
		if len(identities) != 0 {
			evidence["configured_repositories"] = strings.Join(identities, "\n")
		}
		if claims.invalid != 0 {
			evidence["invalid_claim_count"] = fmt.Sprintf("%d", claims.invalid)
		}
		addFinding(report, Finding{
			Code: RepositoryIdentityMismatch, Severity: SeverityError,
			Path:        report.Root,
			Message:     "configured projects for this repository have invalid or conflicting repository identities",
			Remediation: "Update the project registrations to use one canonical network repository identity, then rerun kwt doctor.",
			Evidence:    evidence,
		})
	}
}

func (i *Inspector) addUnreachableProject(
	reports map[string]*RepositoryReport,
	project models.Project,
	message string,
) {
	report := reportForKey(reports, "unreachable:"+pathKey(project.Path))
	report.Root = project.Path
	report.RepositoryIdentity = configuredRepositoryIdentity(project.Repository)
	appendProjectName(report, project.Name)
	addFinding(report, Finding{
		Code: ProjectUnreachable, Severity: SeverityError, Path: project.Path,
		Message:     message,
		Remediation: "Restore access to the configured repository path or update the project registration.",
	})
}

type projectRelocationCandidate struct {
	root       string
	commonDir  string
	repository string
}

func (i *Inspector) classifyMissingProjects(
	reports map[string]*RepositoryReport,
	registrations []config.ProjectRegistration,
	inventoryComplete bool,
) {
	if !inventoryComplete {
		for _, registration := range registrations {
			project := registration.Effective
			report := reportForKey(reports, "unreachable:"+pathKey(project.Path))
			report.Root = project.Path
			appendProjectName(report, project.Name)
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: project.Path,
				Message:     "configured project is absent but repository inventory is incomplete",
				Remediation: "Restore access to every repository reported as unreachable, then rerun kwt doctor before changing this registration.",
				Evidence:    map[string]string{"old_path": project.Path},
			})
		}
		return
	}
	registeredProjectPaths := make(map[string]string, len(i.ProjectRegistrations))
	for _, registration := range i.ProjectRegistrations {
		path := registration.Effective.Path
		if path != "" {
			registeredProjectPaths[pathKey(path)] = path
		}
	}
	duplicates := make([]bool, len(registrations))
	for left := range registrations {
		for right := left + 1; right < len(registrations); right++ {
			if registrations[left].SamePersistedEntry(registrations[right]) {
				duplicates[left] = true
				duplicates[right] = true
			}
		}
	}
	candidatesByIdentity := make(map[string][]projectRelocationCandidate)
	seen := make(map[string]bool)
	for _, report := range reports {
		if report.CommonDir == "" || report.Root == "" || report.RepositoryIdentity == "" {
			continue
		}
		identity, ok := repairableProjectIdentity(report.RepositoryIdentity)
		if !ok {
			continue
		}
		candidateKey := pathKey(report.CommonDir)
		if seen[candidateKey] {
			continue
		}
		seen[candidateKey] = true
		folded := repositoryurl.FoldRepositoryIdentity(identity)
		candidatesByIdentity[folded] = append(candidatesByIdentity[folded], projectRelocationCandidate{
			root: report.Root, commonDir: report.CommonDir, repository: identity,
		})
	}
	matchingTargets := func(registration config.ProjectRegistration) []projectRelocationCandidate {
		identity, ok := repairableProjectIdentity(registration.Persisted.Repository)
		if !ok {
			return nil
		}
		matches := append([]projectRelocationCandidate(nil),
			candidatesByIdentity[repositoryurl.FoldRepositoryIdentity(identity)]...)
		sort.Slice(matches, func(left, right int) bool {
			return pathKey(matches[left].root) < pathKey(matches[right].root)
		})
		return matches
	}
	targetClaimants := make(map[string]int)
	for index, registration := range registrations {
		if duplicates[index] {
			continue
		}
		matches := matchingTargets(registration)
		if len(matches) != 1 {
			continue
		}
		target := matches[0]
		if registeredPath, ok := registeredProjectPaths[pathKey(target.root)]; ok &&
			pathKey(registeredPath) != pathKey(registration.Effective.Path) {
			continue
		}
		targetClaimants[pathKey(target.root)]++
	}

	for index, registration := range registrations {
		project := registration.Effective
		report := reportForKey(reports, "unreachable:"+pathKey(project.Path))
		report.Root = project.Path
		appendProjectName(report, project.Name)
		if duplicates[index] {
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: project.Path,
				Message:     "configured project is absent but its persisted registration is duplicated",
				Remediation: "Remove or consolidate the duplicate project registrations manually, then rerun kwt doctor.",
			})
			continue
		}
		identity, ok := repairableProjectIdentity(registration.Persisted.Repository)
		if !ok {
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: project.Path,
				Message:     "configured project is absent and its stored repository identity is not safe for automatic matching",
				Remediation: "Update or remove this project registration manually, then rerun kwt doctor.",
			})
			continue
		}
		report.RepositoryIdentity = identity
		matches := matchingTargets(registration)
		switch len(matches) {
		case 0:
			addFinding(report, Finding{
				Code: StaleProjectRegistration, Severity: SeverityWarning, Path: project.Path,
				Message:     "configured project path is absent and no matching live repository was found",
				Remediation: "Run kwt doctor --fix to remove the unchanged stale project registration.",
				Fixable:     true,
				Evidence:    map[string]string{"old_path": project.Path},
				ProjectRepair: &ProjectRepairCondition{
					Action: RemoveProject, Expected: registration,
				},
			})
		case 1:
			target := matches[0]
			if registeredPath, ok := registeredProjectPaths[pathKey(target.root)]; ok &&
				pathKey(registeredPath) != pathKey(project.Path) {
				addFinding(report, Finding{
					Code: StaleProjectRegistration, Severity: SeverityWarning, Path: project.Path,
					Message:     "configured project path is absent and the matching live repository is already registered",
					Remediation: "Run kwt doctor --fix to remove the unchanged stale duplicate registration.",
					Fixable:     true,
					Evidence: map[string]string{
						"old_path": project.Path, "registered_path": target.root,
					},
					ProjectRepair: &ProjectRepairCondition{
						Action: RemoveProject, Expected: registration,
					},
				})
				continue
			}
			if targetClaimants[pathKey(target.root)] > 1 {
				addFinding(report, Finding{
					Code: AmbiguousProjectRelocation, Severity: SeverityError, Path: project.Path,
					Message:     "multiple missing project registrations match the same live repository target",
					Remediation: "Choose which project registration should own the live repository, update the others manually, then rerun kwt doctor.",
					Evidence: map[string]string{
						"old_path": project.Path, "candidate_paths": target.root,
					},
				})
				continue
			}
			addFinding(report, Finding{
				Code: ProjectPathMoved, Severity: SeverityWarning, Path: project.Path,
				Message:     "configured project path is absent and one matching live repository was found",
				Remediation: "Run kwt doctor --fix to update the unchanged project registration.",
				Fixable:     true,
				Evidence: map[string]string{
					"old_path": project.Path, "new_path": target.root,
				},
				ProjectRepair: &ProjectRepairCondition{
					Action: RelocateProject, Expected: registration,
					TargetRoot: target.root, TargetCommonDir: target.commonDir,
					TargetRepository: target.repository,
				},
			})
		default:
			paths := make([]string, 0, len(matches))
			for _, match := range matches {
				paths = append(paths, match.root)
			}
			addFinding(report, Finding{
				Code: AmbiguousProjectRelocation, Severity: SeverityError, Path: project.Path,
				Message:     "configured project path is absent and multiple matching live repositories were found",
				Remediation: "Choose the intended repository path and update the project registration manually.",
				Evidence: map[string]string{
					"old_path": project.Path, "candidate_paths": strings.Join(paths, "\n"),
				},
			})
		}
	}
}

func repairableProjectIdentity(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if repositoryurl.IsPathFallbackIdentity(raw) ||
		strings.HasPrefix(strings.ToLower(raw), "file:") {
		return "", false
	}
	identity, ok := repositoryurl.CanonicalRepositoryIdentity(raw)
	if !ok || repositoryurl.IsPathFallbackIdentity(identity) {
		return "", false
	}
	return identity, true
}

func (i *Inspector) classifyRepository(
	report *RepositoryReport,
	claims map[string][]string,
) {
	for _, inspection := range report.Worktrees {
		exists, statErr := i.PathExists(inspection.Path)
		if statErr != nil {
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: inspection.Path,
				Message:     fmt.Sprintf("worktree path could not be inspected: %v", statErr),
				Remediation: "Restore filesystem access and rerun kwt doctor.",
			})
			continue
		}
		if inspection.GitDirError != "" {
			addFinding(report, Finding{
				Code: AmbiguousWorktreeBacklink, Severity: SeverityError,
				Path:        inspection.Path,
				Message:     "Git worktree administrative ownership could not be determined",
				Remediation: "Review the competing Git administrative records, resolve the ambiguity manually, then rerun kwt doctor.",
				Evidence: map[string]string{
					"git_dir_error": inspection.GitDirError,
				},
			})
			continue
		}
		claimants := claimantPaths(claims, inspection)
		ambiguous := false
		for _, claimant := range claimants {
			if pathKey(claimant) != pathKey(inspection.Path) {
				ambiguous = true
				break
			}
		}
		if len(claimants) > 1 {
			ambiguous = true
		}
		if ambiguous {
			addFinding(report, Finding{
				Code: AmbiguousWorktreeBacklink, Severity: SeverityError, Path: inspection.Path,
				Message:     "multiple filesystem paths claim the same Git worktree administrative record",
				Remediation: "Choose the intended worktree path, remove or repair the conflicting copies manually, then rerun kwt doctor.",
				Evidence:    map[string]string{"claimants": strings.Join(claimants, ",")},
			})
		} else if !inspection.IsMain && exists && inspection.GitDir != "" &&
			(inspection.DotGitTarget == "" || pathKey(inspection.DotGitTarget) != pathKey(inspection.GitDir)) {
			addFinding(report, Finding{
				Code: BrokenWorktreeBacklink, Severity: SeverityWarning, Path: inspection.Path,
				Message:     "linked worktree points to an outdated Git administrative directory",
				Remediation: "Run kwt doctor --fix to repair the verified backlink.",
				Fixable:     true,
				Evidence: map[string]string{
					"current_target":  inspection.DotGitTarget,
					"expected_target": inspection.GitDir,
				},
			})
		}
		if !exists {
			addFinding(report, Finding{
				Code: MissingWorktreeDirectory, Severity: SeverityWarning, Path: inspection.Path,
				Message:     "Git records a worktree path that is confirmed absent",
				Remediation: "Run kwt doctor --fix to prune the stale Git administrative record.",
				Fixable:     inspection.Prunable && !inspection.Locked && inspection.GitDir != "" && !ambiguous,
				Evidence:    map[string]string{"git_dir": inspection.GitDir},
			})
			continue
		}
		if inspection.GenerationStatus != gitadapter.GenerationValid {
			addFinding(report, Finding{
				Code: MissingGeneration, Severity: SeverityWarning, Path: inspection.Path,
				Message:     "live worktree has no valid durable kwt generation",
				Remediation: fmt.Sprintf("Run kwt list from %s to initialize the Git generation, then run kwt doctor --fix to reconcile registry metadata.", report.Root),
				Evidence:    map[string]string{"generation_status": string(inspection.GenerationStatus)},
			})
		}
	}
}

func (i *Inspector) classifyRegistry(
	reports map[string]*RepositoryReport,
	liveRepositoryIdentities map[string]string,
) {
	byPath := make(map[string]*RepositoryReport)
	inspectionByPath := make(map[string]gitadapter.WorktreeInspection)
	byIdentity := make(map[string][]*RepositoryReport)
	for _, report := range reports {
		if report.RepositoryIdentity != "" {
			identity := repositoryurl.FoldRepositoryIdentity(report.RepositoryIdentity)
			byIdentity[identity] = append(byIdentity[identity], report)
		}
		for _, inspection := range report.Worktrees {
			byPath[pathKey(inspection.Path)] = report
			inspectionByPath[pathKey(inspection.Path)] = inspection
		}
	}
	registryReports := make(map[string]*RepositoryReport)
	reportForRegistryEntry := func(entry *registry.WorktreeEntry) *RepositoryReport {
		key := pathKey(entry.Path)
		if report := registryReports[key]; report != nil {
			return report
		}
		if report := byPath[key]; report != nil {
			registryReports[key] = report
			return report
		}
		registryIdentity, registryIdentityOK :=
			repositoryurl.CanonicalRepositoryIdentity(entry.Repository)
		if registryIdentityOK {
			matches := byIdentity[repositoryurl.FoldRepositoryIdentity(registryIdentity)]
			if len(matches) == 1 {
				registryReports[key] = matches[0]
				return matches[0]
			}
		}
		report := reportForKey(reports, "registry-path:"+key)
		report.Root = entry.Path
		if registryIdentityOK && report.RepositoryIdentity == "" {
			report.RepositoryIdentity = registryIdentity
			identity := repositoryurl.FoldRepositoryIdentity(registryIdentity)
			byIdentity[identity] = append(byIdentity[identity], report)
		}
		registryReports[key] = report
		return report
	}
	type registryInspectionState struct {
		active bool
		err    error
	}
	states := make(map[*registry.WorktreeEntry]registryInspectionState, len(i.RegistryEntries))
	groups := make(map[string][]*registry.WorktreeEntry)
	for _, entry := range i.RegistryEntries {
		if entry == nil {
			continue
		}
		active, err := i.registryCreationActive(entry)
		states[entry] = registryInspectionState{active: active, err: err}
		key := pathKey(entry.Path)
		groups[key] = append(groups[key], entry)
	}
	deferredGroups := make(map[string]bool)
	handledDuplicateGroups := make(map[string]bool)
	for key, entries := range groups {
		if len(entries) < 2 {
			continue
		}
		for _, entry := range entries {
			state := states[entry]
			if state.active || state.err != nil {
				deferredGroups[key] = true
				break
			}
		}
		if deferredGroups[key] {
			continue
		}
		sort.Slice(entries, func(left, right int) bool {
			return literalPathKey(entries[left].Path) < literalPathKey(entries[right].Path)
		})
		retained := entries[0]
		inspection, matched := inspectionByPath[key]
		if matched && inspection.Exists {
			inspectionPathKey := literalPathKey(inspection.Path)
			for _, entry := range entries {
				if literalPathKey(entry.Path) == inspectionPathKey {
					retained = entry
					break
				}
			}
		}
		existsKnown := matched
		exists := matched && inspection.Exists
		if !matched {
			present, absent := 0, 0
			for _, entry := range entries {
				entryExists, err := i.PathExists(entry.Path)
				if err != nil {
					present, absent = 0, 0
					break
				}
				if entryExists {
					present++
				} else {
					absent++
				}
			}
			switch {
			case present == len(entries):
				existsKnown, exists = true, true
			case absent == len(entries):
				existsKnown, exists = true, false
			}
		}
		equivalent := equivalentRegistryAliases(entries)
		severity := SeverityError
		message := "multiple registry paths resolve to the same path but cannot be safely reconciled"
		remediation := "Restore complete filesystem access, review the conflicting registry records, and rerun kwt doctor."
		fixable := false
		var repair *RegistryAliasRepairCondition
		switch {
		case !existsKnown:
		case !exists:
			message = "multiple registry paths resolve to the same confirmed-absent worktree but their policy metadata differs"
			remediation = "Review the conflicting stale registry records and remove only the policies that are no longer needed."
			if equivalent {
				severity = SeverityWarning
				message = "multiple equivalent registry paths resolve to the same confirmed-absent worktree"
				remediation = "Run kwt doctor --fix to remove the complete unchanged stale alias group."
				fixable = true
				repair = &RegistryAliasRepairCondition{
					Expected: cloneRegistryEntries(entries),
				}
			}
		case matched:
			message = "multiple registry paths resolve to the same live worktree but their policy metadata differs"
			remediation = "Review the conflicting registry records and keep only the intended policy before rerunning kwt doctor."
			if equivalent {
				severity = SeverityWarning
				message = "multiple equivalent registry paths resolve to the same live worktree"
				remediation = "Run kwt doctor --fix to collapse the unchanged aliases to one registry record."
				fixable = true
				repair = &RegistryAliasRepairCondition{
					Expected: cloneRegistryEntries(entries),
					Retained: cloneRegistryEntry(retained),
				}
			}
		default:
			message = "multiple registry paths resolve to an existing path that is not a verified live Git worktree"
			remediation = "Restore or remove the worktree metadata, then rerun kwt doctor before changing the registry records."
		}
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, entry.Path)
		}
		report := reportForRegistryEntry(retained)
		addFinding(report, Finding{
			Code: DuplicateRegistryEntry, Severity: severity, Path: retained.Path,
			Message: message, Remediation: remediation, Fixable: fixable,
			Evidence:            map[string]string{"paths": strings.Join(paths, "\n")},
			RegistryAliasRepair: repair,
		})
		handledDuplicateGroups[key] = true
	}
	for _, entry := range i.RegistryEntries {
		if entry == nil {
			continue
		}
		state := states[entry]
		active, activeErr := state.active, state.err
		if activeErr != nil {
			report := reportForKey(reports, "registry-path:"+pathKey(entry.Path))
			report.Root = entry.Path
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: entry.Path,
				Message:     fmt.Sprintf("registry creation ownership could not be inspected: %v", activeErr),
				Remediation: "Restore access to the registry creation lock and rerun kwt doctor.",
			})
			continue
		}
		key := pathKey(entry.Path)
		if active || deferredGroups[key] || handledDuplicateGroups[key] {
			continue
		}
		registryIdentity, registryIdentityOK :=
			repositoryurl.CanonicalRepositoryIdentity(entry.Repository)
		report := reportForRegistryEntry(entry)
		exists, err := i.PathExists(entry.Path)
		if err != nil {
			addFinding(report, Finding{
				Code: ProjectUnreachable, Severity: SeverityError, Path: entry.Path,
				Message:     fmt.Sprintf("registry path could not be inspected: %v", err),
				Remediation: "Restore filesystem access and rerun kwt doctor.",
			})
			continue
		}
		if !exists {
			addFinding(report, Finding{
				Code: StaleRegistryEntry, Severity: SeverityWarning, Path: entry.Path,
				Message:     "kwt registry records a path that is confirmed absent",
				Remediation: "Run kwt doctor --fix to remove the unchanged stale registry record.",
				Fixable:     true,
				Evidence:    map[string]string{"generation": entry.Generation},
			})
		}
		inspection, matchedInspection := inspectionByPath[pathKey(entry.Path)]
		if exists && !matchedInspection {
			addFinding(report, Finding{
				Code: UnverifiedRegistryEntry, Severity: SeverityError, Path: entry.Path,
				Message:     "registry path exists but is not a verified Git worktree",
				Remediation: "Restore valid worktree metadata or remove this registry policy manually, then rerun kwt doctor.",
				Evidence:    map[string]string{"generation": entry.Generation},
			})
			continue
		}
		if exists && matchedInspection &&
			inspection.GenerationStatus == gitadapter.GenerationValid &&
			entry.Generation != inspection.Generation {
			fixable := entry.Generation == ""
			severity := SeverityWarning
			message := "generation-less registry record does not yet name the verified live Git worktree generation"
			remediation := "Run kwt doctor --fix to adopt the live generation and clear inherited expiration policy."
			if !fixable {
				severity = SeverityError
				message = "registry and live Git generations identify different worktrees at the same path"
				remediation = "Review the replacement conflict and remove or recreate the stale registry record manually; kwt will not transfer its policy metadata."
			}
			addFinding(report, Finding{
				Code: RegistryGenerationMismatch, Severity: severity, Path: entry.Path,
				Message:     message,
				Remediation: remediation,
				Fixable:     fixable,
				Evidence: map[string]string{
					"registry_generation": entry.Generation,
					"git_generation":      inspection.Generation,
				},
			})
		} else if entry.Generation == "" {
			remediation := "Review this unmatched legacy registry record and either register or remove it manually."
			if !exists {
				remediation = "Run kwt doctor --fix to compare-and-delete this unchanged legacy registry record."
			} else if matchedInspection {
				remediation = fmt.Sprintf("Run kwt list from %s to initialize the Git generation, then run kwt doctor --fix to reconcile the registry.", report.Root)
			}
			addFinding(report, Finding{
				Code: MissingGeneration, Severity: SeverityWarning, Path: entry.Path,
				Message:     "registry record has no durable kwt generation",
				Remediation: remediation,
			})
		}
		if matched := byPath[pathKey(entry.Path)]; matched != nil &&
			entry.Repository != "" &&
			(!registryIdentityOK || !repositoryIdentityMatchesAny(
				registryIdentity,
				matched.RepositoryIdentity,
				liveRepositoryIdentities[pathKey(entry.Path)],
			)) {
			registryEvidence := "[redacted]"
			if registryIdentityOK {
				registryEvidence = registryIdentity
			}
			evidence := map[string]string{"registry_repository": registryEvidence}
			if matched.RepositoryIdentity != "" {
				evidence["configured_repository"] = matched.RepositoryIdentity
			}
			if liveIdentity := liveRepositoryIdentities[pathKey(entry.Path)]; liveIdentity != "" {
				evidence["live_repository"] = liveIdentity
			}
			addFinding(matched, Finding{
				Code: RepositoryIdentityMismatch, Severity: SeverityError, Path: entry.Path,
				Message:     "registry repository identity does not match the configured project or live worktree origin",
				Remediation: "Review the registry and configured project identities before changing either record.",
				Evidence:    evidence,
			})
		}
	}
}

func equivalentRegistryAliases(entries []*registry.WorktreeEntry) bool {
	if len(entries) < 2 {
		return false
	}
	first := entries[0]
	firstRepository, ok := repositoryurl.CanonicalRepositoryIdentity(first.Repository)
	if !ok || first.CreationToken != "" {
		return false
	}
	for _, entry := range entries[1:] {
		repositoryIdentity, identityOK := repositoryurl.CanonicalRepositoryIdentity(
			entry.Repository,
		)
		if !identityOK || entry.CreationToken != "" ||
			repositoryurl.FoldRepositoryIdentity(repositoryIdentity) !=
				repositoryurl.FoldRepositoryIdentity(firstRepository) ||
			entry.Branch != first.Branch || entry.Hash != first.Hash ||
			entry.IsMain != first.IsMain ||
			!entry.RegisteredAt.Equal(first.RegisteredAt) ||
			!sameRegistryTime(entry.ExpiresAt, first.ExpiresAt) ||
			entry.Generation != first.Generation ||
			entry.UnreviewedRemoteSource != first.UnreviewedRemoteSource {
			return false
		}
	}
	return true
}

func sameRegistryTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

func cloneRegistryEntries(entries []*registry.WorktreeEntry) []*registry.WorktreeEntry {
	cloned := make([]*registry.WorktreeEntry, 0, len(entries))
	for _, entry := range entries {
		cloned = append(cloned, cloneRegistryEntry(entry))
	}
	return cloned
}

func cloneRegistryEntry(entry *registry.WorktreeEntry) *registry.WorktreeEntry {
	if entry == nil {
		return nil
	}
	cloned := *entry
	if entry.ExpiresAt != nil {
		expiresAt := *entry.ExpiresAt
		cloned.ExpiresAt = &expiresAt
	}
	return &cloned
}

func literalPathKey(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.ToSlash(path))
	}
	return path
}

func (i *Inspector) setDefaults() {
	if i.Config == nil {
		i.Config = &models.Config{}
	}
	if i.ProjectRegistrations == nil {
		i.ProjectRegistrations = make([]config.ProjectRegistration, len(i.Config.Projects))
		for index := range i.Config.Projects {
			i.ProjectRegistrations[index] = config.ProjectRegistration{
				Persisted: i.Config.Projects[index], Effective: i.Config.Projects[index],
			}
		}
	}
	if i.InspectRepository == nil {
		i.InspectRepository = i.inspectRepository
	}
	if i.FindGlobalPaths == nil {
		i.FindGlobalPaths = discovery.FindGlobalWorktreePathsStrict
	}
	if i.ReadDotGitTarget == nil {
		i.ReadDotGitTarget = readDotGitTarget
	}
	if i.PathExists == nil {
		i.PathExists = pathExists
	}
	if i.CreationActive == nil {
		i.CreationActive = func(string) (bool, error) { return true, nil }
	}
}

func (i *Inspector) registryCreationActive(entry *registry.WorktreeEntry) (bool, error) {
	if entry.CreationToken == "" {
		return false, nil
	}
	return i.CreationActive(entry.Path)
}

func (i *Inspector) inspectRepository(path string) (RepositorySnapshot, error) {
	g := gitadapter.New(path)
	root, err := g.GetMainRepositoryPath()
	if err != nil {
		return RepositorySnapshot{}, err
	}
	commonDir, err := g.RunCommand("rev-parse", "--git-common-dir")
	if err != nil {
		return RepositorySnapshot{}, err
	}
	commonDir = strings.TrimSpace(commonDir)
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	rootGit := gitadapter.New(root)
	worktrees, err := rootGit.InspectWorktrees()
	if err != nil {
		return RepositorySnapshot{}, err
	}
	identity := ""
	for _, project := range i.Config.Projects {
		if pathKey(project.Path) == pathKey(root) {
			identity = configuredRepositoryIdentity(project.Repository)
			if identity != "" {
				break
			}
		}
	}
	if identity == "" {
		if remote, remoteErr := rootGit.GetRepositoryURL(); remoteErr == nil {
			identity, _ = repositoryurl.CanonicalRepositoryIdentityFromRemote(remote)
		}
	}
	if identity == "" {
		if info, localErr := worktree.RepositoryInfoFromLocalPath(root); localErr == nil {
			identity = info.FullPath
		}
	}
	liveIdentities := make(map[string]string)
	for _, inspection := range worktrees {
		remote, remoteErr := gitadapter.New(inspection.Path).GetRepositoryURL()
		if remoteErr != nil {
			continue
		}
		liveIdentity, ok := repositoryurl.CanonicalRepositoryIdentityFromRemote(remote)
		if ok {
			liveIdentities[pathKey(inspection.Path)] = liveIdentity
		}
	}
	return RepositorySnapshot{
		Root: root, CommonDir: utils.CanonicalPath(commonDir),
		RepositoryIdentity: identity, LiveRepositoryIdentities: liveIdentities,
		Worktrees: worktrees,
	}, nil
}

func configuredRepositoryIdentity(raw string) string {
	identity, _ := repositoryurl.CanonicalRepositoryIdentity(raw)
	return identity
}

func repositoryIdentityMatchesAny(identity string, candidates ...string) bool {
	folded := repositoryurl.FoldRepositoryIdentity(identity)
	for _, candidate := range candidates {
		if candidate != "" && folded == repositoryurl.FoldRepositoryIdentity(candidate) {
			return true
		}
	}
	return false
}

func mergeLiveRepositoryIdentities(
	destination map[string]string,
	snapshot RepositorySnapshot,
) {
	for path, identity := range snapshot.LiveRepositoryIdentities {
		destination[pathKey(path)] = identity
	}
}

func mergeSnapshot(
	reports map[string]*RepositoryReport,
	snapshot RepositorySnapshot,
) *RepositoryReport {
	key := pathKey(snapshot.CommonDir)
	report := reportForKey(reports, key)
	if report.CommonDir == "" {
		report.CommonDir = utils.CanonicalPath(snapshot.CommonDir)
		report.Root = snapshot.Root
		report.RepositoryIdentity = snapshot.RepositoryIdentity
		report.Worktrees = append([]gitadapter.WorktreeInspection(nil), snapshot.Worktrees...)
	}
	return report
}

func reportForKey(
	reports map[string]*RepositoryReport,
	key string,
) *RepositoryReport {
	if report := reports[key]; report != nil {
		return report
	}
	report := &RepositoryReport{}
	reports[key] = report
	return report
}

func appendProjectName(report *RepositoryReport, name string) {
	if name == "" {
		return
	}
	for _, existing := range report.ProjectNames {
		if existing == name {
			return
		}
	}
	report.ProjectNames = append(report.ProjectNames, name)
}

func addFinding(report *RepositoryReport, finding Finding) {
	for _, existing := range report.Findings {
		if existing.Code == finding.Code && pathKey(existing.Path) == pathKey(finding.Path) {
			return
		}
	}
	report.Findings = append(report.Findings, finding)
}

func addClaim(claims map[string][]string, target string, path string) {
	key := pathKey(target)
	for _, existing := range claims[key] {
		if pathKey(existing) == pathKey(path) {
			return
		}
	}
	claims[key] = append(claims[key], path)
	sort.Strings(claims[key])
}

func claimantPaths(
	claims map[string][]string,
	inspection gitadapter.WorktreeInspection,
) []string {
	unique := make(map[string]string)
	for _, target := range []string{inspection.DotGitTarget, inspection.GitDir} {
		if target == "" {
			continue
		}
		for _, claimant := range claims[pathKey(target)] {
			unique[pathKey(claimant)] = claimant
		}
	}
	result := make([]string, 0, len(unique))
	for _, claimant := range unique {
		result = append(result, claimant)
	}
	sort.Strings(result)
	return result
}

func readDotGitTarget(path string) (string, error) {
	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Clean(dotGit), nil
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(string(data))
	if !strings.HasPrefix(target, "gitdir: ") {
		return "", fmt.Errorf("invalid .git file")
	}
	target = strings.TrimSpace(strings.TrimPrefix(target, "gitdir: "))
	if !filepath.IsAbs(target) {
		target = filepath.Join(path, target)
	}
	return filepath.Clean(target), nil
}

func pathExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return true, nil
		}
		if _, targetErr := os.Stat(path); targetErr != nil {
			if os.IsNotExist(targetErr) {
				return false, fmt.Errorf("path is a dangling symbolic link: %w", targetErr)
			}
			return false, targetErr
		}
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func pathKey(path string) string {
	return registry.PathKey(path)
}
