#!/usr/bin/env bash
set -euo pipefail
for command in pig go jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'required command is missing: %s\n' "$command" >&2
    exit 1
  fi
done
pig_path="$(command -v pig)"
case "$pig_path" in
  *-pig-0.4.1/bin/pig) ;;
  *) printf 'expected pinned PiG from the devenv store, got %s\n' "$pig_path" >&2; exit 1 ;;
esac
pig_version="$(pig --version)"
case "$pig_version" in
  0.4.1+*) ;;
  *) printf 'expected PiG v0.4.1, got %s\n' "$pig_version" >&2; exit 1 ;;
esac
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$root/scripts/test-pig-litter.sh"
temp_dir="$(mktemp -d)"
trap 'rm -rf "$temp_dir"' EXIT
workspace="$temp_dir/workspace"
agent_dir="$temp_dir/pig-home/agent"
mkdir -p "$workspace/extensions" "$agent_dir"
cp -R "$root/extensions/pig-litter" "$workspace/extensions/pig-litter"
cp "$root/piglet.yaml" "$workspace/piglet.yaml"
cp "$root/litter.yaml" "$workspace/litter.yaml"
cd "$workspace"
export PIG_HOME="$temp_dir/pig-home"
export PIG_CODING_AGENT_DIR="$agent_dir"
export PIG_USE_PI_DIRS=0 PIG_OFFLINE=1 PI_OFFLINE=1 GOPROXY=off GOENV=off
for artifact in ./extensions/pig-litter ./extensions/pig-litter/dist/pig-litter; do
  language=go
  form=factory
  if [[ -f "$artifact" ]]; then language=binary; form=standalone; fi
  report="$(pig install --validate-only --json "$artifact")"
  printf '%s\n' "$report" | jq --exit-status --arg language "$language" --arg form "$form" '
    .valid == true and .registered == true and .name == "pig-litter"
    and .definition.language == $language and .definition.form == $form
    and (.tools == ["litter_inspect", "litter_list", "litter_message", "litter_spawn", "litter_stop", "litter_wait"])
    and ((.commands // []) | map(if type == "string" then . else .name end) | index("pig-litter")) != null
  ' >/dev/null
done
pig piglet validate ./piglet.yaml --json --no-input | jq --exit-status '
  .valid == true and .name == "pig-litter"
  and (.extensions | map(.name) | index("pig-litter")) != null
  and (.errors | length) == 0
' >/dev/null
for path in "$agent_dir/settings.json" "$workspace/.pig/settings.json"; do
  if [[ -e "$path" ]]; then printf 'validation wrote settings: %s\n' "$path" >&2; exit 1; fi
done
printf 'PiG %s registered the Go Resource and prebuilt standalone with six child tools and validated the Piglet\n' "$pig_version"
