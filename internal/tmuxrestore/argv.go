package tmuxrestore

import (
	"path/filepath"
	"slices"
	"strings"
)

// LaunchArgs is the command restore runs to resume a conversation.
type LaunchArgs struct {
	Argv []string
	// Dropped names launch flags kwt did not recognize. It never holds
	// their values, which can be secrets.
	Dropped []string
	// Fallback reports that the recorded arguments were missing, so the
	// configured agent command was used.
	Fallback bool
}

type arity int

const (
	argNone arity = iota
	argOne
	argOptional
	argVariadic
)

type flagSpec struct {
	arity arity
	keep  bool
}

// claudeFlags lists the Claude Code launch flags kwt understands. Settings
// are replayed; one-time actions and conversation selectors are not.
var claudeFlags = map[string]flagSpec{
	"--model":                              {argOne, true},
	"--fallback-model":                     {argOne, true},
	"--permission-mode":                    {argOne, true},
	"--dangerously-skip-permissions":       {argNone, true},
	"--allow-dangerously-skip-permissions": {argNone, true},
	"--add-dir":                            {argVariadic, true},
	"--settings":                           {argOne, true},
	"--setting-sources":                    {argOne, true},
	"--mcp-config":                         {argVariadic, true},
	"--strict-mcp-config":                  {argNone, true},
	"--allowedTools":                       {argVariadic, true},
	"--allowed-tools":                      {argVariadic, true},
	"--disallowedTools":                    {argVariadic, true},
	"--disallowed-tools":                   {argVariadic, true},
	"--append-system-prompt":               {argOne, true},
	"--system-prompt":                      {argOne, true},
	"--agent":                              {argOne, true},
	"--agents":                             {argOne, true},
	"--plugin-dir":                         {argOne, true},
	"--ide":                                {argNone, true},
	"--verbose":                            {argNone, true},
	"--debug":                              {argNone, true},
	"-w":                                   {argOptional, false},
	"--worktree":                           {argOptional, false},
	"--tmux":                               {argNone, false},
	"-p":                                   {argNone, false},
	"--print":                              {argNone, false},
	"-r":                                   {argOptional, false},
	"--resume":                             {argOptional, false},
	"-c":                                   {argNone, false},
	"--continue":                           {argNone, false},
	"--session-id":                         {argOne, false},
	"--fork-session":                       {argNone, false},
	"--from-pr":                            {argOne, false},
	"--output-format":                      {argOne, false},
	"--input-format":                       {argOne, false},
}

// codexFlags lists the Codex launch flags kwt understands.
var codexFlags = map[string]flagSpec{
	"-m":                 {argOne, true},
	"--model":            {argOne, true},
	"-s":                 {argOne, true},
	"--sandbox":          {argOne, true},
	"-a":                 {argOne, true},
	"--ask-for-approval": {argOne, true},
	"-p":                 {argOne, true},
	"--profile":          {argOne, true},
	"-c":                 {argOne, true},
	"--config":           {argOne, true},
	"--add-dir":          {argOne, true},
	"-C":                 {argOne, true},
	"--cd":               {argOne, true},
	"--enable":           {argOne, true},
	"--disable":          {argOne, true},
	"--full-auto":        {argNone, true},
	"--dangerously-bypass-approvals-and-sandbox": {argNone, true},
	"--yolo":           {argNone, true},
	"--search":         {argNone, true},
	"--oss":            {argNone, true},
	"--local-provider": {argOne, true},
	"--no-alt-screen":  {argNone, true},
	"-i":               {argOne, false},
	"--image":          {argOne, false},
	"--last":           {argNone, false},
}

// flagGroup is one flag with the values that belong to it.
type flagGroup struct {
	name   string
	tokens []string
}

// BuildLaunch returns the command that resumes rec's conversation. It
// replays only recognized settings from the recorded arguments, then adds
// the agent's resume arguments. configured is kwt's configured command for
// the agent, used when no arguments were recorded.
func BuildLaunch(rec Record, configured string) LaunchArgs {
	argv := rec.Argv
	var result LaunchArgs
	if len(argv) == 0 {
		argv = strings.Fields(configured)
		if len(argv) == 0 {
			argv = []string{rec.Agent}
		}
		result.Fallback = true
	}
	flags := codexFlags
	if rec.Agent == "claude" {
		flags = claudeFlags
	}
	// A runtime keeps its own options and the agent's script, found the way
	// process detection finds it.
	prefix := 1
	if base := filepath.Base(argv[0]); (base == "node" || base == "bun") && len(argv) > 1 {
		prefix = 2
		if pkg, ok := agentPackages[rec.Agent]; ok {
			if i := slices.IndexFunc(argv, func(arg string) bool { return strings.Contains(arg, pkg) }); i > 0 {
				prefix = i + 1
			}
		}
	}
	groups, dropped := parseFlags(argv[prefix:], flags)
	result.Dropped = dropped
	head := slices.Clone(argv[:prefix])
	if prefix == 1 && agentBinary(filepath.Base(head[0]), rec.Agent) {
		// An installed agent records a versioned path, gone after an update,
		// or a platform binary that is not on PATH. Start the program kwt is
		// configured to run for the agent, or let the pane's PATH find the
		// current install by name, as when the user started it. Only an
		// absolute configured path is usable: no shell expands ~ or $HOME.
		head[0] = rec.Agent
		if fields := strings.Fields(configured); len(fields) > 0 && filepath.IsAbs(fields[0]) &&
			agentBinary(filepath.Base(fields[0]), rec.Agent) {
			head[0] = fields[0]
		}
	}

	if rec.Agent == "claude" && rec.PermissionMode != "" {
		bypass := slices.ContainsFunc(groups, func(g flagGroup) bool {
			return g.name == "--dangerously-skip-permissions" || g.name == "--allow-dangerously-skip-permissions"
		})
		groups = slices.DeleteFunc(groups, func(g flagGroup) bool {
			return g.name == "--permission-mode" || g.name == "--dangerously-skip-permissions"
		})
		groups = append(groups, flagGroup{
			name: "--permission-mode", tokens: []string{"--permission-mode", rec.PermissionMode},
		})
		if bypass && !slices.ContainsFunc(groups, func(g flagGroup) bool {
			return g.name == "--allow-dangerously-skip-permissions"
		}) {
			groups = append(groups, flagGroup{
				name:   "--allow-dangerously-skip-permissions",
				tokens: []string{"--allow-dangerously-skip-permissions"},
			})
		}
	}

	result.Argv = head
	for _, group := range groups {
		result.Argv = append(result.Argv, group.tokens...)
	}
	if rec.Agent == "claude" {
		result.Argv = append(result.Argv, "--resume", rec.ConversationID)
	} else {
		result.Argv = append(result.Argv, "resume", rec.ConversationID)
	}
	return result
}

// parseFlags keeps recognized settings and drops everything else. A
// positional argument is a prompt or subcommand and is never replayed.
func parseFlags(args []string, flags map[string]flagSpec) ([]flagGroup, []string) {
	var groups []flagGroup
	var dropped []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			continue
		}
		name, _, inline := strings.Cut(arg, "=")
		spec, known := flags[name]
		if !known {
			if !strings.HasPrefix(name, "--") && len(name) > 2 {
				name = name[:2]
			}
			dropped = append(dropped, name)
			continue
		}
		tokens := []string{arg}
		if !inline {
			switch spec.arity {
			case argOne:
				if i+1 < len(args) {
					i++
					tokens = append(tokens, args[i])
				}
			case argOptional:
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					tokens = append(tokens, args[i])
				}
			case argVariadic:
				for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					tokens = append(tokens, args[i])
				}
			}
		}
		if spec.keep {
			groups = append(groups, flagGroup{name: name, tokens: tokens})
		}
	}
	return groups, dropped
}
