#!/usr/bin/env bash
# Materialize website binaries from the orphan website-assets branch. The
# checked-out files are ignored so binary history stays off development branches.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
asset_dir="${KWT_DOCS_ASSET_DIR:-$repo_root/docs/assets}"
asset_remote="${KWT_WEBSITE_ASSETS_REMOTE:-origin}"
asset_branch="${KWT_WEBSITE_ASSETS_BRANCH:-website-assets}"
raw_root="${KWT_WEBSITE_ASSETS_RAW_ROOT:-https://raw.githubusercontent.com/kenn-io/kwt/website-assets}"
asset_name="og.png"
manifest="$asset_dir/.website-assets.synced"
stage_root="$(mktemp -d "${TMPDIR:-/tmp}/kwt-docs-assets.XXXXXX")"
asset_repository="$stage_root/repository.git"
trap 'rm -rf "$stage_root"' EXIT

valid_png() {
  local path="$1"
  [[ -f "$path" ]] \
    && [[ "$(od -An -tx1 -N8 "$path" | tr -d ' \n')" == "89504e470d0a1a0a" ]]
}

stage_ref() {
  local repository="$1" ref="$2"
  git -C "$repository" cat-file -e "$ref:$asset_name" 2>/dev/null \
    && git -C "$repository" show "$ref:$asset_name" > "$stage_root/$asset_name" \
    && valid_png "$stage_root/$asset_name"
}

stage_raw() {
  curl -fsSL --max-time 30 -o "$stage_root/$asset_name" \
    "$raw_root/$asset_name" 2>/dev/null \
    && valid_png "$stage_root/$asset_name"
}

publish_asset() {
  local source_label="$1" digest
  mkdir -p "$asset_dir"
  digest="$(shasum -a 256 "$stage_root/$asset_name" | awk '{print $1}')"
  printf '%s  %s\n' "$digest" "$asset_name" > "$stage_root/.website-assets.synced"
  mv -f "$stage_root/$asset_name" "$asset_dir/$asset_name"
  mv -f "$stage_root/.website-assets.synced" "$manifest"
  printf 'synced docs asset from %s\n' "$source_label"
}

resolved_remote="$asset_remote"
if remote_url="$(git -C "$repo_root" remote get-url "$asset_remote" 2>/dev/null)"; then
  resolved_remote="$remote_url"
fi

git init --bare -q "$asset_repository"
if git -C "$asset_repository" fetch --depth=1 --no-tags \
  "$resolved_remote" "$asset_branch" >/dev/null 2>&1; then
  if ! stage_ref "$asset_repository" FETCH_HEAD; then
    printf 'error: fetched %s is missing a valid %s\n' "$asset_branch" "$asset_name" >&2
    exit 1
  fi
  publish_asset "$asset_remote/$asset_branch"
  exit 0
fi

if [[ "$asset_remote" == "origin" ]] && stage_ref "$repo_root" "$asset_branch"; then
  publish_asset "local $asset_branch"
  exit 0
fi

if [[ "$asset_remote" == "origin" ]] && stage_raw; then
  publish_asset "raw.githubusercontent.com"
  exit 0
fi

if [[ -f "$manifest" ]] && (cd "$asset_dir" && shasum -a 256 -c "$(basename "$manifest")" >/dev/null 2>&1); then
  printf 'warning: website-assets unavailable; keeping verified local asset\n' >&2
  exit 0
fi

printf 'error: could not materialize %s from %s/%s\n' \
  "$asset_name" "$asset_remote" "$asset_branch" >&2
exit 1
