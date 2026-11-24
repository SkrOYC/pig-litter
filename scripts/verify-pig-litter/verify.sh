#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../.." && pwd -P)"
mode="${1:-run}"

doctor() {
  for command_name in pig go tmux jq; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
      printf 'missing required command: %s\n' "$command_name" >&2
      return 1
    fi
  done
  for path in scripts/go.mod extensions/pig-litter/adapter.go piglet.yaml scripts/verify-terminal-ui.sh; do
    [[ -f "$repo_root/$path" ]] || {
      printf 'required file is missing: %s\n' "$path" >&2; return 1;
    }
  done
  local module_json expected_pig expected_go actual_pig actual_go actual_tmux
  module_json="$(GOENV=off GOFLAGS= GOTOOLCHAIN=local GOWORK=off go -C "$repo_root/scripts" mod edit -json)" || {
    printf 'cannot read scripts/go.mod\n' >&2; return 1;
  }
  expected_pig="$(jq -er '.Require[]? | select(.Path == "github.com/MichaelKinsy/PiG") | .Version | select(startswith("v")) | .[1:] | select(length > 0)' <<< "$module_json")" || {
    printf 'missing PiG root module requirement in scripts/go.mod\n' >&2; return 1;
  }
  expected_go="$(jq -er '.Toolchain | select(type == "string" and length > 0)' <<< "$module_json")" || {
    printf 'missing toolchain directive in scripts/go.mod\n' >&2; return 1;
  }
  actual_pig="$(pig --version)" || return 1
  [[ "${actual_pig%%+*}" == "$expected_pig" ]] || {
    printf 'expected PiG %s from scripts/go.mod, got %s\n' "$expected_pig" "$actual_pig" >&2; return 1;
  }
  actual_go="$(go -C "$repo_root/scripts" env GOVERSION)" || return 1
  [[ "$actual_go" == "$expected_go" ]] || {
    printf 'expected Go %s from scripts/go.mod, got %s\n' "$expected_go" "$actual_go" >&2; return 1;
  }
  actual_tmux="$(tmux -V)" || return 1
  [[ -n "$actual_tmux" ]] || { printf 'tmux -V returned no version\n' >&2; return 1; }
  printf 'PiG %s, Go %s, %s; background-session terminal fixture is available\n' "$actual_pig" "$actual_go" "$actual_tmux"
}

case "$mode" in
  doctor) doctor ;;
  run)
    for selection in direct piglet unselected; do
      doctor
      "$repo_root/scripts/verify-terminal-ui.sh" --case "$selection"
    done
    ;;
  *) printf 'usage: %s [run|doctor]\n' "$0" >&2; exit 2 ;;
esac
