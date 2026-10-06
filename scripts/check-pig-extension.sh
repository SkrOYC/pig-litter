#!/usr/bin/env bash
set -euo pipefail

for command in pig jq bun node; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'required command is missing: %s\n' "$command" >&2
    exit 1
  fi
done

pig_path="$(command -v pig)"
case "$pig_path" in
  *-pig-0.4.1/bin/pig) ;;
  *)
    printf 'expected pinned PiG from the devenv store, got %s\n' "$pig_path" >&2
    exit 1
    ;;
esac

pig_version="$(pig --version)"
case "$pig_version" in
  0.4.1+*) ;;
  *)
    printf 'expected PiG v0.4.1, got %s\n' "$pig_version" >&2
    exit 1
    ;;
esac

node_version="$(node --version)"
case "$node_version" in
  v24.*) ;;
  *) printf 'expected Node 24, got %s\n' "$node_version" >&2; exit 1 ;;
esac

root="${DEVENV_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
bash "$root/scripts/test-pig-litter.sh"

temp_dir="$(mktemp -d)"
trap 'rm -rf "$temp_dir"' EXIT
export PIG_HOME="$temp_dir/pig-home"
export PIG_CODING_AGENT_DIR="$PIG_HOME/agent"
export PIG_CODING_AGENT_SESSION_DIR="$PIG_CODING_AGENT_DIR/sessions"
export PIG_USE_PI_DIRS=0
export PIG_OFFLINE=1
export PI_OFFLINE=1
export GOPROXY=off
export GOENV=off
workspace="$temp_dir/workspace"
mkdir -p "$PIG_HOME" "$PIG_CODING_AGENT_DIR" "$workspace/extensions"
mkdir -p "$workspace/extensions/pig-litter/dist"
cp "$root/extensions/pig-litter/dist/pig-litter.mjs" "$workspace/extensions/pig-litter/dist/pig-litter.mjs"
cp "$root/piglet.yaml" "$workspace/piglet.yaml"
cp "$root/litter.yaml" "$workspace/litter.yaml"
cd "$workspace"

report="$(pig install --validate-only --json ./extensions/pig-litter/dist/pig-litter.mjs)"
printf '%s\n' "$report" | jq --exit-status --arg name pig-litter '
  .valid == true
  and .registered == true
  and .name == $name
  and .definition.language == "node"
  and .definition.form == "factory"
  and (.tools == ["litter_inspect", "litter_list", "litter_message", "litter_spawn", "litter_stop", "litter_wait"])
  and ((.commands // []) | map(if type == "string" then . else .name end) | index("pig-litter")) != null
' >/dev/null

pig piglet validate ./piglet.yaml --json --no-input \
  | jq --exit-status --arg name pig-litter '
      .valid == true
      and .name == $name
      and (.extensions | map(.name) | index("pig-litter")) != null
      and (.errors | length) == 0
    ' >/dev/null

for settings_file in "$PIG_CODING_AGENT_DIR/settings.json" "$workspace/.pig/settings.json"; do
  if [[ -e "$settings_file" ]]; then
    printf 'extension validation wrote settings: %s\n' "$settings_file" >&2
    exit 1
  fi
done

printf 'PiG %s loaded the Node extension with six background child tools and validated its Piglet with %s\n' "$pig_version" "$node_version"
