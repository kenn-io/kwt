#!/bin/sh
# Stands in for claude or codex in restore tests: records how a restored pane
# started it, keyed by the pane.
pane=$(printf %s "$TMUX_PANE" | tr -d %)
{
	printf 'name=%s\n' "$(basename "$0")"
	printf 'cwd=%s\n' "$(pwd -P)"
	printf 'args=%s\n' "$*"
} > "$FAKE_AGENT_LOG/$pane.tmp"
mv -f "$FAKE_AGENT_LOG/$pane.tmp" "$FAKE_AGENT_LOG/$pane"
