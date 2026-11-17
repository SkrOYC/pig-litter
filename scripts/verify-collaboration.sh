#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
evidence_root="$root/.pstack/evidence/collaboration"
previous=
for argument in "$@"; do
  if [[ "$previous" == --evidence-root ]]; then
    evidence_root="$argument"
  elif [[ "$argument" == --evidence-root=* ]]; then
    evidence_root="${argument#--evidence-root=}"
  fi
  previous="$argument"
done
[[ "$evidence_root" == /* ]] || { printf 'evidence root must be absolute\n' >&2; exit 2; }
mkdir -p "$evidence_root"
evidence_root="$(cd "$evidence_root" && pwd -P)"
bootstrap="$(mktemp -d "$evidence_root/bootstrap-XXXXXXXX")"
chmod 700 "$bootstrap"
mkdir -m 700 "$bootstrap/home" "$bootstrap/cache" "$bootstrap/modules" "$bootstrap/gopath" "$bootstrap/tmp" "$bootstrap/gotmp" "$bootstrap/config" "$bootstrap/data" "$bootstrap/state" "$bootstrap/runtime"
jq -n --args '$ARGS.positional' -- "$@" > "$bootstrap/arguments.json"
export HOME="$bootstrap/home" TMPDIR="$bootstrap/tmp" GOTMPDIR="$bootstrap/gotmp"
export GOCACHE="$bootstrap/cache" GOMODCACHE="$bootstrap/modules" GOPATH="$bootstrap/gopath"
export GOENV=off GOTELEMETRY=off XDG_CONFIG_HOME="$bootstrap/config" XDG_CACHE_HOME="$bootstrap/cache"
export XDG_DATA_HOME="$bootstrap/data" XDG_STATE_HOME="$bootstrap/state" XDG_RUNTIME_DIR="$bootstrap/runtime"
child=
child_start=
child_group=false
child_stage=build
interrupted=false
forced=false
joined=false
group_alive=false
process_start() {
  local process_stat
  local -a process_fields
  [[ -r "/proc/$1/stat" ]] || return 1
  process_stat="$(cat "/proc/$1/stat")" || return 1
  read -r -a process_fields <<< "${process_stat##*) }"
  [[ "${#process_fields[@]}" -gt 19 ]] || return 1
  printf '%s' "${process_fields[19]}"
}
on_signal() {
  interrupted=true
  if [[ -n "$child" ]]; then
    if [[ "$child_stage" == build && "$child_group" == true ]]; then
      kill -TERM -- "-$child" 2>/dev/null || true
    else
      kill -TERM "$child" 2>/dev/null || true
    fi
  fi
}
trap on_signal INT TERM

build=(go -C "$root/scripts" build -mod=readonly -modcacherw -o "$bootstrap/verify-lifecycle" ./cmd/verify-lifecycle)
if command -v setsid >/dev/null 2>&1; then
  build=(setsid "${build[@]}")
  child_group=true
fi
printf '%q ' "${build[@]}" > "$bootstrap/build-command.txt"
printf '\n' >> "$bootstrap/build-command.txt"
"${build[@]}" > "$bootstrap/build.stdout" 2> "$bootstrap/build.stderr" &
child=$!
child_start="$(process_start "$child" || true)"
if [[ "$interrupted" == true ]]; then on_signal; fi

finish_interrupted() {
  local attempt current
  for ((attempt=0; attempt<20; attempt++)); do
    if ! kill -0 "$child" 2>/dev/null; then break; fi
    sleep 1
  done
  if kill -0 "$child" 2>/dev/null; then
    current="$(process_start "$child" || true)"
    if [[ -n "$child_start" && "$current" == "$child_start" ]]; then
      forced=true
      if [[ "$child_group" == true ]]; then
        kill -KILL -- "-$child" 2>/dev/null || true
      else
        kill -KILL "$child" 2>/dev/null || true
      fi
    fi
  fi
  for ((attempt=0; attempt<5; attempt++)); do
    if ! kill -0 "$child" 2>/dev/null; then break; fi
    sleep 1
  done
  if ! kill -0 "$child" 2>/dev/null; then
    wait "$child" 2>/dev/null || true
    joined=true
  fi
  if [[ "$child_group" == true ]]; then
    for ((attempt=0; attempt<5; attempt++)); do
      if ! kill -0 -- "-$child" 2>/dev/null; then break; fi
      sleep 1
    done
    if kill -0 -- "-$child" 2>/dev/null; then group_alive=true; fi
  fi
}
set +e
wait "$child"
status=$?
if [[ "$interrupted" == true ]]; then finish_interrupted; status=130; else joined=true; fi
set -e
if (( status != 0 )); then
  jq -n --argjson code "$status" --argjson pid "$child" --arg start "$child_start" --argjson interrupted "$interrupted" --argjson forced "$forced" --argjson joined "$joined" --argjson group_owned "$child_group" --argjson group_alive "$group_alive" '{stage:"build", outcome:"inconclusive", exit_code:$code, child_pid:$pid, child_start_ticks:$start, interrupted:$interrupted, forced_cleanup:$forced, joined:$joined, process_group_owned:$group_owned, process_group_alive:$group_alive}' > "$bootstrap/result.json"
  cat "$bootstrap/build.stderr" >&2
  printf 'bootstrap evidence: %s\n' "$bootstrap" >&2
  exit "$status"
fi

child=
child_start=
child_group=false
child_stage=driver
joined=false
"$bootstrap/verify-lifecycle" collaboration --repo-root "$root" --evidence-root "$evidence_root" "$@" &
child=$!
child_start="$(process_start "$child" || true)"
if [[ "$interrupted" == true ]]; then on_signal; fi
set +e
wait "$child"
status=$?
if [[ "$interrupted" == true ]]; then finish_interrupted; status=130; else joined=true; fi
set -e
jq -n --argjson code "$status" --argjson pid "$child" --arg start "$child_start" --argjson interrupted "$interrupted" --argjson forced "$forced" --argjson joined "$joined" '{stage:"driver", exit_code:$code, child_pid:$pid, child_start_ticks:$start, interrupted:$interrupted, forced_cleanup:$forced, joined:$joined}' > "$bootstrap/result.json"
printf 'bootstrap evidence: %s\n' "$bootstrap" >&2
exit "$status"
