package tmuxrestore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// ErrProcessNotFound reports that no process has the requested PID.
var ErrProcessNotFound = errors.New("process not found")

// ProcInfo is the part of a process that agent identification reads.
type ProcInfo struct {
	PID         int32
	PPID        int32
	Name        string
	Argv        []string
	StartMillis int64
}

// ProcessTable looks up processes by PID.
type ProcessTable interface {
	Lookup(ctx context.Context, pid int32) (ProcInfo, error)
}

// SystemProcesses reads the host's process table.
func SystemProcesses() ProcessTable { return systemProcesses{} }

type systemProcesses struct{}

func (systemProcesses) Lookup(ctx context.Context, pid int32) (ProcInfo, error) {
	p, err := process.NewProcessWithContext(ctx, pid)
	if errors.Is(err, process.ErrorProcessNotRunning) {
		return ProcInfo{}, ErrProcessNotFound
	}
	if err != nil {
		return ProcInfo{}, fmt.Errorf("look up process %d: %w", pid, err)
	}
	start, err := p.CreateTimeWithContext(ctx)
	if err != nil {
		return ProcInfo{}, fmt.Errorf("read process %d start time: %w", pid, err)
	}
	ppid, err := p.PpidWithContext(ctx)
	if err != nil {
		return ProcInfo{}, fmt.Errorf("read process %d parent: %w", pid, err)
	}
	name, _ := p.NameWithContext(ctx)
	// Arguments can be unreadable even for the user's own processes; the
	// record then falls back to the configured agent command.
	argv, _ := p.CmdlineSliceWithContext(ctx)
	return ProcInfo{PID: pid, PPID: ppid, Name: name, Argv: argv, StartMillis: start}, nil
}

// IdentifyAgent walks up from start to find the agent process. The agent must
// be panePID or one of its descendants; otherwise the hook is not running in
// that pane and nothing is returned.
func IdentifyAgent(
	ctx context.Context,
	table ProcessTable,
	start, panePID int32,
	agent string,
) (*ProcessRef, []string) {
	var found *ProcInfo
	for pid, steps := start, 0; pid > 1 && steps < 64; steps++ {
		info, err := table.Lookup(ctx, pid)
		if err != nil {
			return nil, nil
		}
		if found == nil && isAgentProcess(info, agent) {
			found = &info
		}
		if pid == panePID {
			if found == nil {
				return nil, nil
			}
			return &ProcessRef{PID: found.PID, StartMillis: found.StartMillis}, found.Argv
		}
		pid = info.PPID
	}
	return nil, nil
}

// HookInPane reports whether a hook process, start, runs for the agent in the
// pane whose first process is panePID. Codex's shared background server runs
// every client's hooks in its own process, with the tmux environment of
// whichever pane started it, so a hook below a Codex app server does not
// belong to that pane. A process kwt cannot read leaves the question open,
// and the hook counts as in the pane.
func HookInPane(ctx context.Context, table ProcessTable, start, panePID int32) bool {
	for pid, steps := start, 0; steps < 64; steps++ {
		if pid <= 1 {
			return false
		}
		info, err := table.Lookup(ctx, pid)
		if err != nil {
			return true
		}
		if isAgentProcess(info, "codex") && len(info.Argv) > 0 &&
			codexSubcommand(info.Argv[agentArgsStart(info.Argv, "codex"):]) == "app-server" {
			return false
		}
		if pid == panePID {
			return true
		}
		pid = info.PPID
	}
	return true
}

var agentPackages = map[string]string{
	"claude": "@anthropic-ai/claude-code",
	"codex":  "@openai/codex",
}

// agentBinary reports whether a program name is the agent's own binary,
// such as "claude", the "claude.exe" that Claude Code's npm package runs on
// every platform, or Codex's "codex-<platform>".
func agentBinary(name, agent string) bool {
	name = strings.TrimSuffix(name, ".exe")
	return name == agent || strings.HasPrefix(name, agent+"-")
}

func isAgentProcess(info ProcInfo, agent string) bool {
	names := []string{info.Name}
	if len(info.Argv) > 0 {
		names = append(names, filepath.Base(info.Argv[0]))
	}
	if slices.ContainsFunc(names, func(name string) bool { return agentBinary(name, agent) }) {
		return true
	}
	pkg, ok := agentPackages[agent]
	if !ok || (info.Name != "node" && info.Name != "bun") {
		return false
	}
	for _, arg := range info.Argv {
		if strings.Contains(arg, pkg) {
			return true
		}
	}
	return false
}

// Liveness is what a save can tell about a recorded agent process.
type Liveness int

const (
	LivenessUnknown Liveness = iota
	LivenessAlive
	LivenessGone
)

// CheckLiveness reports whether ref still names a running process.
func CheckLiveness(ctx context.Context, table ProcessTable, ref *ProcessRef) Liveness {
	if ref == nil {
		return LivenessUnknown
	}
	info, err := table.Lookup(ctx, ref.PID)
	switch {
	case errors.Is(err, ErrProcessNotFound):
		return LivenessGone
	case err != nil:
		return LivenessUnknown
	case info.StartMillis != ref.StartMillis:
		return LivenessGone
	default:
		return LivenessAlive
	}
}
