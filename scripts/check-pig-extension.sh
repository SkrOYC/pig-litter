#!/usr/bin/env bash
set -euo pipefail
registration_only=false
evidence_dir=""
case "$#" in
  0) ;;
  1)
    [[ "$1" == --registration-only ]] || { printf 'usage: %s [--registration-only [--evidence-dir DIR]]\n' "$0" >&2; exit 2; }
    registration_only=true
    ;;
  3)
    [[ "$1" == --registration-only && "$2" == --evidence-dir && -n "$3" ]] || { printf 'usage: %s [--registration-only [--evidence-dir DIR]]\n' "$0" >&2; exit 2; }
    registration_only=true
    mkdir -p "$3"
    evidence_dir="$(cd "$3" && pwd -P)"
    ;;
  *) printf 'usage: %s [--registration-only [--evidence-dir DIR]]\n' "$0" >&2; exit 2 ;;
esac
for command in pig go jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    printf 'required command is missing: %s\n' "$command" >&2
    exit 1
  fi
done
pig_path="$(command -v pig)"
temp_dir="$(mktemp -d)"
trap 'rm -rf "$temp_dir"' EXIT
if [[ -n "${PIG_LITTER_EXPECTED_PIG:-}" ]]; then
  expected_pig="$(perl -MCwd=abs_path -e 'print abs_path($ARGV[0])' "$PIG_LITTER_EXPECTED_PIG")"
  [[ "$(perl -MCwd=abs_path -e 'print abs_path($ARGV[0])' "$pig_path")" == "$expected_pig" ]] || { printf 'expected preview PiG %s, got %s\n' "$PIG_LITTER_EXPECTED_PIG" "$pig_path" >&2; exit 1; }
  pig_digest="$(shasum -a 256 "$pig_path" | awk '{ print $1 }')"
  if [[ -n "${PIG_LITTER_EXPECTED_PIG_SHA256_FILE:-}" ]]; then
    [[ "$pig_digest" == "$(cat "$PIG_LITTER_EXPECTED_PIG_SHA256_FILE")" ]] || { printf 'unexpected packaged PiG digest: %s\n' "$pig_digest" >&2; exit 1; }
  fi
  if [[ -n "${PIG_LITTER_EXPECTED_PIG_SHA256:-}" ]]; then
    [[ "$pig_digest" == "$PIG_LITTER_EXPECTED_PIG_SHA256" ]] || { printf 'unexpected preview PiG digest: %s\n' "$pig_digest" >&2; exit 1; }
  elif [[ -z "${PIG_LITTER_EXPECTED_PIG_SHA256_FILE:-}" ]]; then
    printf 'expected PiG digest is missing\n' >&2
    exit 1
  fi
  pig_platform="$(go env GOHOSTOS)/$(go env GOHOSTARCH)"
  mkdir -p "$temp_dir/pig-home/agent"
  pig_identity="$(PIG_HOME="$temp_dir/pig-home" PIG_CODING_AGENT_DIR="$temp_dir/pig-home/agent" PIG_USE_PI_DIRS=0 PIG_OFFLINE=1 PI_OFFLINE=1 pig verify --json)"
  if ! printf '%s\n' "$pig_identity" | jq --exit-status --arg digest "$pig_digest" --arg build "$PIG_LITTER_EXPECTED_PIG_BUILD" --arg platform "$pig_platform" '
    .verified == true and .identity.pigVersion == "0.7.0" and .identity.go == "go1.27.4"
    and .identity.platform == $platform and .identity.build == $build
    and (.targets | length == 1 and .[0].kind == "binary" and .[0].sha256 == $digest)
  ' >/dev/null; then
    printf 'unexpected preview PiG identity\n' >&2
    exit 1
  fi
  if [[ -n "$evidence_dir" ]]; then
    printf '%s\n' "$pig_identity" > "$evidence_dir/pig-preview-identity.json"
    shasum -a 256 "$pig_path" > "$evidence_dir/pig-preview-binary.sha256"
  fi
  pig_kind=preview
else
  case "$pig_path" in
    *-pig-0.7.0/bin/pig) ;;
    *) printf 'expected pinned stock PiG, got %s\n' "$pig_path" >&2; exit 1 ;;
  esac
  pig_kind=stock
fi
pig_version="$(pig --version)"
case "$pig_version" in
  0.7.0+*) ;;
  *) printf 'expected PiG v0.7.0, got %s\n' "$pig_version" >&2; exit 1 ;;
esac
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -n "${PIG_LITTER_SOURCE_DIR:-}" ]]; then
  source_dir="$(cd "$PIG_LITTER_SOURCE_DIR" && pwd -P)"
  for module in github.com/MichaelKinsy/PiG github.com/MichaelKinsy/PiG/extensions/sdk; do
    expected_dir="$source_dir"
    name=pig
    if [[ "$module" == */extensions/sdk ]]; then expected_dir="$source_dir/extensions/sdk"; name=sdk; fi
    module_info="$(go -C "$root/scripts" list -m -json "$module")"
    printf '%s\n' "$module_info" | jq --exit-status --arg dir "$expected_dir" '.Main == true and .Dir == $dir and .GoMod == ($dir + "/go.mod")' >/dev/null
    if [[ -n "$evidence_dir" ]]; then printf '%s\n' "$module_info" > "$evidence_dir/standalone-$name-module.json"; fi
  done
fi
if "$registration_only"; then
  printf 'Registration-only validation does not run unit, race, TUI, or iTerm2 checks.\n'
  mkdir -p "$root/extensions/pig-litter/dist"
  go -C "$root/scripts" build -mod=readonly -o "$root/extensions/pig-litter/dist/pig-litter" ./cmd/pig-litter
else
  bash "$root/scripts/test-pig-litter.sh"
fi
workspace="$temp_dir/workspace"
agent_dir="$temp_dir/pig-home/agent"
mkdir -p "$workspace/extensions" "$agent_dir"
cp -R "$root/extensions/pig-litter" "$workspace/extensions/pig-litter"
cp "$root/piglet.yaml" "$workspace/piglet.yaml"
cp "$root/litter.yaml" "$workspace/litter.yaml"
if [[ -n "${source_dir:-}" ]]; then
  bash "$root/scripts/prepare-pig-workspace.sh" "$source_dir" "$workspace" "$workspace/extensions/pig-litter" > /dev/null
fi
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
  if [[ "$form" == factory && -n "${source_dir:-}" ]]; then
    runner="$(find "$PIG_HOME/cache/cells/go" -type f -name runner -print -quit)"
    [[ -n "$runner" ]]
    go version -m "$runner" > "$temp_dir/source-runner-info.txt"
    awk -F '\t' -v dir="$source_dir" '
      $2 == "dep" && $3 == "github.com/MichaelKinsy/PiG" { if (getline && $2 == "=>" && $3 == dir) found = 1 }
      END { exit !found }
    ' "$temp_dir/source-runner-info.txt"
    staged_sdk="$(awk -F '\t' '
      $2 == "dep" && $3 == "github.com/MichaelKinsy/PiG/extensions/sdk" { if (getline && $2 == "=>") print $3 }
    ' "$temp_dir/source-runner-info.txt")"
    [[ "$staged_sdk" == "$PIG_HOME/"* && -d "$staged_sdk" ]]
    awk '/^\/\/go:embed / { for (i = 2; i <= NF; i++) { name = $i; sub(/^\.gz\//, "", name); sub(/\.gz$/, "", name); print name } }' "$source_dir/extensions/sdk/bundle_embed.go" | LC_ALL=C sort > "$temp_dir/expected-sdk-files.txt"
    (cd "$staged_sdk" && find . -type f ! -name .pig-sdk-version -print | sed 's#^\./##' | LC_ALL=C sort) > "$temp_dir/staged-sdk-files.txt"
    cmp "$temp_dir/expected-sdk-files.txt" "$temp_dir/staged-sdk-files.txt"
    while IFS= read -r path; do cmp "$source_dir/extensions/sdk/$path" "$staged_sdk/$path"; done < "$temp_dir/staged-sdk-files.txt"
    if [[ -n "$evidence_dir" ]]; then
      cp "$temp_dir/source-runner-info.txt" "$evidence_dir/source-runner-info.txt"
      cp "$temp_dir/staged-sdk-files.txt" "$evidence_dir/source-staged-sdk-files.txt"
      printf 'source=%s\nstaged_sdk=%s\nmatched_files=%s\n' "$source_dir" "$staged_sdk" "$(wc -l < "$temp_dir/staged-sdk-files.txt")" > "$evidence_dir/source-module-identity.txt"
      shasum -a 256 "$runner" > "$evidence_dir/source-runner.sha256"
    fi
  fi
  if [[ -n "$evidence_dir" ]]; then
    if [[ "$form" == factory ]]; then name=source; else name=standalone; fi
    printf '%s\n' "$report" > "$evidence_dir/$name-registration.json"
  fi
done
piglet_report="$(pig piglet validate ./piglet.yaml --json --no-input)"
printf '%s\n' "$piglet_report" | jq --exit-status '
  .valid == true and .name == "pig-litter"
  and (.extensions | map(.name) | index("pig-litter")) != null
  and (.errors | length) == 0
' >/dev/null
if [[ -n "$evidence_dir" ]]; then
  printf '%s\n' "$piglet_report" > "$evidence_dir/piglet-manifest.json"
fi
for path in "$agent_dir/settings.json" "$workspace/.pig/settings.json"; do
  if [[ -e "$path" ]]; then printf 'validation wrote settings: %s\n' "$path" >&2; exit 1; fi
done
printf 'PiG %s (%s host) registered the Go Resource and prebuilt standalone with six child tools and validated the Piglet\n' "$pig_version" "$pig_kind"
