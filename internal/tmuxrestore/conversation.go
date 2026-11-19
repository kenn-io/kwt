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

// searchByName walks dirs for a file whose name matches. Any directory that
// cannot be read leaves the answer unknown.
func searchByName(id string, match func(string) bool, dirs ...string) Presence {
	if id == "" {
		return PresenceUnknown
	}
	errFound := errors.New("found")
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && match(entry.Name()) {
				return errFound
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
	return PresenceAbsent
}
