package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddBranchCompletionsSeparateLocalAndRemoteSources(t *testing.T) {
	repoPath := newTUITestRepo(t)
	runTUITestGit(t, repoPath, "branch", "feature/local-completion")
	runTUITestGit(t, repoPath, "remote", "add", "origin", repoPath)
	runTUITestGit(
		t,
		repoPath,
		"update-ref",
		"refs/remotes/origin/feature/remote-completion",
		"HEAD",
	)
	runTUITestGit(
		t,
		repoPath,
		"branch",
		"origin/feature/remote-completion",
	)
	t.Chdir(repoPath)

	positional, _ := getBranchCompletions(nil, nil, "feature/")
	remote, _ := getRemoteBranchCompletions(nil, nil, "origin/")

	assert.Contains(t, completionValues(positional), "feature/local-completion")
	assert.NotContains(t, completionValues(positional), "feature/remote-completion")
	assert.Equal(
		t,
		[]string{"refs/remotes/origin/feature/remote-completion"},
		completionValues(remote),
	)

	flagCompletion, ok := addCmd.GetFlagCompletionFunc("from")
	require.True(t, ok)
	require.NotNil(t, flagCompletion)
}

func completionValues(completions []string) []string {
	values := make([]string, 0, len(completions))
	for _, completion := range completions {
		value, _, _ := strings.Cut(completion, "\t")
		values = append(values, value)
	}
	return values
}
