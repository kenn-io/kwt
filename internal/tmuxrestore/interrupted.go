package tmuxrestore

import "go.kenn.io/kwt/internal/agentstate"

// ContinuePrompt is the message a continued conversation resumes with. The
// agent's processes and temporary files did not survive the server's loss,
// so it checks its work before going on.
const ContinuePrompt = "The machine restarted while you were working, which stopped this turn. " +
	"Processes you started and files in temporary directories may be gone. " +
	"Check the state of your work, then continue the task."

// latestTurn finds rec's conversation and reads its latest turn. Only Codex
// writes turns kwt can read; other agents get an empty Turn.
func latestTurn(rec Record, home string, getenv func(string) string) (Presence, Turn) {
	presence, path := FindConversation(rec, home, getenv)
	if rec.Agent != "codex" || presence != PresenceFound {
		return presence, Turn{}
	}
	return presence, CodexTurn(path)
}

// cutShort reports whether the pane's agent was in the middle of a turn when
// its server was lost. Claude Code tells through the activity its hooks last
// saved; Codex through its rollout, since its hooks may not be enabled. An
// agent waiting on the person, or one kwt cannot read, was not cut short.
func cutShort(pane Pane, turn Turn) bool {
	if pane.Resume == nil {
		return false
	}
	switch pane.Resume.Agent {
	case "claude":
		return pane.Activity != nil && pane.Activity.Agent == "claude" &&
			pane.Activity.State == string(agentstate.Working)
	case "codex":
		return turn.Unfinished && !waiting(pane.Activity, "codex")
	default:
		return false
	}
}

// waiting reports whether the agent's hook last recorded it waiting for the
// person's approval or answer.
func waiting(activity *Activity, agent string) bool {
	return activity != nil && activity.Agent == agent &&
		(activity.State == string(agentstate.Approval) || activity.State == string(agentstate.Input))
}
