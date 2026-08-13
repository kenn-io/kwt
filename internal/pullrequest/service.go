package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kwt/internal/tmux"
	repositoryurl "go.kenn.io/kwt/internal/url"
	"go.kenn.io/kwt/internal/utils"
)

type Provider interface {
	List(context.Context, Repository, string) ([]PullRequest, error)
	Get(context.Context, Repository, int) (PullRequest, error)
}

type WorkspaceBackend interface {
	ListWorkspaces(context.Context) ([]Workspace, error)
	ImportPullRequest(context.Context, PullRequest, string) (Workspace, error)
	Rollback(context.Context, Workspace) error
}

type WorkspaceCreationCoordinator interface {
	AcquireWorkspaceCreation(context.Context, string) (func() error, error)
}

type Store interface {
	View(context.Context, func(map[string]Provenance) error) error
	Update(context.Context, func(map[string]Provenance) error) error
}

type Service struct {
	provider          Provider
	backend           WorkspaceBackend
	store             Store
	projectIdentities []string
}

func NewService(
	provider Provider,
	backend WorkspaceBackend,
	store Store,
	projectIdentities ...string,
) *Service {
	service := &Service{provider: provider, backend: backend, store: store}
	for _, identity := range projectIdentities {
		repository, err := RepositoryFromProject(Project{Identity: identity})
		if err != nil || service.hasProjectIdentity(repository.Identity) {
			continue
		}
		service.projectIdentities = append(
			service.projectIdentities,
			repository.Identity,
		)
	}
	return service
}

func (s *Service) List(ctx context.Context, project Project, state string) ([]PullRequest, error) {
	repository, err := RepositoryFromProject(project)
	if err != nil {
		return nil, err
	}
	project.Identity = repository.Identity
	prs, err := s.provider.List(ctx, repository, state)
	if err != nil {
		return nil, err
	}
	workspaces, err := s.backend.ListWorkspaces(ctx)
	if err != nil {
		return nil, NewError(CodeWorkspaceCreation, "failed to inspect project workspaces", false, err)
	}
	paths := make(map[string]Workspace, len(workspaces))
	for _, workspace := range workspaces {
		paths[utils.CanonicalPath(workspace.Path)] = workspace
	}
	records := make(map[string]Provenance)
	if err := s.store.View(ctx, func(current map[string]Provenance) error {
		for key, record := range current {
			records[key] = record
		}
		return nil
	}); err != nil {
		return nil, NewError(CodeWorkspaceCreation, "failed to read pull-request provenance", false, err)
	}
	for i := range prs {
		prs[i].Source.IsFork = !EqualRepositoryIdentity(prs[i].Source.Repository.Identity, prs[i].Repository.Identity)
		_, record, ok := s.findProvenance(records, prs[i])
		if ok && s.sameProjectClone(record, project) &&
			s.provenanceSourceMatches(record, prs[i]) {
			if workspace, live := matchingProvenanceWorkspace(paths, record); live {
				prs[i].Imported = true
				prs[i].Workspace = &workspace
			}
		}
	}
	return prs, nil
}

func (s *Service) Import(ctx context.Context, project Project, selector string) (result ImportResult, err error) {
	repository, err := RepositoryFromProject(project)
	if err != nil {
		return result, err
	}
	project.Identity = repository.Identity
	number, err := ParseSelector(selector, repository.Identity)
	if err != nil {
		return result, err
	}
	pr, err := s.provider.Get(ctx, repository, number)
	if err != nil {
		return result, err
	}
	if !EqualRepositoryIdentity(pr.Repository.Identity, repository.Identity) {
		return result, NewError(CodeRepositoryMismatch,
			fmt.Sprintf("pull request belongs to %s, not project %s", pr.Repository.Identity, repository.Identity),
			false, nil)
	}
	if strings.TrimSpace(pr.Source.Repository.Identity) == "" || strings.TrimSpace(pr.Source.Name) == "" {
		return result, NewError(CodeInaccessibleHead, "pull-request head repository or branch is unavailable", false, nil)
	}
	pr.Source.IsFork = !EqualRepositoryIdentity(pr.Source.Repository.Identity, pr.Repository.Identity)
	branch := importBranchName(pr)
	if coordinator, ok := s.backend.(WorkspaceCreationCoordinator); ok {
		release, acquireErr := coordinator.AcquireWorkspaceCreation(ctx, branch)
		if acquireErr != nil {
			return result, AsError(
				acquireErr,
				CodeWorkspaceCreation,
				"failed to coordinate pull-request workspace creation",
			)
		}
		defer func() {
			if releaseErr := release(); releaseErr != nil {
				err = errors.Join(
					err,
					NewError(
						CodeWorkspaceCreation,
						"failed to release pull-request workspace creation lock",
						false,
						releaseErr,
					),
				)
			}
		}()
	}
	var created *Workspace
	cleanupReason := "workspace created but provenance could not be persisted"

	err = s.store.Update(ctx, func(records map[string]Provenance) error {
		workspaces, listErr := s.backend.ListWorkspaces(ctx)
		if listErr != nil {
			return NewError(CodeWorkspaceCreation, "failed to inspect project workspaces", false, listErr)
		}
		byPath := make(map[string]Workspace, len(workspaces))
		for _, workspace := range workspaces {
			byPath[utils.CanonicalPath(workspace.Path)] = workspace
		}
		recordKey, record, ok := s.findProvenance(records, pr)
		if ok {
			if !s.sameProjectClone(record, project) {
				return NewError(
					CodeConflict,
					"pull request is recorded for a different project clone",
					false, nil,
				)
			}
			if workspace, live := matchingProvenanceWorkspace(byPath, record); live {
				if !provenanceSourceComplete(record) {
					return NewError(CodeConflict,
						"existing import has incomplete source provenance", false, nil)
				}
				if !s.provenanceSourceMatches(record, pr) {
					return NewError(CodeConflict,
						"pull-request source repository or branch changed after import", false, nil)
				}
				if recordKey != pr.ID {
					delete(records, recordKey)
				}
				aliases := s.mergedRepositoryAliases(record)
				record.PullRequestID = pr.ID
				record.Provider = pr.Provider
				record.Repository = pr.Repository.Identity
				record.RepositoryAliases = aliases
				record.Number = pr.Number
				record.URL = pr.URL
				record.SourceRepo = NormalizeRepositoryIdentity(pr.Source.Repository.Identity)
				record.SourceBranch = pr.Source.Name
				record.Project.Identity = project.Identity
				record.Workspace = workspace
				records[pr.ID] = record
				result = ImportResult{Status: ImportExisting, PullRequest: pr, Project: project, Workspace: workspace}
				result.PullRequest.Imported = true
				result.PullRequest.Workspace = &result.Workspace
				return nil
			}
			if recordKey != pr.ID {
				delete(records, recordKey)
			}
		}

		workspace, createErr := s.backend.ImportPullRequest(ctx, pr, branch)
		if createErr != nil {
			if workspace.Path != "" || workspace.Branch != "" {
				if workspace.preserveOnImportError {
					return NewError(
						CodeWorkspaceCreation,
						fmt.Sprintf(
							"pull-request lifecycle preserved path %q and branch %q; manual cleanup is required",
							workspace.Path, workspace.Branch,
						),
						false, createErr,
					)
				}
				created = &workspace
				cleanupReason = "workspace setup failed"
			}
			return AsError(createErr, CodeWorkspaceCreation, "failed to create pull-request workspace")
		}
		created = &workspace

		newRecord := Provenance{
			PullRequestID: pr.ID, Provider: pr.Provider, Repository: pr.Repository.Identity,
			RepositoryAliases: s.mergedRepositoryAliases(Provenance{
				Repository: pr.Repository.Identity,
			}),
			Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA,
			SourceRepo: pr.Source.Repository.Identity, SourceBranch: pr.Source.Name,
			Project: project, Workspace: workspace,
		}
		records[pr.ID] = newRecord
		result = ImportResult{Status: ImportCreated, PullRequest: pr, Project: project, Workspace: workspace}
		result.PullRequest.Imported = true
		result.PullRequest.Workspace = &result.Workspace
		return nil
	})
	if err != nil {
		if created != nil {
			if rollbackErr := s.backend.Rollback(context.WithoutCancel(ctx), *created); rollbackErr != nil {
				return ImportResult{}, NewError(CodeWorkspaceCreation,
					fmt.Sprintf("%s and rollback failed; manual cleanup is required at %s for branch %q", cleanupReason, created.Path, created.Branch),
					false, errors.Join(err, rollbackErr))
			}
			var typed *Error
			if errors.As(err, &typed) {
				return ImportResult{}, NewError(
					typed.Code,
					typed.Message+"; "+cleanupReason+"; rolled it back",
					typed.Retryable,
					err,
				)
			}
			return ImportResult{}, NewError(CodeWorkspaceCreation, cleanupReason+"; rolled it back", false, err)
		}
		return ImportResult{}, AsError(err, CodeWorkspaceCreation, "pull-request import failed")
	}
	return result, err
}

func (s *Service) findProvenance(
	records map[string]Provenance,
	pr PullRequest,
) (string, Provenance, bool) {
	if record, ok := records[pr.ID]; ok {
		return pr.ID, record, true
	}
	for key, record := range records {
		if record.Number != pr.Number ||
			!s.recordRepositoryMatches(record, pr.Repository.Identity) {
			continue
		}
		if record.Provider != "" && !strings.EqualFold(record.Provider, pr.Provider) {
			continue
		}
		return key, record, true
	}
	return "", Provenance{}, false
}

func (s *Service) sameProjectClone(record Provenance, current Project) bool {
	if utils.CanonicalPath(record.Project.Path) !=
		utils.CanonicalPath(current.Path) {
		return false
	}
	if EqualRepositoryIdentity(record.Project.Identity, current.Identity) {
		return true
	}
	return ProvenanceHasRepositoryIdentity(
		record,
		record.Project.Identity,
	) && s.hasProjectIdentity(
		current.Identity,
	) && s.recordAliasesConnected(record)
}

func matchingProvenanceWorkspace(byPath map[string]Workspace, record Provenance) (Workspace, bool) {
	workspace, ok := byPath[utils.CanonicalPath(record.Workspace.Path)]
	if !ok || workspace.Branch != record.Workspace.Branch ||
		(record.Workspace.Generation != "" &&
			workspace.Generation != record.Workspace.Generation) {
		return Workspace{}, false
	}
	for _, identity := range ProvenanceRepositoryIdentities(record) {
		info, valid := repositoryurl.CanonicalRepositoryInfo(identity)
		if !valid || !tmux.MatchesWorkspaceSessionName(
			record.Workspace.SessionName,
			info,
			record.Workspace.Branch,
			record.Workspace.Path,
		) {
			continue
		}
		workspace.Path = record.Workspace.Path
		workspace.SessionName = record.Workspace.SessionName
		workspace.TmuxSocketName = tmux.ProtectedWorkspaceSocketName(
			record.Workspace.SessionName,
			record.Workspace.Path,
		)
		break
	}
	return workspace, true
}

func (s *Service) provenanceSourceMatches(record Provenance, pr PullRequest) bool {
	if !provenanceSourceComplete(record) || record.SourceBranch != pr.Source.Name {
		return false
	}
	if EqualRepositoryIdentity(record.SourceRepo, pr.Source.Repository.Identity) {
		return true
	}
	return ProvenanceHasRepositoryIdentity(
		record,
		record.SourceRepo,
	) && EqualRepositoryIdentity(
		pr.Source.Repository.Identity,
		pr.Repository.Identity,
	) && s.hasProjectIdentity(
		pr.Repository.Identity,
	) && s.recordAliasesConnected(record)
}

func provenanceSourceComplete(record Provenance) bool {
	return strings.TrimSpace(record.SourceRepo) != "" && strings.TrimSpace(record.SourceBranch) != ""
}

func (s *Service) recordRepositoryMatches(
	record Provenance,
	current string,
) bool {
	if EqualRepositoryIdentity(record.Repository, current) {
		return true
	}
	return ProvenanceHasRepositoryIdentity(
		record,
		record.Repository,
	) && s.hasProjectIdentity(
		current,
	) && s.recordAliasesConnected(record)
}

func (s *Service) hasProjectIdentity(identity string) bool {
	for _, candidate := range s.projectIdentities {
		if EqualRepositoryIdentity(candidate, identity) {
			return true
		}
	}
	return false
}

func (s *Service) recordAliasesConnected(record Provenance) bool {
	for _, identity := range provenanceRepositoryIdentities(record) {
		if s.hasProjectIdentity(identity) {
			return true
		}
	}
	return false
}

func (s *Service) mergedRepositoryAliases(record Provenance) []string {
	identities := provenanceRepositoryIdentities(record)
	for _, identity := range s.projectIdentities {
		identities = appendCanonicalRepositoryIdentity(
			identities,
			identity,
		)
	}
	if len(identities) < 2 {
		return nil
	}
	return identities
}

// ProvenanceHasRepositoryIdentity reports whether identity belongs to the
// canonical repository alias history recorded for an import.
func ProvenanceHasRepositoryIdentity(
	record Provenance,
	identity string,
) bool {
	for _, candidate := range provenanceRepositoryIdentities(record) {
		if EqualRepositoryIdentity(candidate, identity) {
			return true
		}
	}
	return false
}

func provenanceRepositoryIdentities(record Provenance) []string {
	repository, err := RepositoryFromProject(Project{
		Identity: record.Repository,
	})
	if err != nil {
		return nil
	}
	if len(record.RepositoryAliases) == 0 {
		return []string{repository.Identity}
	}
	var identities []string
	for _, identity := range record.RepositoryAliases {
		identities = appendCanonicalRepositoryIdentity(
			identities,
			identity,
		)
	}
	foundRepository := false
	for _, identity := range identities {
		if EqualRepositoryIdentity(identity, repository.Identity) {
			foundRepository = true
			break
		}
	}
	if !foundRepository {
		return nil
	}
	return identities
}

// ProvenanceRepositoryIdentities returns the credential-free repository
// identities connected by a durable import record's alias history.
func ProvenanceRepositoryIdentities(record Provenance) []string {
	identities := provenanceRepositoryIdentities(record)
	if len(identities) != 0 || record.Repository != "" {
		return append([]string(nil), identities...)
	}
	identities = appendCanonicalRepositoryIdentity(
		identities, record.Project.Identity,
	)
	identities = appendCanonicalRepositoryIdentity(
		identities, record.Workspace.Repository,
	)
	return identities
}

func appendCanonicalRepositoryIdentity(
	identities []string,
	identity string,
) []string {
	repository, err := RepositoryFromProject(Project{Identity: identity})
	if err != nil {
		return identities
	}
	for _, candidate := range identities {
		if EqualRepositoryIdentity(candidate, repository.Identity) {
			return identities
		}
	}
	return append(identities, repository.Identity)
}

// RepositoryFromProject validates and parses a GitHub project identity.
func RepositoryFromProject(project Project) (Repository, error) {
	identity := NormalizeRepositoryIdentity(project.Identity)
	parts := strings.Split(identity, "/")
	if len(parts) != 3 || parts[0] != "github.com" || parts[1] == "" || parts[2] == "" {
		return Repository{}, NewError(CodeUnsupportedProvider,
			fmt.Sprintf("project %q is not a supported GitHub repository identity", project.Identity), false, nil)
	}
	return Repository{
		Provider: "github", Identity: identity, Host: parts[0], Owner: parts[1], Name: parts[2],
	}, nil
}

func ParseSelector(selector, repository string) (int, error) {
	number, selectorRepository, err := parseSelector(selector)
	if err != nil {
		return 0, err
	}
	if selectorRepository != "" &&
		!EqualRepositoryIdentity(selectorRepository, repository) {
		return 0, NewError(
			CodeInvalidSelector,
			"pull-request selector does not match the selected repository",
			false,
			nil,
		)
	}
	return number, nil
}

// ParseSelectorNumber validates a selector's provider syntax and returns its
// pull-request number without authorizing its repository identity.
func ParseSelectorNumber(selector string) (int, error) {
	number, _, err := parseSelector(selector)
	return number, err
}

func parseSelector(selector string) (int, string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return 0, "", NewError(
			CodeInvalidSelector,
			"pull-request selector is empty",
			false,
			nil,
		)
	}
	if number, err := strconv.Atoi(selector); err == nil && number > 0 {
		return number, "", nil
	}
	if strings.HasPrefix(strings.ToLower(selector), "github:") {
		identity, numberText, ok := strings.Cut(selector[len("github:"):], "#")
		parts := strings.Split(NormalizeRepositoryIdentity(identity), "/")
		if ok && len(parts) == 3 && parts[0] == "github.com" &&
			parts[1] != "" && parts[2] != "" {
			if number, err := strconv.Atoi(numberText); err == nil && number > 0 {
				return number, strings.Join(parts, "/"), nil
			}
		}
	}
	parsed, err := url.Parse(selector)
	if err == nil && strings.EqualFold(parsed.Host, "github.com") {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 4 && parts[0] != "" && parts[1] != "" &&
			strings.EqualFold(parts[2], "pull") {
			if number, convertErr := strconv.Atoi(parts[3]); convertErr == nil && number > 0 {
				return number, NormalizeRepositoryIdentity(
					"github.com/" + parts[0] + "/" + parts[1],
				), nil
			}
		}
	}
	return 0, "", NewError(
		CodeInvalidSelector,
		"pull-request selector does not match the selected repository",
		false,
		nil,
	)
}

func importBranchName(pr PullRequest) string {
	var out strings.Builder
	lastDash := false
	for _, r := range pr.Source.Name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(out.String(), "-.")
	if slug == "" {
		slug = "head"
	}
	if len(slug) > 80 {
		end := 80
		for end > 0 && !utf8.RuneStart(slug[end]) {
			end--
		}
		slug = strings.TrimRight(slug[:end], "-.")
		if strings.HasSuffix(strings.ToLower(slug), ".lock") {
			slug = slug[:len(slug)-len(".lock")] + "-lock"
		}
	}
	return fmt.Sprintf("pr-%d-%s", pr.Number, slug)
}
