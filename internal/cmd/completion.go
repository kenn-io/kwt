package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/registry"
	"go.kenn.io/kwt/internal/worktree"
)

// getWorktreeCompletions returns worktree names for shell completion
func getWorktreeCompletions(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	g, err := git.NewFromCwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	wm := worktree.New(g, nil)
	worktrees, err := wm.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, wt := range worktrees {
		if strings.HasPrefix(wt.Branch, toComplete) || strings.HasPrefix(wt.Path, toComplete) {
			desc := fmt.Sprintf("Branch: %s", wt.Branch)
			if wt.Path != "" {
				desc += fmt.Sprintf(" | Path: %s", wt.Path)
			}
			completions = append(completions, fmt.Sprintf("%s\t%s", wt.Branch, desc))
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// getBranchCompletions returns branch names for shell completion
func getBranchCompletions(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	g, err := git.NewFromCwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	branches, err := g.ListBranches(false)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, branch := range branches {
		if strings.HasPrefix(branch.Name, toComplete) {
			completions = append(
				completions,
				fmt.Sprintf("%s\tLocal branch", branch.Name),
			)
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// getRemoteBranchCompletions returns remote sources accepted by --from.
func getRemoteBranchCompletions(
	_ *cobra.Command,
	args []string,
	toComplete string,
) ([]string, cobra.ShellCompDirective) {
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	g, err := git.NewFromCwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	branches, err := g.ListAvailableBranches()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, branch := range branches {
		if !branch.IsRemote {
			continue
		}
		source := branch.Source
		shortSource := strings.TrimPrefix(source, "refs/remotes/")
		if strings.HasPrefix(source, toComplete) ||
			strings.HasPrefix(shortSource, toComplete) {
			completions = append(
				completions,
				fmt.Sprintf("%s\tRemote branch", source),
			)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

// getGlobalWorktreeCompletions returns global worktree names (repo:branch format)
func getGlobalWorktreeCompletions(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	reg, err := registry.New()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	entries := reg.List()

	var completions []string
	for _, entry := range entries {
		fullName := fmt.Sprintf("%s:%s", entry.Repository, entry.Branch)
		if strings.HasPrefix(fullName, toComplete) || strings.HasPrefix(entry.Repository, toComplete) || strings.HasPrefix(entry.Branch, toComplete) {
			completions = append(completions, fmt.Sprintf("%s\tPath: %s", fullName, entry.Path))
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// getConfigKeyCompletions returns config key names for shell completion
func getConfigKeyCompletions(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	keys := []struct {
		name string
		desc string
	}{
		{"worktree.basedir", "Base directory for worktrees"},
		{"worktree.auto_mkdir", "Automatically create directories"},
		{"finder.preview", "Enable preview window"},
		{"finder.preview_size", "Preview window size"},
		{"finder.keybind_select", "Key binding for selection"},
		{"finder.keybind_cancel", "Key binding for cancellation"},
		{"naming.template", "Directory name template"},
		{"ui.color", "Enable colored output"},
		{"ui.icons", "Enable icon display"},
		{"ui.tilde_home", "Display home directory as ~"},
		{"cd.launch_shell", "Launch new shell on cd (default: true)"},
		{"agents.codex", "Codex agent command used by layouts"},
		{"agents.claude", "Claude agent command used by layouts"},
		{"agents.roborev", "Roborev command used by layouts"},
		{"layouts.default", "Default workspace layout preset; unset or 'none' means a blank session"},
		{"layouts.auto_launch_on_add", "Launch a tmux workspace on 'kwt add' (default: true)"},
	}

	var completions []string
	for _, key := range keys {
		if strings.HasPrefix(key.name, toComplete) {
			completions = append(completions, fmt.Sprintf("%s\t%s", key.name, key.desc))
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}

// getRemoveCompletions returns only removable (non-main) worktree names for removal
func getRemoveCompletions(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	g, err := git.NewFromCwd()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	wm := worktree.New(g, nil)
	worktrees, err := wm.List()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	// Filter out main worktrees - same logic as fuzzy finder
	var completions []string
	for _, wt := range worktrees {
		if !wt.IsMain && (strings.HasPrefix(wt.Branch, toComplete) || strings.HasPrefix(wt.Path, toComplete)) {
			desc := fmt.Sprintf("Branch: %s", wt.Branch)
			if wt.Path != "" {
				desc += fmt.Sprintf(" | Path: %s", wt.Path)
			}
			completions = append(completions, fmt.Sprintf("%s\t%s", wt.Branch, desc))
		}
	}

	return completions, cobra.ShellCompDirectiveNoFileComp
}
