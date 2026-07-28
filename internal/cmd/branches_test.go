package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kwt/pkg/models"
)

func TestBranchesJSONListsAvailableCandidates(t *testing.T) {
	repoPath := newTUITestRepo(t)
	initCommandTestConfig(t, t.TempDir())
	t.Chdir(repoPath)
	runTUITestGit(t, repoPath, "branch", "feature/ready")

	oldJSON := branchesJSON
	t.Cleanup(func() { branchesJSON = oldJSON })
	branchesJSON = true

	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)

	err := runBranches(cmd, nil)

	require.NoError(t, err)
	var branches []models.Branch
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &branches))
	require.Len(t, branches, 1)
	assert.Equal(t, "feature/ready", branches[0].Name)
	assert.Equal(t, "feature/ready", branches[0].Source)
}
