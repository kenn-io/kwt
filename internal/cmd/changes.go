package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/service"
)

var (
	changesJSON               bool
	changesExpectedRepository string
	changesExpectedGeneration string
	queryChangesInventory     = queryInspectionInventoryForCLI
	newChangesInspector       = func(cmd *cobra.Command) kwt.Inspector {
		return kwt.NewInspectionService(kwt.InspectionServiceOptions{
			Inventory: changesCLIInventory{stderr: cmd.ErrOrStderr()},
		})
	}
)

var changesCmd = &cobra.Command{
	Use:   "changes [path]",
	Short: "Inspect changed files in one worktree",
	Long: `Inspect staged and working-tree file states for one exact worktree.

The optional path defaults to the current directory. This bounded foreground
inspection does not fetch, watch, generate diffs, or require tmux. Use the
expected identity flags to reject a worktree registration that changed after a
prior observation.`,
	Example: `  kwt changes
  kwt changes [path] --json
  kwt changes /path/to/worktree \
    --expected-repository github.com/acme/widget \
    --expected-generation 0123456789abcdef0123456789abcdef --json`,
	Args:              changesMaximumOneArg,
	PersistentPreRunE: changesPreRun,
	RunE:              withGracefulSignals(runChanges),
}

func changesPreRun(cmd *cobra.Command, args []string) error {
	if err := globalOnlyPreRun(cmd, args); err != nil {
		return writeChangesFailure(cmd, service.NewError(
			service.InspectionFailed,
			"failed to initialize configuration",
			false,
			nil,
			err,
		))
	}
	return nil
}

func changesMaximumOneArg(cmd *cobra.Command, args []string) error {
	if len(args) <= 1 {
		return nil
	}
	return writeCommandFailure(
		cmd,
		service.Descriptor{
			Code: service.InvalidRequest, Message: "expected at most one worktree path",
		},
		2,
		changesJSON,
		"changes",
	)
}

type changesCLIInventory struct {
	stderr io.Writer
}

func (i changesCLIInventory) Query(
	ctx context.Context,
	request kwt.Request,
) (kwt.Result, error) {
	return queryChangesInventory(ctx, request, false, i.stderr)
}

func (changesCLIInventory) ApproveConfig(
	context.Context,
	kwt.ConfigApproval,
) error {
	return service.NewError(
		service.Unsupported,
		"configuration approval is unavailable during worktree inspection",
		false,
		nil,
		nil,
	)
}

func init() {
	rootCmd.AddCommand(changesCmd)
	changesCmd.Flags().BoolVar(&changesJSON, "json", false, "Output a machine-readable inspection result")
	changesCmd.Flags().StringVar(
		&changesExpectedRepository,
		"expected-repository",
		"",
		"Require the exact credential-free repository identity",
	)
	changesCmd.Flags().StringVar(
		&changesExpectedGeneration,
		"expected-generation",
		"",
		"Require the exact observed worktree generation",
	)
	changesCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return writeCommandFailure(
			cmd,
			service.Descriptor{
				Code: service.InvalidRequest, Message: err.Error(),
			},
			2,
			changesJSONRequested(),
			"changes",
		)
	})
}

func changesJSONRequested() bool {
	if changesJSON {
		return true
	}
	// pflag stops at the first parse error, so a later --json never reaches
	// the bound variable. Preserve the caller's output choice from raw argv.
	requested := false
	for _, argument := range os.Args[1:] {
		if argument == "--" {
			break
		}
		if argument == "--json" {
			requested = true
			continue
		}
		name, value, ok := strings.Cut(argument, "=")
		if !ok || name != "--json" {
			continue
		}
		enabled, err := strconv.ParseBool(value)
		if err == nil {
			requested = enabled
		}
	}
	return requested
}

func runChanges(cmd *cobra.Command, args []string) error {
	path, err := changesPath(args)
	if err != nil {
		return writeChangesFailure(cmd, err)
	}
	result, err := newChangesInspector(cmd).Inspect(
		cmd.Context(),
		kwt.InspectionRequest{
			Path:               path,
			ExpectedRepository: changesExpectedRepository,
			ExpectedGeneration: changesExpectedGeneration,
		},
	)
	if err != nil {
		return writeChangesFailure(cmd, err)
	}
	if !changesJSON {
		return writeChangesHuman(cmd.OutOrStdout(), result)
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func writeChangesFailure(cmd *cobra.Command, err error) error {
	var typed *service.Error
	if !errors.As(err, &typed) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		typed = service.AsError(err)
	}
	exitCode := 1
	if typed.Code == service.InvalidRequest || typed.Code == service.NotFound {
		exitCode = 2
	}
	failure := writeCommandFailure(
		cmd,
		typed.Descriptor,
		exitCode,
		changesJSON,
		"changes",
	)
	failure.cause = typed.Err
	return failure
}

func writeChangesHuman(output io.Writer, result kwt.InspectionResult) error {
	var body strings.Builder
	_, _ = fmt.Fprintf(
		&body,
		"Repository: %s\nWorktree: %s\nGeneration: %s\nObserved at: %s\n",
		strconv.Quote(result.Worktree.Repository),
		strconv.Quote(result.Worktree.Path),
		result.Worktree.Generation,
		result.ObservedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	)
	if len(result.Changes.Files) == 0 {
		body.WriteString("No changed files\n")
		_, err := io.WriteString(output, body.String())
		return err
	}
	body.WriteString("Changes:\n")
	for _, file := range result.Changes.Files {
		body.WriteString("  ")
		if file.OriginalPath != "" {
			body.WriteString(strconv.Quote(file.OriginalPath))
			body.WriteString(" -> ")
		}
		body.WriteString(strconv.Quote(file.Path))
		body.WriteByte('\n')
		_, _ = fmt.Fprintf(
			&body,
			"    staged: %s\n    working tree: %s\n",
			changesStagedStateLabel(file.Index),
			changesStateLabel(file.Worktree),
		)
	}
	_, err := io.WriteString(output, body.String())
	return err
}

func changesStagedStateLabel(state kwt.FileState) string {
	if state == kwt.FileStateConflicted {
		return "-"
	}
	return changesStateLabel(state)
}

func changesStateLabel(state kwt.FileState) string {
	if state == "" {
		return "-"
	}
	return string(state)
}

func changesPath(args []string) (string, error) {
	path := ""
	if len(args) == 0 {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	} else {
		path = args[0]
	}
	return filepath.Abs(path)
}
