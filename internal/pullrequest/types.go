// Package pullrequest owns provider-neutral pull-request discovery and import.
package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kwt/pkg/models"
)

type ErrorCode string

const (
	CodeAuthentication        ErrorCode = "authentication_failed"
	CodeRepositoryMismatch    ErrorCode = "repository_mismatch"
	CodeNotFound              ErrorCode = "pull_request_not_found"
	CodeInaccessibleHead      ErrorCode = "inaccessible_head"
	CodeNamingConflict        ErrorCode = "naming_conflict"
	CodeNetwork               ErrorCode = "network_failure"
	CodeWorkspaceCreation     ErrorCode = "workspace_creation_failed"
	CodeMalformedResponse     ErrorCode = "malformed_provider_response"
	CodeConflict              ErrorCode = "import_conflict"
	CodeInvalidSelector       ErrorCode = "invalid_pull_request_selector"
	CodeUnsupportedProvider   ErrorCode = "unsupported_provider"
	CodeUnsupportedGitVersion ErrorCode = "unsupported_git_version"
)

// Error is the stable failure contract used by both the service and JSON CLI.
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Retryable bool      `json:"retryable"`
	cause     error
}

func NewError(code ErrorCode, message string, retryable bool, cause error) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable, cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.cause }

func AsError(err error, fallback ErrorCode, message string) *Error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	return NewError(fallback, message, false, err)
}

type Repository struct {
	Provider string `json:"provider"`
	Identity string `json:"identity"`
	Host     string `json:"host"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	CloneURL string `json:"-"`
	SSHURL   string `json:"-"`
}

type Project struct {
	Identity string `json:"identity"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

type Branch struct {
	Name       string     `json:"branch"`
	Repository Repository `json:"repository"`
	IsFork     bool       `json:"is_fork"`
}

type PullRequest struct {
	ID         string     `json:"id"`
	Provider   string     `json:"provider"`
	Repository Repository `json:"repository"`
	Number     int        `json:"number"`
	URL        string     `json:"url"`
	Title      string     `json:"title"`
	Author     string     `json:"author"`
	Source     Branch     `json:"source"`
	Target     Branch     `json:"target"`
	Draft      bool       `json:"draft"`
	State      string     `json:"state"`
	HeadSHA    string     `json:"head_sha"`
	MergedAt   *time.Time `json:"merged_at,omitempty"`
	Imported   bool       `json:"imported"`
	Workspace  *Workspace `json:"workspace,omitempty"`
}

type Workspace struct {
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	Generation  string `json:"generation,omitempty"`
	State       string `json:"state"`
	SessionName string `json:"session_name"`
	// TmuxSocketName is empty when protection is known but repository identity
	// is insufficient to publish an attachable endpoint.
	TmuxSocketName string                `json:"tmux_socket_name,omitempty"`
	TmuxAttachMode models.TmuxAttachMode `json:"tmux_attach_mode"`
	partialCleanup *workspacePartialCleanup
	// preserveOnImportError means Kit could not prove that cleanup was safe.
	// Other post-creation failures retain rollback metadata and are cleaned up.
	preserveOnImportError bool
}

type workspacePartialCleanup struct {
	run func(context.Context) error
}

type Provenance struct {
	PullRequestID     string    `json:"pull_request_id"`
	Provider          string    `json:"provider"`
	Repository        string    `json:"repository"`
	RepositoryAliases []string  `json:"repository_aliases,omitempty"`
	Number            int       `json:"number"`
	URL               string    `json:"url"`
	HeadSHA           string    `json:"head_sha"`
	SourceRepo        string    `json:"source_repository"`
	SourceBranch      string    `json:"source_branch"`
	Project           Project   `json:"project"`
	Workspace         Workspace `json:"workspace"`
}

type ImportStatus string

const (
	ImportCreated  ImportStatus = "created"
	ImportExisting ImportStatus = "already_imported"
)

type ImportResult struct {
	Status            ImportStatus `json:"status"`
	PullRequest       PullRequest  `json:"pull_request"`
	Project           Project      `json:"project"`
	Workspace         Workspace    `json:"workspace"`
	SessionStartError *Error       `json:"session_start_error,omitempty"`
}

type ErrorEnvelope struct {
	Error *Error `json:"error"`
}

func OpaqueID(repository string, number int) string {
	return fmt.Sprintf("github:%s#%d", NormalizeRepositoryIdentity(repository), number)
}

// NormalizeRepositoryIdentity returns the stable provider identity used for
// comparisons and persistence. GitHub owner and repository names are
// case-insensitive.
func NormalizeRepositoryIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if strings.HasPrefix(strings.ToLower(identity), "github.com/") {
		return strings.ToLower(identity)
	}
	return identity
}

func EqualRepositoryIdentity(left, right string) bool {
	return NormalizeRepositoryIdentity(left) == NormalizeRepositoryIdentity(right)
}
