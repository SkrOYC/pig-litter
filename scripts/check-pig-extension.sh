#!/usr/bin/env bash
set -euo pipefail

for command in pig go jq bun; do
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

go_version="$(go env GOVERSION)"
case "$go_version" in
  go1.27.1) ;;
  *)
    printf 'expected Go 1.27.1, got %s\n' "$go_version" >&2
    exit 1
    ;;
esac

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
export GOCACHE="$temp_dir/go-cache"
export GOMODCACHE="$temp_dir/go-mod-cache"
workspace="$temp_dir/workspace"
mkdir -p "$PIG_HOME" "$PIG_CODING_AGENT_DIR" "$workspace/extensions"
cp -R "$DEVENV_ROOT/extensions/pig-litter" "$workspace/extensions/pig-litter"
cp "$DEVENV_ROOT/piglet.yaml" "$workspace/piglet.yaml"
cd "$workspace"

report="$(pig install --validate-only --json ./extensions/pig-litter)"
printf '%s\n' "$report" | jq --exit-status --arg name pig-litter '
  .valid == true
  and .registered == true
  and .name == $name
  and .definition.language == "go"
  and .definition.form == "factory"
  and (.tools == ["pig_litter_agent"])
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

printf 'PiG %s loaded the pig-litter Go extension with exactly the pig_litter_agent tool and validated its Piglet with %s\n' "$pig_version" "$go_version"
