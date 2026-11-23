package tmuxrestore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// shells are foreground programs that show a pane is at its prompt, with no
// agent running.
var shells = map[string]bool{
	"bash": true, "zsh": true, "sh": true, "dash": true, "fish": true,
	"ksh": true, "tcsh": true, "csh": true, "nu": true,
}

// discoverLimit bounds how many processes a save reads in one pane.
const discoverLimit = 256

// discover gives a record to each pane whose agent never wrote one, as when
// the agent started before kwt's hooks were installed. The agent itself tells
// which conversation it runs: Claude Code writes a session file named for its
// process, and Codex holds its conversation's rollout file open. A pane that
// already holds a discovered record is read again, since the agent can start
// a new conversation; one kwt cannot read now keeps the record it has, but
// one running two conversations loses it.
func discover(ctx context.Context, snap *Snapshot, table ProcessTable, claudeDir string, now time.Time) {
	if table == nil {
		return
	}
	var panes []*Pane
	for _, window := range snap.Windows {
		for i := range window.Panes {
			pane := &window.Panes[i]
			if (pane.Resume == nil || pane.Resume.Discovered) && pane.PID > 1 &&
				!shells[strings.TrimPrefix(pane.Command, "-")] {
				panes = append(panes, pane)
			}
		}
	}
	if len(panes) == 0 {
		return
	}
	parents, err := table.Parents(ctx)
	if err != nil {
		return
	}
	children := map[int32][]int32{}
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	for _, pids := range children {
		slices.Sort(pids)
	}
	for _, pane := range panes {
		rec, ambiguous := runningConversation(ctx, table, children, pane, claudeDir, now)
		if rec != nil || ambiguous {
			pane.Resume = rec
		}
	}
}

// runningConversation searches the processes under a pane for an agent that
// names its conversation. Two agents naming different conversations, as when
// one is suspended in the shell's background, leave the pane unknown: kwt
// cannot tell which one the person was using; ambiguous reports that case.
// The conversation last ended on purpose in the pane is skipped while the
// process that ended it exits.
func runningConversation(
	ctx context.Context,
	table ProcessTable,
	children map[int32][]int32,
	pane *Pane,
	claudeDir string,
	now time.Time,
) (found *Record, ambiguous bool) {
	queue := []int32{pane.PID}
	for read := 0; len(queue) > 0 && read < discoverLimit; read++ {
		pid := queue[0]
		queue = append(queue[1:], children[pid]...)
		info, err := table.Lookup(ctx, pid)
		if err != nil {
			continue
		}
		rec := Record{Process: &ProcessRef{PID: pid, StartMillis: info.StartMillis}, Argv: info.Argv, RecordedAt: now.Unix()}
		switch {
		case claudeDir != "" && isAgentProcess(info, "claude"):
			rec.Agent = "claude"
			rec.ConversationID = claudeConversation(claudeDir, pid)
		case isAgentProcess(info, "codex"):
			rec.Agent = "codex"
			rec.ConversationID, rec.TranscriptPath = codexConversation(ctx, table, info)
		}
		switch {
		case rec.ConversationID == "" || pane.endedOnPurpose(&rec):
		case found == nil:
			rec.Discovered = true
			found = &rec
		case found.ConversationID != rec.ConversationID:
			return nil, true
		}
	}
	return found, false
}

// claudeConversation reads the conversation Claude Code's session file
// records for its process.
func claudeConversation(claudeDir string, pid int32) string {
	data, err := os.ReadFile(filepath.Join(claudeDir, "sessions", strconv.Itoa(int(pid))+".json"))
	if err != nil {
		return ""
	}
	var session struct {
		PID       int32  `json:"pid"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(data, &session) != nil || session.PID != pid {
		return ""
	}
	return session.SessionID
}

var rolloutName = regexp.MustCompile(`^rollout-.+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// codexConversation reads which conversation a Codex process writes. A
// process writing several, such as Codex's shared app server, names none.
func codexConversation(ctx context.Context, table ProcessTable, info ProcInfo) (string, string) {
	if len(info.Argv) > 0 && codexSubcommand(info.Argv[agentArgsStart(info.Argv, "codex"):]) == "app-server" {
		return "", ""
	}
	files, err := table.OpenFiles(ctx, info.PID)
	if err != nil {
		return "", ""
	}
	var id, path string
	for _, file := range files {
		m := rolloutName.FindStringSubmatch(filepath.Base(file))
		if m == nil {
			continue
		}
		if id != "" && id != m[1] {
			return "", ""
		}
		id, path = m[1], file
	}
	return id, path
}

// ClaudeDir is where Claude Code keeps its settings and conversations.
func ClaudeDir(home string, getenv func(string) string) string {
	if dir := getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}
