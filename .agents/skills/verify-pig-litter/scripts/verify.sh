#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd "$script_dir/../../../.." && pwd -P)"
mode="${1:-run}"

doctor() {
  for command_name in pig go tmux; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
      printf 'missing required command: %s\n' "$command_name" >&2
      return 1
    fi
  done
  [[ "$(pig --version)" == '0.4.1+1.0.3' ]] || {
    printf 'expected PiG 0.4.1+1.0.3\n' >&2; return 1;
  }
  [[ "$(go env GOVERSION)" == 'go1.27.1' ]] || {
    printf 'expected Go 1.27.1\n' >&2; return 1;
  }
  [[ "$(tmux -V)" == 'tmux 3.7c' ]] || {
    printf 'expected tmux 3.7c\n' >&2; return 1;
  }
  for path in extensions/pig-litter/adapter.go piglet.yaml scripts/verify-terminal-ui.sh; do
    [[ -f "$repo_root/$path" ]] || {
      printf 'required file is missing: %s\n' "$path" >&2; return 1;
    }
  done
  printf 'PiG 0.4.1+1.0.3, Go 1.27.1, tmux 3.7c; background-session terminal fixture is available\n'
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
