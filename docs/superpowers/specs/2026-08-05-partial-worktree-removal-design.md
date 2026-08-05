# Partial Worktree Removal Design

## Problem

`git worktree remove` can deregister a worktree and then fail while deleting its directory. A background process may keep recreating files during deletion, for example an Astro development server recreating `website/.astro`.

Generation-conditional removal must not recursively delete the remaining path after Git returns because another checkout could have appeared there. The current implementation preserves that safety property, but reports the outcome as a generic hard failure. In the TUI this also prevents the normal bookkeeping and refresh, leaving a stale row and exposing an internal explanation about generation-conditional cleanup.

## Intended Behavior

When kwt confirms that a previously registered worktree is no longer registered after Git returns an error, it treats the operation as a partial success:

- The residual directory is preserved.
- Registry and fleet state are updated as they are after a successful removal.
- The TUI refreshes so the removed worktree disappears.
- The user sees an actionable warning that the worktree was removed from Git, files remain at the path, and processes using the directory should be stopped before it is deleted manually.

If Git still reports the worktree as registered, removal remains a hard failure and no success bookkeeping runs.

## Design

The Git layer will return a typed partial-removal error when its before-and-after registration check proves that Git deregistered the worktree but directory deletion failed. The type will preserve the underlying Git error for programmatic inspection while presenting a concise user-facing message containing the residual path. A helper will let higher layers distinguish this outcome from an ordinary error without matching error text.

CLI and TUI removal consumers will recognize the typed outcome as a completed Git removal. They will conditionally unregister the previously observed registry record, publish updated fleet state, and perform any remaining successful-removal cleanup. They will still return the warning so the residual directory is not hidden.

The TUI action result already represents both errors and refresh requests, but its update path currently exits before honoring refresh when an error is present. It will honor refresh for this warning result while continuing to display the warning. Ordinary errors will continue not to refresh unless explicitly requested.

No process discovery, process termination, retry loop, or recursive cleanup is added. Those behaviors would be platform-specific and could affect processes or files kwt does not own.

## Testing

Tests will be added failing-first at each observable boundary:

- The Git removal regression will simulate Git deregistration followed by a directory-deletion failure, assert the typed partial-success classification and actionable message, and verify the residual directory remains.
- TUI backend coverage will assert that a partial removal still performs registry cleanup and fleet publication while returning the warning.
- TUI model coverage will assert that a removal warning remains visible while a refresh is started, causing stale rows to be replaced by current inventory.
- Existing hard-failure and successful-removal tests will continue to define their current behavior.

