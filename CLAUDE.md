---
title: "Agent Guidelines"
description: "Agent guidance for kwt development, repository commands, code conventions, and issue tracking."
last_edited: "2026-09-29"
---
# Agent Guidelines

@AGENTS.md

## Core Principles

- **Do NOT maintain backward compatibility** unless explicitly requested. Break things boldly.
- **Keep this file under 20-30 lines of instructions.**

---

## Project Overview

**Project type:** CLI tool — Git worktree manager
**Primary language:** Go

---

## Commands

```bash
make build          # Build to ./kwt
make test           # go test ./...
make lint           # golangci-lint run
make install        # Build and install kwt
```

---

## Code Conventions

- Follow the existing patterns in the codebase
- Prefer explicit over clever
- Delete dead code immediately

---

## Maintenance Notes

**Keep this file lean and current:**

1. **Review regularly** - stale instructions poison the agent's context
2. **CRITICAL: Keep total under 20-30 lines** - move detailed docs to separate files and reference them
3. **Update commands immediately** when workflows change
4. **Delete anything the agent can infer** from your code

<!-- BEGIN KATA (managed by `kata init --with-agents`) -->

## kata issue tracker

This project uses [kata](https://github.com/kenn-io/kata) as its shared issue
ledger. Run `kata quickstart` at the start of each session for the full agent
contract. The short version:

- Search before creating: `kata search "<keywords>" --agent`.
- Prefer updating existing issues over duplicates (`kata comment`, `kata label add`, `kata edit`).
- Default to `--agent` for ordinary reads and mutations; use `--json` only when a script needs structured data.
- Close only verified work: `kata close <ref> --done --message "<scope + verification>" --commit <sha>`.
- If work is incomplete, label `needs-review` and comment what remains rather than closing.
- Never `kata delete` or `kata purge` without explicit user authorization.

<!-- END KATA -->
