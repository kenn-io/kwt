# Design Notes

These notes preserve the design intent and architecture behind kwt after a
feature ships. They explain why a contract has its current boundary; they are
not the starting point for using that contract.

Start with [Embed and connect kwt](../integrations/embedding.md) when building a
client, or with the workflow guides for day-to-day use. Read these notes when
changing behavior, reviewing a proposed feature, or deciding whether a new
abstraction belongs in kwt.

## Available notes

- [TUI and project registry](tui-projects.md)
- [Multi-machine sync architecture](multi-machine-sync.md)
- [Worktree maintenance](worktree-maintenance.md)
- [Worktree change inspection](worktree-changes.md)
- [Local service daemon](daemon.md)

Feature specs and implementation plans should be folded into these maintained
notes once they ship, so draft checklists do not compete with the current
contract.
