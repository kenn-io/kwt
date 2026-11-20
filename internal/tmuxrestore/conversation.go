package tmuxrestore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Presence is what kwt could learn about a saved conversation.
type Presence int

const (
	// PresenceUnknown means kwt could not tell; restore still resumes.
	PresenceUnknown Presence = iota
	PresenceFound
	// PresenceAbsent means kwt searched everywhere the agent keeps
	// conversations and found none with the ID.
	PresenceAbsent
)

// FindConversation looks for rec's conversation by ID. The recorded
// transcript path is only the first place to look: agents move and compress
// transcripts, so a missing path alone proves nothing.
func FindConversation(rec Record, home string, getenv func(string) string) Presence {
	if rec.TranscriptPath != "" {
		if _, err := os.Stat(rec.TranscriptPath); err == nil {
			return PresenceFound
		}
	}
	switch rec.Agent {
	case "claude":
		root := getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		return searchByName(rec.ConversationID, func(name string) bool {
			return name == rec.ConversationID+".jsonl"
		}, filepath.Join(root, "projects"))
	case "codex":
		root := getenv("CODEX_HOME")
		if root == "" {
			root = filepath.Join(home, ".codex")
		}
		return searchByName(rec.ConversationID, func(name string) bool {
			return strings.Contains(name, rec.ConversationID)
		}, filepath.Join(root, "sessions"), filepath.Join(root, "archived_sessions"))
	default:
		return PresenceUnknown
	}
}

// searchByName walks dirs for a file whose name matches. A linked directory
// is searched; a link inside it is not followed, so it leaves the answer
// unknown, as does any directory that cannot be read.
func searchByName(id string, match func(string) bool, dirs ...string) Presence {
	if id == "" {
		return PresenceUnknown
	}
	errFound := errors.New("found")
	unfollowed := false
	for _, dir := range dirs {
		root, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return PresenceUnknown
		}
		err = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case !entry.IsDir() && match(entry.Name()):
				return errFound
			case entry.Type()&fs.ModeSymlink != 0:
				unfollowed = true
			}
			return nil
		})
		if errors.Is(err, errFound) {
			return PresenceFound
		}
		if err != nil {
			return PresenceUnknown
		}
	}
	if unfollowed {
		return PresenceUnknown
	}
	return PresenceAbsent
}
