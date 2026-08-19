package tmux

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kwt/internal/template"
	"go.kenn.io/kwt/internal/url"
)

// Session is KWT's internal observation of one managed tmux session. Wire
// commands map it to their own response types rather than serializing it.
type Session struct {
	SessionName string
	SocketName  string
	ID          string
	Context     string
	Identifier  string
	WorkingDir  string
	Command     string
	StartTime   time.Time
	HistorySize int
	Metadata    map[string]string
}

type SessionOptions struct {
	Context    string
	Identifier string
	WorkingDir string
	Command    string
	Metadata   map[string]string
}

type SessionConfig struct {
	Enabled      bool   `toml:"enabled" json:"enabled"`
	TmuxCommand  string `toml:"tmux_command" json:"tmux_command"`
	HistoryLimit int    `toml:"history_limit" json:"history_limit"`
}

// SessionEndpointLabel returns the human-readable server identity for a KWT
// tmux session.
func SessionEndpointLabel(session *Session) string {
	if session == nil || session.SocketName == "" {
		return "default"
	}
	return session.SocketName
}

func DefaultSessionConfig() *SessionConfig {
	return &SessionConfig{
		Enabled:      true,
		TmuxCommand:  "tmux",
		HistoryLimit: 50000,
	}
}

// WorkspaceSessionName returns a stable, collision-resistant, tmux-safe session
// name for a worktree workspace: kwt-wt-{repo}-{branch}-{hash}.
// The hash is computed over worktreePath, which is globally unique per worktree
// (unlike info+branch: two detached-HEAD worktrees of the same repo both report
// branch "HEAD" and would otherwise collide on the same session name).
func WorkspaceSessionName(info *url.RepositoryInfo, branch, worktreePath string) string {
	return sanitizeTmuxName(workspaceSessionNameRaw(info, branch, worktreePath))
}

// MatchesWorkspaceSessionName verifies the deterministic session identities
// used by current and previously imported protected pull-request workspaces.
func MatchesWorkspaceSessionName(
	name string,
	info *url.RepositoryInfo,
	branch, worktreePath string,
) bool {
	currentRaw := workspaceSessionNameRaw(info, branch, worktreePath)
	previousRaw := previousWorkspaceSessionNameRaw(info, branch, worktreePath)
	return name == sanitizeTmuxName(currentRaw) ||
		name == sanitizeTmuxName(previousRaw) ||
		// compat(kag1): pre-dollar tmux session names
		name == sanitizeTmuxNameBeforeDollarNormalization(currentRaw) ||
		name == sanitizeTmuxNameBeforeDollarNormalization(previousRaw)
}

func workspaceSessionNameRaw(
	info *url.RepositoryInfo,
	branch, worktreePath string,
) string {
	hash := template.ShortHash(worktreePath)
	return fmt.Sprintf("kwt-wt-%s-%s-%s", info.Repository, branch, hash)
}

func previousWorkspaceSessionNameRaw(
	info *url.RepositoryInfo,
	branch, worktreePath string,
) string {
	hash := template.ShortHash(worktreePath)
	return fmt.Sprintf("kwt-workspace-%s-%s-%s-%s-%s",
		info.Host, info.Owner, info.Repository, branch, hash)
}

// MatchesLegacyWorkspaceSessionPath recognizes pre-marker KWT sessions by
// the path-derived suffix shared by the current and previous naming schemes.
// The repository and branch portions are intentionally not compared because
// either can change while a legacy client remains attached to the worktree.
func MatchesLegacyWorkspaceSessionPath(name, worktreePath string) bool {
	suffix := "-" + template.ShortHash(worktreePath)
	return isKWTSessionName(name) &&
		strings.HasSuffix(name, suffix)
}

func isKWTSessionName(name string) bool {
	return strings.HasPrefix(name, "kwt-wt-") ||
		strings.HasPrefix(name, "kwt-workspace-")
}

// IsKWTWorktreeSessionName reports whether name may belong to a Git worktree.
// The current kwt-wt namespace is unambiguous. The legacy kwt-workspace
// namespace is not: its next component may be either a repository host or the
// directory-workspace marker, and "dir" is a valid host alias. Callers may
// exempt a directory session only when independent workspace markers prove it.
func IsKWTWorktreeSessionName(name string) bool {
	return isKWTSessionName(name)
}

// sanitizeTmuxName replaces characters that are disallowed or not preserved
// consistently in tmux session names, plus path separators and whitespace,
// with '-'. tmux 3.4 rewrites '$' during new-session, which makes later
// commands unable to target the requested spelling.
func sanitizeTmuxName(s string) string {
	return strings.NewReplacer(
		".", "-", ":", "-", "$", "-", "/", "-", " ", "-", "\t", "-",
	).Replace(s)
}

// compat(kag1): pre-dollar tmux session names
func sanitizeTmuxNameBeforeDollarNormalization(s string) string {
	return strings.NewReplacer(
		".", "-", ":", "-", "/", "-", " ", "-", "\t", "-",
	).Replace(s)
}

const dirWorkspaceSessionPrefix = "kwt-workspace-dir-"

// IsKWTDirectoryWorkspaceSessionName reports whether name is in the managed
// directory-workspace namespace.
func IsKWTDirectoryWorkspaceSessionName(name string) bool {
	return strings.HasPrefix(name, dirWorkspaceSessionPrefix)
}

// DirWorkspaceSessionName returns a stable, tmux-safe session name for a
// directory workspace: kwt-workspace-dir-{name}-{hash}. The hash is computed
// over the workspace path so renames keep a recognizable suffix and distinct
// directories never collide.
func DirWorkspaceSessionName(name, path string) string {
	raw := fmt.Sprintf("%s%s-%s", dirWorkspaceSessionPrefix, name, template.ShortHash(path))
	return sanitizeTmuxName(raw)
}

// MatchDirWorkspaceSession finds the live directory-workspace session for
// path, matching by prefix and trailing path hash rather than the full name
// so a renamed workspace still finds its running session.
func MatchDirWorkspaceSession(sessions []string, path string) (string, bool) {
	suffix := "-" + template.ShortHash(path)
	for _, session := range sessions {
		if strings.HasPrefix(session, dirWorkspaceSessionPrefix) && strings.HasSuffix(session, suffix) {
			return session, true
		}
	}
	return "", false
}
