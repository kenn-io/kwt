---
title: Git worktrees and agent workspaces
description: Manage isolated Git worktrees and tmux workspaces from a terminal dashboard or a scriptable CLI.
hide:
  - toc
---

<section class="kwt-hero">
  <div class="kwt-hero__copy">
    <h1>Git worktrees and agent workspaces, managed from one place.</h1>
    <p class="kwt-hero__lede">
      kwt is a Git worktree manager with a terminal dashboard for people and a
      scriptable CLI for agents and other tools. It creates isolated checkouts,
      opens their tmux workspaces, shows their current state, and cleans them up
      safely across all your projects.
    </p>
    <div class="kwt-hero__actions">
      <a class="md-button md-button--primary" href="get-started/install/">Install kwt</a>
      <a class="md-button" href="get-started/quickstart/">Quickstart</a>
    </div>
    <p class="kwt-hero__proof">
      kwt powers Git worktrees in
      <a href="https://ghosthub.ai"><strong>Ghosthub</strong></a>, which brings
      tmux, Herdr, and Zellij sessions from your Mac and SSH hosts into one
      native terminal.
    </p>
  </div>
  <figure class="kwt-hero__preview">
    <img
      src="assets/og.png"
      alt="The kwt terminal dashboard listing worktrees, branch state, activity, and workspace status"
      width="1200"
      height="630"
    >
  </figure>
</section>

## Work interactively

```sh
go install go.kenn.io/kwt/cmd/kwt@v0.5.0
kwt
```

Run `kwt` inside a repository to register the project and open the dashboard.
From one keyboard-driven view, you can:

- Attach to a worktree's tmux session (`enter`), creating it when needed.
- Create a branch and worktree (`n`), or search local and remote branches for
  one (`b`).
- See per-branch Git state — dirty, ahead, behind — across every project.
- Delete worktrees (`d`), kill live sessions (`K`), and switch the active
  project (`P`).

A plain directory can be a workspace too. Run `kwt` there, and `enter` opens a
tmux session in place. Follow the [quickstart](get-started/quickstart.md) to
create and use your first workspace.

## Automate worktrees

Agents and other tools can manage the same lifecycle with plain commands and
stable JSON:

```sh
kwt add -b feature/new-ui
kwt exec feature/new-ui -- make test
kwt changes --json
kwt remove -b feature/new-ui
```

Use `kwt list --json` to discover worktrees, `kwt get` to resolve an exact path,
and `kwt pr import` to create an inert workspace from a GitHub pull request.
Configurable [layouts](workflows/agent-workspaces.md) define the tmux panes each
workspace starts with. See [Agent workspaces](workflows/agent-workspaces.md) for
a complete automation loop and the [CLI reference](reference/cli.md) for the
machine contracts.

## Guardrails

Checkouts of existing or remote branches are created inert: kwt skips
repository hooks, setup commands, and workspace launch until you review the
files and explicitly open the workspace. Pull-request imports use a protected
session boundary and preserve exact push routing. Multi-machine sync stays off
until you [configure it](multi-machine-sync.md).

<aside class="kwt-ecosystem">
  <div>
    <p class="kwt-eyebrow">Build on kwt</p>
    <h2>The same lifecycle is available to other applications.</h2>
  </div>
  <div>
    <p>
      Public Go services, the local daemon, stable JSON, and explicit tmux
      session endpoints let clients reuse kwt without rebuilding its worktree
      safety rules. <a href="https://ghosthub.ai"><strong>Ghosthub</strong></a>
      is one application that embeds these services for local and SSH-hosted
      workspaces.
    </p>
    <a class="kwt-text-link" href="integrations/embedding/">Embed and connect kwt →</a>
  </div>
</aside>

<div class="kwt-link-grid">
  <a href="reference/cli/"><strong>CLI reference</strong><span>Commands, flags, JSON, and attachment contracts</span></a>
  <a href="reference/configuration/"><strong>Configuration</strong><span>Paths, layouts, agents, setup, and trust</span></a>
  <a href="reference/pull-requests/"><strong>Pull-request automation</strong><span>Discover, import, and attach safely</span></a>
  <a href="releases/"><strong>Releases</strong><span>Versions, artifacts, and upgrade guidance</span></a>
</div>
