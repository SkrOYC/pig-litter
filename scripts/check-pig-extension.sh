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
  *-pig-0.3.1/bin/pig) ;;
  *)
    printf 'expected pinned PiG from the devenv store, got %s\n' "$pig_path" >&2
    exit 1
    ;;
esac

pig_version="$(pig --version)"
case "$pig_version" in
  0.3.1+*) ;;
  *)
    printf 'expected PiG v0.3.1, got %s\n' "$pig_version" >&2
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
export GOPROXY=off
export GOENV=off
export GOCACHE="$temp_dir/go-cache"
export GOMODCACHE="$temp_dir/go-mod-cache"
workspace="$temp_dir/workspace"
mkdir -p "$PIG_HOME" "$PIG_CODING_AGENT_DIR" "$workspace/extensions/hello"
cd "$workspace"

pig extension init "$workspace/extensions/hello" --name hello --lang go --json >/dev/null
report="$(pig install --validate-only --json ./extensions/hello)"
printf '%s\n' "$report" | jq --exit-status --arg name hello '
  .valid == true
  and .registered == true
  and .name == $name
  and .definition.language == "go"
  and .definition.form == "factory"
  and (.tools | index("hello_ping")) != null
' >/dev/null

cat > ./piglet.yaml <<'PIGLET'
name: hello-agent
description: "Temporary Piglet validation"
extensions:
  - name: hello
    origins: [local:./extensions/hello]
PIGLET
pig piglet validate ./piglet.yaml --json --no-input \
  | jq --exit-status --arg name hello-agent '
      .valid == true
      and .name == $name
      and (.extensions | map(.name) | index("hello")) != null
      and (.errors | length) == 0
    ' >/dev/null

for settings_file in "$PIG_CODING_AGENT_DIR/settings.json" "$workspace/.pig/settings.json"; do
  if [[ -e "$settings_file" ]]; then
    printf 'extension validation wrote settings: %s\n' "$settings_file" >&2
    exit 1
  fi
done

printf 'PiG %s loaded the Go extension, registered hello_ping, and validated hello-agent with %s\n' "$pig_version" "$go_version"
