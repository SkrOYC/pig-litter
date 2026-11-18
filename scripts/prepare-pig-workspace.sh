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
mv "$tmp/go.work" "$workspace/go.work"
if [[ -n "${PIG_LITTER_WORKSPACE_BRIDGE:-}" ]]; then
  bridge_parent="$(cd "$(dirname "$PIG_LITTER_WORKSPACE_BRIDGE")" && pwd -P)"
  bridge="$bridge_parent/$(basename "$PIG_LITTER_WORKSPACE_BRIDGE")"
  if [[ -L "$bridge" ]]; then
    previous="$(readlink "$bridge")"
    marker="$(dirname "$previous")/.pig-litter-workspace"
    [[ -f "$marker" && "$(cat "$marker")" == "$bridge" ]] || { printf 'workspace bridge is not managed: %s\n' "$bridge" >&2; exit 1; }
  elif [[ -e "$bridge" ]]; then
    printf 'workspace bridge already exists: %s\n' "$bridge" >&2
    exit 1
  fi
  printf '%s\n' "$bridge" > "$tmp/owner"
  mv "$tmp/owner" "$workspace/.pig-litter-workspace"
  ln -s "$workspace/go.work" "$tmp/bridge"
  mv -f "$tmp/bridge" "$bridge"
fi
printf '%s\n' "$workspace/go.work"
