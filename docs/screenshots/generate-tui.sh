#!/usr/bin/env bash
# Render docs/website/assets/dashboard.svg from a disposable kwt installation.
#
# Everything the dashboard shows comes from synthetic Git repositories under a
# private KWT_HOME, so the capture never touches the operator's own projects
# or daemon. The one exception is the live workspace row: kwt pins its
# workspace server to `tmux -L kwt` in the default socket directory and
# ignores TMUX_TMPDIR, so that single session is created on the operator's kwt
# server and removed by exact name on exit.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUTPUT="${1:-$REPO_ROOT/docs/website/assets/dashboard.svg}"
FREEZE_CONFIG="$REPO_ROOT/docs/screenshots/freeze.json"
COLUMNS_WANTED=104
ROWS_WANTED=13

for tool in go git tmux freeze; do
  command -v "$tool" >/dev/null || {
    echo "missing $tool" >&2
    exit 1
  }
done

umask 077
# Unix socket paths are limited to about 100 bytes, so the private tmux
# directory must stay short; macOS's per-user temp dir is already too long.
WORK="$(mktemp -d /tmp/kwt-shot.XXXXXX)"
CAPTURE_SOCKET="kwt-shot-$$"
LIVE_SESSION=""

# Strip inherited Git and tmux state so fixtures cannot reach the caller's repos.
for name in $(env | sed -n 's/^\(GIT_[A-Z_]*\)=.*/\1/p'); do unset "$name"; done
unset TMUX TMUX_PANE
export GIT_CONFIG_GLOBAL="$WORK/gitconfig"
export GIT_CONFIG_NOSYSTEM=1
export KWT_HOME="$WORK/home"
export TMUX_TMPDIR="$WORK/tmux"
mkdir -p "$KWT_HOME" "$TMUX_TMPDIR" "$WORK/src" "$WORK/wt"

cleanup() {
  tmux -L "$CAPTURE_SOCKET" kill-server 2>/dev/null || true
  if [[ -x "$WORK/kwt" ]]; then
    "$WORK/kwt" daemon stop >/dev/null 2>&1 || true
  fi
  if [[ -n "$LIVE_SESSION" ]]; then
    env -u TMUX_TMPDIR tmux -L kwt kill-session -t "=$LIVE_SESSION" 2>/dev/null || true
  fi
  if command -v trash >/dev/null; then
    trash "$WORK"
  else
    rm -r "$WORK"
  fi
}
trap cleanup EXIT

git config --file "$GIT_CONFIG_GLOBAL" user.name "Ada Example"
git config --file "$GIT_CONFIG_GLOBAL" user.email "ada@example.com"
git config --file "$GIT_CONFIG_GLOBAL" init.defaultBranch main

(cd "$REPO_ROOT" && go build -o "$WORK/kwt" ./cmd/kwt)
KWT="$WORK/kwt"

cat >"$KWT_HOME/config.toml" <<CONF
[worktree]
basedir = "$WORK/wt"

[naming]
template = "{{.FullPath}}/{{.Branch}}"
CONF

# commit <repo> <minutes-ago> <message>: commit everything in the repo, dated
# relative to now so the ACTIVITY column reads the same on any day.
commit() {
  local repo="$1" minutes="$2" message="$3"
  local when
  when="@$(python3 -c 'import sys, time; print(int(time.time() - 60 * float(sys.argv[1])))' "$minutes")"
  (cd "$repo" && git add -A && GIT_AUTHOR_DATE="$when" GIT_COMMITTER_DATE="$when" \
    git commit -q -m "$message")
}

# stamp <minutes-ago> <path...>: set modification times the ACTIVITY column reads.
stamp() {
  local minutes="$1"
  shift
  python3 - "$minutes" "$@" <<'PY'
import os, sys, time
when = time.time() - 60 * float(sys.argv[1])
for path in sys.argv[2:]:
    os.utime(path, (when, when))
PY
}

# track <worktree> <branch>: pretend the branch was pushed, without a network.
track() {
  (cd "$1" && git update-ref "refs/remotes/origin/$2" HEAD && git branch -q -u "origin/$2")
}

# seed_repo <name> <remote-url>: bare origin plus a primary clone with history.
seed_repo() {
  local name="$1" remote="$2"
  local bare="$WORK/src/$name.git" primary="$WORK/src/$name"
  git init -q --bare "$bare"
  git clone -q "$bare" "$primary" 2>/dev/null
  mkdir -p "$primary/cmd/$name" "$primary/internal/status"
  printf 'package main\n\nfunc main() {}\n' >"$primary/cmd/$name/main.go"
  printf 'package status\n' >"$primary/internal/status/status.go"
  printf '# %s\n' "$name" >"$primary/README.md"
  commit "$primary" 8640 "Initial import"
  printf 'package status\n\nfunc Collect() {}\n' >"$primary/internal/status/status.go"
  commit "$primary" 2900 "Add status collector"
  (cd "$primary" && git push -q origin main && git remote set-url origin "$remote")
  echo "$primary"
}

WIDGET="$(seed_repo widget https://github.com/acme/widget.git)"
ROUTER="$(seed_repo router https://github.com/acme/router.git)"

"$KWT" projects add "$WIDGET" >/dev/null
"$KWT" projects add "$ROUTER" >/dev/null

# Widget: a dirty branch that is ahead of origin, a fresh feature, a reviewed PR.
(cd "$WIDGET" && "$KWT" add -b fix/flaky-status --no-launch >/dev/null)
FLAKY="$(cd "$WIDGET" && "$KWT" get fix/flaky-status)"
track "$FLAKY" fix/flaky-status
printf 'package status\n\nfunc Collect() {}\n\nfunc retry() {}\n' >"$FLAKY/internal/status/status.go"
commit "$FLAKY" 40 "Retry flaky status probe"
printf 'package status\n\nfunc Collect() {}\n\nfunc retry() {}\n\nfunc backoff() {}\n' >"$FLAKY/internal/status/status.go"
printf 'package status\n' >"$FLAKY/internal/status/retry_test.go"
printf 'package status\n' >"$FLAKY/internal/status/backoff_test.go"

(cd "$WIDGET" && "$KWT" add -b feature/new-ui --no-launch >/dev/null)
NEWUI="$(cd "$WIDGET" && "$KWT" get feature/new-ui)"
mkdir -p "$NEWUI/internal/tui"
printf 'package tui\n' >"$NEWUI/internal/tui/tui.go"
commit "$NEWUI" 25 "Scaffold the dashboard"
track "$NEWUI" feature/new-ui

(cd "$WIDGET" && git branch -q contrib/pr-17 main)
(cd "$WIDGET" && "$KWT" add contrib/pr-17 >/dev/null)
PR17="$(cd "$WIDGET" && "$KWT" get contrib/pr-17)"
printf '# widget\n\nContributed fix.\n' >"$PR17/README.md"
commit "$PR17" 200 "Document the contributed fix"
printf 'package main\n\nfunc main() { run() }\n' >"$PR17/cmd/widget/main.go"
commit "$PR17" 185 "Wire the fix into main"
track "$PR17" contrib/pr-17
(cd "$PR17" && git reset -q --hard HEAD~2)

# Router: one long-lived branch with staged and unstaged work.
(cd "$ROUTER" && "$KWT" add -b perf/route-cache --no-launch >/dev/null)
CACHE="$(cd "$ROUTER" && "$KWT" get perf/route-cache)"
printf 'package status\n\nfunc Collect() {}\n\nvar cache map[string]int\n' >"$CACHE/internal/status/status.go"
(cd "$CACHE" && git add internal/status/status.go)
printf '# router\n\nSee docs.\n' >"$CACHE/README.md"

stamp 4 "$FLAKY" "$FLAKY"/internal/status/*
stamp 25 "$NEWUI"
stamp 70 "$CACHE" "$CACHE/internal/status/status.go" "$CACHE/README.md"
stamp 190 "$PR17"
stamp 2900 "$WIDGET" "$ROUTER"

# One live workspace so the WORKSPACE column shows a session. This is the only
# state created outside $WORK; cleanup kills exactly this session.
LIVE_SESSION="$("$KWT" open "$NEWUI" --start-session --layout none --json |
  python3 -c 'import json, sys; print(json.load(sys.stdin)["session_name"])')"
[[ -n "$LIVE_SESSION" ]] || {
  echo "kwt open did not report a session name" >&2
  exit 1
}

# Launch from a directory kwt will not register: it adopts any other launch
# directory as a project or workspace row and narrows the dashboard to it. The
# home directory is the one exception, so this process alone gets a scratch HOME.
tmux -L "$CAPTURE_SOCKET" -f /dev/null new-session -d -s shot -c "$WORK" \
  -x "$COLUMNS_WANTED" -y "$ROWS_WANTED" \
  -e HOME="$WORK" -e KWT_HOME="$KWT_HOME" -e TMUX_TMPDIR="$TMUX_TMPDIR" \
  -e GIT_CONFIG_GLOBAL="$GIT_CONFIG_GLOBAL" -e GIT_CONFIG_NOSYSTEM=1 \
  -e TERM=xterm-256color -e COLORTERM=truecolor \
  "$KWT tui"
tmux -L "$CAPTURE_SOCKET" set-option -g default-terminal "tmux-256color"
tmux -L "$CAPTURE_SOCKET" set-option -ga terminal-overrides ",*:Tc"

for _ in $(seq 1 60); do
  if tmux -L "$CAPTURE_SOCKET" capture-pane -pt shot | grep -q "route-cache"; then
    break
  fi
  sleep 0.25
done
sleep 1.5
tmux -L "$CAPTURE_SOCKET" send-keys -t shot g
sleep 0.5

# Show home-relative paths instead of the disposable temp directory.
tmux -L "$CAPTURE_SOCKET" capture-pane -pet shot |
  sed -e "s#/private$WORK/wt#~/worktrees#g" -e "s#$WORK/wt#~/worktrees#g" \
    -e "s#/private$WORK/src#~/code#g" -e "s#$WORK/src#~/code#g" >"$WORK/dashboard.ansi"
sed 's/\x1b\[[0-9;]*m//g' "$WORK/dashboard.ansi" >&2
# freeze reads stdin whenever stdin is not a terminal, so feed it explicitly.
freeze --language ansi -c "$FREEZE_CONFIG" -o "$OUTPUT" <"$WORK/dashboard.ansi"
echo "wrote $OUTPUT"
