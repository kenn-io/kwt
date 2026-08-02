---
title: Git worktrees for terminal workflows
description: One terminal dashboard for every project, worktree, and agent workspace.
hide:
  - toc
---

<section class="kwt-hero">
  <div class="kwt-hero__copy">
    <p class="kwt-eyebrow">Git worktrees, made operational</p>
    <h1>Keep every branch, project, and coding agent within reach.</h1>
    <p class="kwt-hero__lede">
      kwt turns Git worktrees into durable terminal workspaces. Create an
      isolated checkout, open its tmux session, and see the state of all your
      active work from one keyboard-driven dashboard.
    </p>
    <div class="kwt-hero__actions">
      <a class="md-button md-button--primary" href="get-started/install/">Install kwt</a>
      <a class="md-button" href="get-started/quickstart/">Take the quickstart</a>
    </div>
  </div>
  <div class="kwt-terminal" aria-label="Example kwt terminal dashboard">
    <div class="kwt-terminal__bar">
      <span></span><span></span><span></span>
      <strong>kwt</strong>
    </div>
    <div class="kwt-terminal__body">
      <p><b>REPO</b><b>BRANCH</b><b>CHANGES</b><b>WORKSPACE</b></p>
      <p class="is-active"><span>kwt</span><span>main [primary]</span><span>clean</span><span>live</span></p>
      <p><span>kwt</span><span>docs/refresh</span><span class="is-warm">~3</span><span>live</span></p>
      <p><span>api</span><span>feat/oauth</span><span>clean</span><span>quiet</span></p>
      <p><span>web</span><span>fix/nav</span><span class="is-cool">remote</span><span>remote</span></p>
      <div class="kwt-terminal__detail">
        <span>docs/refresh</span>
        <span>↑2 · agent workspace active</span>
      </div>
      <div class="kwt-terminal__keys">enter open&nbsp;&nbsp; n new&nbsp;&nbsp; b branch&nbsp;&nbsp; / search&nbsp;&nbsp; ? help</div>
    </div>
  </div>
</section>

<div class="kwt-signal-row" aria-label="kwt highlights">
  <span><strong>Cross-project</strong> by default</span>
  <span><strong>Local-first</strong> state</span>
  <span><strong>tmux-backed</strong> sessions</span>
  <span><strong>Scriptable</strong> CLI + JSON</span>
</div>

## One command to get oriented

```sh
go install go.kenn.io/kwt/cmd/kwt@latest
kwt
```

Run `kwt` inside a repository and it registers the project, discovers its
worktrees, and opens the dashboard. Run it from a regular directory and that
directory can become a tmux-backed workspace too.

<div class="kwt-card-grid">
  <article>
    <span class="kwt-card__number">01</span>
    <h3>Separate the work</h3>
    <p>Create a worktree for each feature, fix, review, or agent. Branches stop competing for one checkout.</p>
    <a href="get-started/quickstart/#create-a-worktree">Create a worktree →</a>
  </article>
  <article>
    <span class="kwt-card__number">02</span>
    <h3>Keep context alive</h3>
    <p>Every worktree can have a predictable tmux session, from one plain shell to an explicit agent layout.</p>
    <a href="workflows/agent-workspaces/">Build an agent workflow →</a>
  </article>
  <article>
    <span class="kwt-card__number">03</span>
    <h3>See the whole fleet</h3>
    <p>Scan project, Git, and workspace state locally—or opt in to an advisory view across your machines.</p>
    <a href="multi-machine-sync/">Understand multi-machine sync →</a>
  </article>
</div>

<aside class="kwt-ecosystem">
  <div>
    <p class="kwt-eyebrow">Bundled by Ghosthub</p>
    <h2>The same worktree engine, inside a native terminal.</h2>
  </div>
  <div>
    <p>
      <a href="https://ghosthub.ai"><strong>Ghosthub</strong></a> bundles kwt
      to manage project registration, linked worktrees, pull-request imports,
      and canonical tmux sessions across local and SSH-hosted workspaces. Use
      Ghosthub for a native macOS experience or kwt directly from any supported
      terminal.
    </p>
    <a class="kwt-text-link" href="https://ghosthub.ai">Explore Ghosthub →</a>
  </div>
</aside>

## A direct daily loop

<div class="kwt-workflow">
  <div>
    <span>1</span>
    <div>
      <h3>Create an isolated checkout</h3>
      <code>kwt add -b feature/new-ui</code>
    </div>
  </div>
  <div>
    <span>2</span>
    <div>
      <h3>Run work where the branch lives</h3>
      <code>kwt exec feature/new-ui -- make test</code>
    </div>
  </div>
  <div>
    <span>3</span>
    <div>
      <h3>Return to its persistent workspace</h3>
      <code>kwt open feature/new-ui</code>
    </div>
  </div>
</div>

kwt also imports pull requests through a stable JSON contract, manages plain
directory workspaces, runs commands in exact worktree paths, and can publish an
advisory multi-machine view without syncing files or locking branches.

## Designed for automation without giving up control

Existing or remote branch checkouts are created inertly: kwt skips repository
setup commands and workspace launch until you inspect the files and explicitly
open the workspace. Pull-request imports use a protected session boundary and
preserve exact push routing. Multi-machine sync is disabled until you configure
it.

<div class="kwt-link-grid">
  <a href="reference/cli/"><strong>CLI reference</strong><span>Commands, flags, JSON, and attachment contracts</span></a>
  <a href="reference/configuration/"><strong>Configuration</strong><span>Paths, layouts, agents, setup, and trust</span></a>
  <a href="reference/pull-requests/"><strong>Pull-request automation</strong><span>Discover, import, and attach safely</span></a>
  <a href="releases/"><strong>Releases</strong><span>Versions, artifacts, and upgrade guidance</span></a>
</div>

<div class="kwt-closing">
  <p class="kwt-eyebrow">Ready when your next branch is</p>
  <h2>Start with one worktree. Keep the dashboard.</h2>
  <a class="md-button md-button--primary" href="get-started/install/">Install kwt</a>
  <a class="md-button" href="https://github.com/kenn-io/kwt">View the source</a>
</div>
