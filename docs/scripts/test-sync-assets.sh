#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sync_script="$repo_root/docs/scripts/sync-assets.sh"
scratch_root="$(mktemp -d "${TMPDIR:-/tmp}/kwt-docs-assets-test.XXXXXX")"
trap 'rm -rf "$scratch_root"' EXIT

asset_repo="$scratch_root/assets"
destination="$scratch_root/materialized"

git init -q -b website-assets "$asset_repo"
git -C "$asset_repo" config user.name "kwt docs test"
git -C "$asset_repo" config user.email "docs-test@kwt.sh"
printf '\211PNG\r\n\032\nfixture' > "$asset_repo/og.png"
git -C "$asset_repo" add og.png
git -C "$asset_repo" commit -q -m "Add fixture asset"

KWT_WEBSITE_ASSETS_REMOTE="$asset_repo" \
KWT_DOCS_ASSET_DIR="$destination" \
  "$sync_script"

cmp "$asset_repo/og.png" "$destination/og.png"
test -f "$destination/.website-assets.synced"

printf 'preserve-me' > "$destination/og.png"
git -C "$asset_repo" rm -q og.png
git -C "$asset_repo" commit -q -m "Remove fixture asset"

if KWT_WEBSITE_ASSETS_REMOTE="$asset_repo" \
  KWT_DOCS_ASSET_DIR="$destination" \
  "$sync_script" 2>/dev/null; then
  echo "sync unexpectedly accepted an incomplete asset branch" >&2
  exit 1
fi

test "$(cat "$destination/og.png")" = "preserve-me"
