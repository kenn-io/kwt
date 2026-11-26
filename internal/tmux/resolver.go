package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type endpointCommand interface {
	ListSessionsContext(context.Context) ([]string, error)
	SessionUserOptionContext(context.Context, string, string) (string, error)
	ServerPIDContext(context.Context) (string, error)
}

type workspaceSessionRecord struct {
	Name         string
	Identity     string
	Generation   string
	MarkersKnown bool
}

type batchEndpointCommand interface {
	ListWorkspaceSessionsContext(context.Context) ([]workspaceSessionRecord, error)
}

func listWorkspaceSessions(
	ctx context.Context,
	command endpointCommand,
) ([]string, map[string]workspaceSessionRecord, error) {
	if batch, ok := command.(batchEndpointCommand); ok {
		records, err := batch.ListWorkspaceSessionsContext(ctx)
		if err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(records))
		byName := make(map[string]workspaceSessionRecord, len(records))
		for _, record := range records {
			names = append(names, record.Name)
			byName[record.Name] = record
		}
		return names, byName, nil
	}
	names, err := command.ListSessionsContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	return names, nil, nil
}

// WorkspaceEndpointRequest is the identity required to resolve an existing
// workspace session. Directory workspaces omit the generation.
type WorkspaceEndpointRequest struct {
	SessionName         string
	WorkspacePath       string
	WorkspaceGeneration string
}

// WorkspaceSessionResolution is one inventory-oriented endpoint result.
// Err describes why the intended endpoint had to be returned instead of a
// verified live session; callers must not use this best-effort result to
// establish or attach a session without resolving it again.
type WorkspaceSessionResolution struct {
	Session WorkspaceSession
	Err     error
}

type resolvedWorkspaceSession struct {
	WorkspaceSession
	adopted bool
}

func newResolvedWorkspaceSession(
	sessionName string,
	live bool,
	adopted bool,
) resolvedWorkspaceSession {
	socketName := KWTServerSocketName
	if adopted {
		socketName = ""
	}
	return resolvedWorkspaceSession{
		WorkspaceSession: WorkspaceSession{
			Endpoint: SessionEndpoint{
				SessionName: sessionName,
				SocketName:  socketName,
			},
			Live: live,
		},
		adopted: adopted,
	}
}

// endpointResolver chooses the KWT server or a verified live default-server
// session. It never migrates or modifies sessions.
type endpointResolver struct {
	canonical        endpointCommand
	defaultServer    endpointCommand
	reportDiagnostic func(error)
}

func newEndpointResolver(
	canonical endpointCommand,
	defaultServer endpointCommand,
	reportDiagnostic func(error),
) *endpointResolver {
	if reportDiagnostic == nil {
		reportDiagnostic = func(error) {}
	}
	return &endpointResolver{
		canonical:        canonical,
		defaultServer:    defaultServer,
		reportDiagnostic: reportDiagnostic,
	}
}

func (r *endpointResolver) resolve(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) (resolvedWorkspaceSession, error) {
	intended := newResolvedWorkspaceSession(request.SessionName, false, false)
	canonicalNames, canonicalRecords, err := listWorkspaceSessions(ctx, r.canonical)
	if err != nil {
		return resolvedWorkspaceSession{}, fmt.Errorf(
			"inspect canonical tmux endpoint: %w",
			err,
		)
	}
	if candidate, ok := workspaceSessionCandidate(canonicalNames, request); ok {
		candidateRequest := request
		candidateRequest.SessionName = candidate
		matches, inspectErr := inspectWorkspaceCandidateWithRecords(
			ctx,
			r.canonical,
			canonicalRecords,
			candidateRequest,
			true,
		)
		if inspectErr != nil {
			return resolvedWorkspaceSession{}, inspectErr
		}
		if matches {
			return newResolvedWorkspaceSession(candidate, true, false), nil
		}
	}

	// compat(kag1): default-server adoption
	defaultNames, defaultRecords, err := listWorkspaceSessions(ctx, r.defaultServer)
	if err != nil {
		r.reportDiagnostic(fmt.Errorf("default-server adoption lookup degraded: %w", err))
		return intended, nil
	}
	adopted, found, err := r.adoptDefaultServerSession(ctx, request, defaultNames, defaultRecords)
	if err != nil {
		return resolvedWorkspaceSession{}, err
	}
	if found {
		return adopted, nil
	}
	return intended, nil
}

// liveEndpoints returns every verified live endpoint for cleanup operations.
// Unlike attachment resolution, it does not stop after finding the preferred
// canonical endpoint.
func (r *endpointResolver) liveEndpoints(
	ctx context.Context,
	request WorkspaceEndpointRequest,
) ([]SessionEndpoint, error) {
	canonicalNames, canonicalRecords, err := listWorkspaceSessions(ctx, r.canonical)
	if err != nil {
		return nil, fmt.Errorf("inspect canonical tmux endpoint: %w", err)
	}
	// compat(kag1): default-server adoption
	defaultNames, defaultRecords, err := listWorkspaceSessions(ctx, r.defaultServer)
	if err != nil {
		return nil, fmt.Errorf("inspect default tmux endpoint: %w", err)
	}

	endpoints := make([]SessionEndpoint, 0, 2)
	for _, candidate := range workspaceCleanupCandidates(canonicalNames, request) {
		candidateRequest := request
		candidateRequest.SessionName = candidate
		matches, inspectErr := inspectWorkspaceCleanupCandidateWithRecords(
			ctx,
			r.canonical,
			canonicalRecords,
			candidateRequest,
			candidate == request.SessionName,
		)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if matches {
			endpoints = append(endpoints, canonicalEndpoint(candidate))
		}
	}
	// compat(kag1): default-server adoption
	for _, candidate := range workspaceCleanupCandidates(defaultNames, request) {
		candidateRequest := request
		candidateRequest.SessionName = candidate
		matches, inspectErr := inspectWorkspaceCleanupCandidateWithRecords(
			ctx,
			r.defaultServer,
			defaultRecords,
			candidateRequest,
			false,
		)
		if inspectErr != nil {
			return nil, fmt.Errorf(
				"inspect adopted default-server workspace: %w",
				inspectErr,
			)
		}
		if matches {
			endpoints = append(endpoints, SessionEndpoint{SessionName: candidate})
		}
	}
	return endpoints, nil
}

// resolveAll resolves an inventory snapshot while enumerating each server at
// most once. Exact candidates still receive the same marker checks as resolve.
func (r *endpointResolver) resolveAll(
	ctx context.Context,
	requests []WorkspaceEndpointRequest,
) ([]resolvedWorkspaceSession, error) {
	results, err := r.resolveEach(ctx, requests, false)
	if err != nil {
		return nil, err
	}
	resolved := make([]resolvedWorkspaceSession, len(results))
	for index := range results {
		if results[index].Err != nil {
			return nil, results[index].Err
		}
		resolved[index] = resolvedWorkspaceSession{
			WorkspaceSession: results[index].Session,
		}
	}
	return resolved, nil
}

// resolveAllBestEffort preserves one result per inventory request. Marker
// inspection failures degrade only that request to its intended canonical
// endpoint. When the canonical server cannot be listed, every request
// degrades to its intended endpoint, not live, with one diagnostic; a missing
// tmux binary is left to the caller, which treats it as no sessions.
func (r *endpointResolver) resolveAllBestEffort(
	ctx context.Context,
	requests []WorkspaceEndpointRequest,
) ([]WorkspaceSessionResolution, error) {
	return r.resolveEach(ctx, requests, true)
}

func (r *endpointResolver) resolveEach(
	ctx context.Context,
	requests []WorkspaceEndpointRequest,
	degradeUnlistedServer bool,
) ([]WorkspaceSessionResolution, error) {
	if len(requests) == 0 {
		return []WorkspaceSessionResolution{}, nil
	}
	canonicalNames, canonicalRecords, err := listWorkspaceSessions(ctx, r.canonical)
	if cancellationErr := resolutionCancellation(ctx, err); cancellationErr != nil {
		return nil, cancellationErr
	}
	if err != nil {
		if !degradeUnlistedServer || errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("inspect canonical tmux endpoint: %w", err)
		}
		r.reportDiagnostic(fmt.Errorf("cannot list sessions on the kwt tmux server: %w", err))
		results := make([]WorkspaceSessionResolution, len(requests))
		for index := range requests {
			results[index].Session = newResolvedWorkspaceSession(
				requests[index].SessionName, false, false,
			).WorkspaceSession
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return results, nil
	}
	// compat(kag1): default-server adoption
	defaultNames, defaultRecords, defaultErr := listWorkspaceSessions(ctx, r.defaultServer)
	if cancellationErr := resolutionCancellation(ctx, defaultErr); cancellationErr != nil {
		return nil, cancellationErr
	}
	if defaultErr != nil {
		r.reportDiagnostic(fmt.Errorf("default-server adoption lookup degraded: %w", defaultErr))
		defaultNames = nil
		defaultRecords = nil
	}

	results := make([]WorkspaceSessionResolution, 0, len(requests))
	for _, request := range requests {
		endpoint, resolveErr := r.resolveFromInventory(
			ctx,
			request,
			canonicalNames,
			canonicalRecords,
			defaultNames,
			defaultRecords,
			defaultErr == nil,
		)
		if resolveErr != nil {
			if cancellationErr := resolutionCancellation(ctx, resolveErr); cancellationErr != nil {
				return nil, cancellationErr
			}
			results = append(results, WorkspaceSessionResolution{
				Session: newResolvedWorkspaceSession(
					request.SessionName, false, false,
				).WorkspaceSession,
				Err: resolveErr,
			})
			continue
		}
		results = append(results, WorkspaceSessionResolution{
			Session: endpoint.WorkspaceSession,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func resolutionCancellation(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func (r *endpointResolver) resolveFromInventory(
	ctx context.Context,
	request WorkspaceEndpointRequest,
	canonicalNames []string,
	canonicalRecords map[string]workspaceSessionRecord,
	defaultNames []string,
	defaultRecords map[string]workspaceSessionRecord,
	defaultAvailable bool,
) (resolvedWorkspaceSession, error) {
	intended := newResolvedWorkspaceSession(request.SessionName, false, false)
	if candidate, ok := workspaceSessionCandidate(canonicalNames, request); ok {
		candidateRequest := request
		candidateRequest.SessionName = candidate
		matches, err := inspectWorkspaceCandidateWithRecords(
			ctx,
			r.canonical,
			canonicalRecords,
			candidateRequest,
			true,
		)
		if err != nil {
			return resolvedWorkspaceSession{}, err
		}
		if matches {
			return newResolvedWorkspaceSession(candidate, true, false), nil
		}
	}
	if !defaultAvailable {
		return intended, nil
	}
	adopted, found, err := r.adoptDefaultServerSession(ctx, request, defaultNames, defaultRecords)
	if err != nil {
		return resolvedWorkspaceSession{}, err
	}
	if found {
		return adopted, nil
	}
	return intended, nil
}

// compat(kag1): default-server adoption
func (r *endpointResolver) adoptDefaultServerSession(
	ctx context.Context,
	request WorkspaceEndpointRequest,
	names []string,
	records map[string]workspaceSessionRecord,
) (resolvedWorkspaceSession, bool, error) {
	candidate, ok := workspaceSessionCandidate(names, request)
	if !ok {
		return resolvedWorkspaceSession{}, false, nil
	}
	candidateRequest := request
	candidateRequest.SessionName = candidate
	matches, err := inspectWorkspaceCandidateWithRecords(
		ctx,
		r.defaultServer,
		records,
		candidateRequest,
		false,
	)
	if err != nil {
		return resolvedWorkspaceSession{}, false, fmt.Errorf(
			"inspect adopted default-server workspace: %w",
			err,
		)
	}
	if !matches {
		return resolvedWorkspaceSession{}, false, nil
	}
	return newResolvedWorkspaceSession(candidate, true, true), true, nil
}

func workspaceSessionCandidate(
	names []string,
	request WorkspaceEndpointRequest,
) (string, bool) {
	if containsExactSession(names, request.SessionName) {
		return request.SessionName, true
	}
	if IsKWTDirectoryWorkspaceSessionName(request.SessionName) {
		return MatchDirWorkspaceSession(names, request.WorkspacePath)
	}
	// compat(kag1): pre-dollar tmux session names. The path hash survives
	// sanitization changes; marker inspection still proves the candidate's
	// identity and generation before it can be used.
	for _, name := range names {
		if MatchesLegacyWorkspaceSessionPath(name, request.WorkspacePath) {
			return name, true
		}
	}
	return "", false
}

func workspaceCleanupCandidates(
	names []string,
	request WorkspaceEndpointRequest,
) []string {
	directoryWorkspace := request.WorkspaceGeneration == "" &&
		IsKWTDirectoryWorkspaceSessionName(request.SessionName)
	candidates := make([]string, 0, len(names))
	for _, name := range names {
		if directoryWorkspace {
			if IsKWTDirectoryWorkspaceSessionName(name) {
				candidates = append(candidates, name)
			}
			continue
		}
		if IsKWTWorktreeSessionName(name) {
			candidates = append(candidates, name)
		}
	}
	return candidates
}

func containsExactSession(names []string, expected string) bool {
	for _, name := range names {
		if name == expected {
			return true
		}
	}
	return false
}

func inspectWorkspaceCandidateWithRecords(
	ctx context.Context,
	command endpointCommand,
	records map[string]workspaceSessionRecord,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	if record, ok := records[request.SessionName]; ok && record.MarkersKnown {
		matches, err := validateWorkspaceIdentity(record.Identity, request, canonical)
		if err != nil || !matches {
			return matches, err
		}
		if request.WorkspaceGeneration == "" {
			return true, nil
		}
		return validateWorkspaceGeneration(record.Generation, request, canonical)
	}
	return inspectWorkspaceCandidate(ctx, command, request, canonical)
}

func inspectWorkspaceCleanupCandidateWithRecords(
	ctx context.Context,
	command endpointCommand,
	records map[string]workspaceSessionRecord,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	if request.WorkspaceGeneration != "" {
		if record, ok := records[request.SessionName]; ok && record.MarkersKnown {
			return validateWorkspaceGeneration(record.Generation, request, canonical)
		}
		return inspectWorkspaceGenerationCandidate(ctx, command, request, canonical)
	}
	return inspectWorkspaceCandidateWithRecords(ctx, command, records, request, canonical)
}

func inspectWorkspaceCandidate(
	ctx context.Context,
	command endpointCommand,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	identity, err := command.SessionUserOptionContext(
		ctx,
		request.SessionName,
		workspaceIdentityOption,
	)
	if err != nil {
		return false, fmt.Errorf("read workspace identity marker: %w", err)
	}
	matches, err := validateWorkspaceIdentity(identity, request, canonical)
	if err != nil || !matches {
		return matches, err
	}
	if request.WorkspaceGeneration == "" {
		return true, nil
	}
	return inspectWorkspaceGenerationCandidate(ctx, command, request, canonical)
}

func validateWorkspaceIdentity(
	identity string,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q lacks workspace identity markers; remove it before reopening the workspace",
				request.SessionName,
			)}
		}
		return false, nil
	}
	if !validLowerHex(identity, 64) {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q has malformed workspace identity markers",
				request.SessionName,
			)}
		}
		return false, errors.New("malformed workspace identity marker")
	}
	if identity != WorkspacePathIdentity(request.WorkspacePath) {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q belongs to a different workspace identity",
				request.SessionName,
			)}
		}
		return false, nil
	}
	return true, nil
}

func inspectWorkspaceGenerationCandidate(
	ctx context.Context,
	command endpointCommand,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	generation, err := command.SessionUserOptionContext(
		ctx,
		request.SessionName,
		workspaceGenerationOption,
	)
	if err != nil {
		return false, fmt.Errorf("read workspace generation marker: %w", err)
	}
	return validateWorkspaceGeneration(generation, request, canonical)
}

func validateWorkspaceGeneration(
	generation string,
	request WorkspaceEndpointRequest,
	canonical bool,
) (bool, error) {
	generation = strings.TrimSpace(generation)
	if generation == "" {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q lacks worktree identity markers; remove it before reopening the worktree",
				request.SessionName,
			)}
		}
		return false, nil
	}
	if !validLowerHex(generation, 32) {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q has malformed worktree generation markers",
				request.SessionName,
			)}
		}
		return false, errors.New("malformed workspace generation marker")
	}
	if generation != request.WorkspaceGeneration {
		if canonical {
			return false, &SessionSafetyError{Reason: fmt.Sprintf(
				"tmux session %q belongs to a different worktree generation",
				request.SessionName,
			)}
		}
		return false, nil
	}
	return true, nil
}

func parseTMUXServerPID(value string) (uint64, error) {
	lastComma := strings.LastIndexByte(value, ',')
	if lastComma <= 0 || lastComma == len(value)-1 {
		return 0, errors.New("TMUX must contain socket, server PID, and pane index")
	}
	remaining := value[:lastComma]
	secondComma := strings.LastIndexByte(remaining, ',')
	if secondComma <= 0 || secondComma == len(remaining)-1 {
		return 0, errors.New("TMUX must contain socket, server PID, and pane index")
	}
	return parseCanonicalPID(remaining[secondComma+1:])
}

func parseCanonicalPID(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("invalid numeric PID %q", value)
	}
	return parsed, nil
}
