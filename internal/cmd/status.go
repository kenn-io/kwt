package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/internal/config"
	"go.kenn.io/kwt/internal/discovery"
	"go.kenn.io/kwt/internal/git"
	"go.kenn.io/kwt/internal/status"
	"go.kenn.io/kwt/internal/ui"
	"go.kenn.io/kwt/internal/worktree"
	"go.kenn.io/kwt/pkg/models"
)

var (
	statusWatch     bool
	statusInterval  int
	statusFilter    string
	statusSort      string
	statusJSON      bool
	statusCSV       bool
	statusVerbose   bool
	statusGlobal    bool
	statusNoFetch   bool
	statusStaleDays int
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of all worktrees",
	Long: `Show status of all worktrees including git status and recent activity.

This command provides a comprehensive view of all worktrees' current state, which is essential
for managing multiple AI coding agents working in parallel across different worktrees.`,
	Example: `  # Table view with basic status
  kwt status
  
  # JSON output for scripting
  kwt status --json
  
  # Watch mode with 5 second interval
  kwt status --watch

  # Filter modified worktrees
  kwt status --filter modified
  
  # Global status from anywhere
  kwt status --global`,
	RunE: withGracefulSignals(runStatus),
}

func init() {
	rootCmd.AddCommand(statusCmd)

	statusCmd.Flags().BoolVarP(&statusWatch, "watch", "w", false, "Auto-refresh mode")
	statusCmd.Flags().IntVarP(&statusInterval, "interval", "i", 5, "Refresh interval in seconds for watch mode")
	statusCmd.Flags().StringVarP(&statusFilter, "filter", "f", "", "Filter by status (changed, up to date, inactive)")
	statusCmd.Flags().StringVarP(&statusSort, "sort", "s", "", "Sort by field (branch, modified, activity)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
	statusCmd.Flags().BoolVar(&statusCSV, "csv", false, "Output as CSV")
	statusCmd.Flags().BoolVarP(&statusVerbose, "verbose", "v", false, "Show additional information")
	statusCmd.Flags().BoolVarP(&statusGlobal, "global", "g", false, "Show all worktrees from base directory")
	statusCmd.Flags().BoolVar(&statusNoFetch, "no-fetch", false, "Skip remote status check (faster)")
	statusCmd.Flags().IntVar(&statusStaleDays, "stale-days", 14, "Days of inactivity before marking as stale")
}

func runStatus(cmd *cobra.Command, args []string) error {
	if statusWatch {
		return runStatusWatch(cmd, time.Duration(statusInterval)*time.Second)
	}

	return runStatusOnce(cmd)
}

func runStatusOnce(cmd *cobra.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	printer := ui.New(&cfg.UI)
	ctx := cmd.Context()

	statuses, err := collectWorktreeStatuses(ctx, cfg, printer)
	if err != nil {
		return fmt.Errorf("failed to collect worktree statuses: %w", err)
	}

	statuses = applyFiltersAndSort(statuses)

	return outputStatuses(statuses, printer, cfg)
}

func runStatusWatch(cmd *cobra.Command, interval time.Duration) error {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	printer := ui.New(&cfg.UI)

	// Setup watch mode (cursor control and cancellation)
	cleanup, ctx := setupWatchMode(cmd.Context())
	defer cleanup()

	// Create refresh function for status updates
	refresh := createRefreshFunction(ctx, cfg, printer)

	// Run the watch loop with periodic refreshes
	return runWatchLoop(ctx, refresh, interval)
}

// setupWatchMode initializes cursor control and cancellation handling
func setupWatchMode(parent context.Context) (func(), context.Context) {
	hideCursor := "\033[?25l"
	showCursor := "\033[?25h"

	fmt.Print(hideCursor)

	ctx, cancel := context.WithCancel(parent)

	cleanup := func() {
		fmt.Print(showCursor)
		cancel()
	}

	return cleanup, ctx
}

// createRefreshFunction creates the refresh function for watch mode
func createRefreshFunction(ctx context.Context, cfg *models.Config, printer *ui.Printer) func() error {
	clearScreen := "\033[H\033[2J"

	return func() error {
		fmt.Print(clearScreen)

		statuses, err := collectWorktreeStatuses(ctx, cfg, printer)
		if err != nil {
			return fmt.Errorf("failed to collect worktree statuses: %w", err)
		}

		statuses = applyFiltersAndSort(statuses)

		// Display summary header
		if err := displayWatchHeader(statuses); err != nil {
			return err
		}

		// Output status details
		if err := outputStatuses(statuses, printer, cfg); err != nil {
			return err
		}

		fmt.Println("\n[Press Ctrl+C to exit]")
		return nil
	}
}

// displayWatchHeader displays the summary header for watch mode
func displayWatchHeader(statuses []*models.WorktreeStatus) error {
	summary := calculateSummary(statuses)
	currentRepo := getCurrentRepository()

	fmt.Printf("Worktrees Status (%s) - Updated: %s\n",
		currentRepo, time.Now().Format("15:04:05"))
	fmt.Printf("Total: %d | Changed: %d | Up to date: %d | Inactive: %d\n\n",
		summary.Total, summary.Modified, summary.Clean, summary.Stale)

	return nil
}

// runWatchLoop runs the main watch loop with periodic refreshes
func runWatchLoop(ctx context.Context, refresh func() error, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial refresh
	if err := refresh(); err != nil {
		return err
	}

	// Main loop
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := refresh(); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		}
	}
}

func collectWorktreeStatuses(ctx context.Context, cfg *models.Config, printer *ui.Printer) ([]*models.WorktreeStatus, error) {
	var worktrees []*models.Worktree

	g, err := git.NewFromCwd()
	if err != nil || statusGlobal {
		globalEntries, err := discovery.DiscoverGlobalWorktrees(
			cfg.Worktree.BaseDir,
			cfg.Projects,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to discover worktrees: %w", err)
		}
		for _, entry := range globalEntries {
			model := entry.Model()
			worktrees = append(worktrees, &model)
		}
	} else {
		wm := worktree.New(g, cfg)
		localWorktrees, err := wm.List()
		if err != nil {
			return nil, fmt.Errorf("failed to list worktrees: %w", err)
		}
		// Local worktrees all belong to the repository containing cwd. Enrich
		// them before collection so registered identity precedence and the
		// canonical local fallback match the global status surface.
		enrichWorktreeIdentity(g, cfg.Projects, localWorktrees)
		for i := range localWorktrees {
			worktrees = append(worktrees, &localWorktrees[i])
		}
	}

	collector := status.NewStatusCollectorWithOptions(status.StatusCollectorOptions{
		FetchRemote:    !statusNoFetch,
		StaleThreshold: time.Duration(statusStaleDays) * 24 * time.Hour,
		BaseDir:        cfg.Worktree.BaseDir,
	})
	return collector.CollectAll(ctx, worktrees)
}

func applyFiltersAndSort(statuses []*models.WorktreeStatus) []*models.WorktreeStatus {
	if statusFilter != "" {
		statuses = filterStatuses(statuses, statusFilter)
	}

	if statusSort != "" {
		sortStatuses(statuses, statusSort)
	}

	return statuses
}

func outputStatuses(statuses []*models.WorktreeStatus, printer *ui.Printer, cfg *models.Config) error {
	switch {
	case statusJSON:
		return outputJSON(statuses)
	case statusCSV:
		return outputCSV(statuses)
	default:
		return outputTable(statuses, printer, statusVerbose)
	}
}

func getCurrentRepository() string {
	g, err := git.NewFromCwd()
	if err != nil {
		return "all repositories"
	}

	remote, err := g.GetRepositoryURL()
	if err != nil {
		return "local"
	}

	return remote
}

type statusSummary struct {
	Total    int
	Modified int
	Clean    int
	Stale    int
}

func calculateSummary(statuses []*models.WorktreeStatus) statusSummary {
	summary := statusSummary{Total: len(statuses)}

	for _, s := range statuses {
		switch s.Status {
		case models.WorktreeStatusModified:
			summary.Modified++
		case models.WorktreeStatusClean:
			summary.Clean++
		case models.WorktreeStatusStale:
			summary.Stale++
		}
	}

	return summary
}

func filterStatuses(statuses []*models.WorktreeStatus, filter string) []*models.WorktreeStatus {
	var filtered []*models.WorktreeStatus

	for _, s := range statuses {
		switch filter {
		case "modified", "changed":
			if s.Status == models.WorktreeStatusModified {
				filtered = append(filtered, s)
			}
		case "clean", "up to date":
			if s.Status == models.WorktreeStatusClean {
				filtered = append(filtered, s)
			}
		case "stale", "inactive":
			if s.Status == models.WorktreeStatusStale {
				filtered = append(filtered, s)
			}
		case "staged":
			if s.Status == models.WorktreeStatusStaged {
				filtered = append(filtered, s)
			}
		case "conflict", "conflicted":
			if s.Status == models.WorktreeStatusConflict {
				filtered = append(filtered, s)
			}
		}
	}

	return filtered
}
