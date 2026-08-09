package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/service"
	publicworktree "go.kenn.io/kwt/worktree"
)

func TestWriteConfigNotesPreservesNoninteractiveWarning(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []publicworktree.Note{{
		Code: "untrusted_config_skipped", Path: "/repo/.kwt.toml",
	}}, false, false)
	assert.Equal(t, "kwt: skipping untrusted local config /repo/.kwt.toml (non-interactive session)\n", stderr.String())
}

func TestWriteConfigNotesWarnsWhenUnsafeLocalConfigIsSkipped(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []publicworktree.Note{{
		Code: "unsafe_config_skipped", Path: "/repo/.kwt.toml",
	}}, false, false)
	assert.Equal(t, "kwt: skipping unsafe local config /repo/.kwt.toml\n", stderr.String())
}

func TestWriteConfigNotesWarnsWhenTrustStoreIsUnavailable(t *testing.T) {
	var stderr bytes.Buffer
	writeConfigNotes(&stderr, []publicworktree.Note{{
		Code: "trust_store_unavailable", Path: "/home/trusted_configs.json",
	}}, false, false)
	assert.Equal(t, "kwt: failed to load trust store /home/trusted_configs.json (continuing empty)\n", stderr.String())
}

func TestTrustRequirementDecodesHTTPDetailTypes(t *testing.T) {
	err := service.NewError(service.InteractionRequired, "trust", false, map[string]any{
		"kind": "repository_config_trust", "path": "/repo/.kwt.toml",
		"digest": "abc", "size": float64(42), "preview": "[naming]", "truncated": true,
	}, nil)
	requirement, decodeErr := trustRequirement(err)
	require.NoError(t, decodeErr)
	assert.Equal(t, 42, requirement.Size)
	assert.True(t, requirement.Truncated)
}
