#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  printf 'usage: %s SOURCE_DIR WORKSPACE_DIR MODULE_DIR...\n' "$0" >&2
  exit 2
fi

source="$(cd "$1" && pwd -P)"
mkdir -p "$2"
workspace="$(cd "$2" && pwd -P)"
shift 2
modules=()
for module in "$@"; do modules+=("$(cd "$module" && pwd -P)"); done
tmp="$(mktemp -d "$workspace/.go-work.XXXXXXXX")"
trap 'rm -rf "$tmp"' EXIT
GOWORK=off go -C "$tmp" work init "${modules[@]}" "$source" "$source/extensions/sdk"
reuse_workspace=false
if [[ -n "${PIG_LITTER_WORKSPACE_BRIDGE:-}" ]]; then
  bridge_parent="$(cd "$(dirname "$PIG_LITTER_WORKSPACE_BRIDGE")" && pwd -P)"
  bridge="$bridge_parent/$(basename "$PIG_LITTER_WORKSPACE_BRIDGE")"
  if [[ -L "$bridge" ]]; then
    previous="$(readlink "$bridge")"
    marker="$(dirname "$previous")/.pig-litter-workspace"
    previous_dir="$(dirname "$previous")"
    previous_hash="$(basename "$previous_dir")"
    managed_parent="$(dirname "$workspace")"
    current_hash="$(basename "$workspace")"
    if [[ -e "$marker" || -L "$marker" ]]; then
      [[ -f "$marker" && "$(cat "$marker")" == "$bridge" ]] || { printf 'workspace bridge is not managed: %s\n' "$bridge" >&2; exit 1; }
    elif [[ "$previous" == "$workspace/go.work" && -f "$previous" ]] && cmp -s "$previous" "$tmp/go.work"; then
      reuse_workspace=true
    # This digest-keyed state namespace belongs to the workspace helper.
    elif [[ ! -e "$previous" && "$(basename "$managed_parent")" == pig-sdk &&
      "$(dirname "$previous_dir")" == "$managed_parent" && "$previous" == "$previous_dir/go.work" &&
      "$previous_hash" =~ ^[0-9a-f]{64}$ && "$current_hash" =~ ^[0-9a-f]{64}$ ]]; then
      :
    else
      printf 'workspace bridge is not managed: %s\n' "$bridge" >&2
      exit 1
    fi
  elif [[ -e "$bridge" ]]; then
    printf 'workspace bridge already exists: %s\n' "$bridge" >&2
    exit 1
  fi
fi
if [[ "$reuse_workspace" != true ]]; then mv "$tmp/go.work" "$workspace/go.work"; fi
if [[ -n "${bridge:-}" ]]; then
  printf '%s\n' "$bridge" > "$tmp/owner"
  mv "$tmp/owner" "$workspace/.pig-litter-workspace"
  if [[ "${previous:-}" != "$workspace/go.work" ]]; then
    ln -s "$workspace/go.work" "$tmp/bridge"
    mv -f "$tmp/bridge" "$bridge"
  fi
fi
printf '%s\n' "$workspace/go.work"
