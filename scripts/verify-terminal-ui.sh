#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_root="$root/.pstack/evidence/go-tui"
case_name= previous=
for argument in "$@"; do
  case "$argument" in
    --run-allocation|--run-allocation=*|-run-allocation|-run-allocation=*) printf 'the wrapper owns --run-allocation\n' >&2; exit 2 ;;
  esac
  if [[ "$previous" == --case ]]; then
    case_name="$argument"
  elif [[ "$argument" == --case=* ]]; then
    case_name="${argument#--case=}"
  elif [[ "$previous" == --evidence-root ]]; then
    evidence_root="$argument"
  elif [[ "$argument" == --evidence-root=* ]]; then
    evidence_root="${argument#--evidence-root=}"
  fi
  previous="$argument"
done
[[ "$evidence_root" == /* ]] || { printf 'evidence root must be absolute\n' >&2; exit 2; }
run_root="$(mktemp -d /tmp/plt.XXXXXXXX)"
run_identity="$(stat -c '%d:%i:%u' "$run_root")"
run_token="$(cat /proc/sys/kernel/random/uuid)"
owner_stat="$(cat "/proc/$$/stat")"
read -r -a owner_fields <<< "${owner_stat##*) }"
owner_start="${owner_fields[19]}"
verifier_pid=
retained=true
trap 'if [[ "$retained" == true ]]; then printf "terminal verification resources retained at %s\n" "$run_root" >&2; fi' EXIT
trap 'if [[ -n "$verifier_pid" ]]; then kill -TERM "$verifier_pid" 2>/dev/null || true; wait "$verifier_pid" || true; fi; exit 130' INT TERM
mkdir -p "$evidence_root"
evidence_root="$(cd "$evidence_root" && pwd -P)"
build_evidence="$(mktemp -d "$evidence_root/build-XXXXXXXX")"
chmod 700 "$build_evidence"
resource_paths=("$run_root")
resource_identities=("$run_identity")
for name in bin cache gotmp tmp modules runtime gopath state data; do
  mkdir -m 700 "$run_root/$name"
  resource_paths+=("$run_root/$name")
  resource_identities+=("$(stat -c '%d:%i:%u' "$run_root/$name")")
done
resources='[]'
for index in "${!resource_paths[@]}"; do
  resources="$(jq -cn --argjson resources "$resources" --arg path "${resource_paths[$index]}" --arg identity "${resource_identities[$index]}" '$resources + [{path:$path, identity:$identity}]')"
done
allocation="$build_evidence/allocation.json"
jq -n --arg root "$run_root" --arg token "$run_token" --argjson ownerPID "$$" --arg ownerStartTicks "$owner_start" --argjson resources "$resources" --arg evidence "$build_evidence" '{root:$root,token:$token,ownerPID:$ownerPID,ownerStartTicks:$ownerStartTicks,resources:$resources,evidence:$evidence}' > "$allocation"
chmod 600 "$allocation"
allocation_digest="$(sha256sum "$allocation")"
allocation_digest="${allocation_digest%% *}"
export GOCACHE="$run_root/cache" GOTMPDIR="$run_root/gotmp" TMPDIR="$run_root/tmp"
export GOMODCACHE="$run_root/modules" GOPATH="$run_root/gopath" GOENV=off GOTELEMETRY=off
export XDG_CACHE_HOME="$run_root/cache" XDG_RUNTIME_DIR="$run_root/runtime"
export XDG_STATE_HOME="$run_root/state" XDG_DATA_HOME="$run_root/data"
export CCACHE_DIR="$run_root/cache/ccache" SCCACHE_DIR="$run_root/cache/sccache"
for command_name in pig-litter verify-tui tui-idle-observer; do
  printf 'go -C %q build -mod=readonly -modcacherw -o %q %q\n' "$root/scripts" "$run_root/bin/$command_name" "./cmd/$command_name" >> "$build_evidence/build-commands.txt"
  go -C "$root/scripts" build -mod=readonly -modcacherw -o "$run_root/bin/$command_name" "./cmd/$command_name" 2> >(tee -a "$build_evidence/build.stderr" >&2)
done
observer_args=()
if [[ "$case_name" != unselected ]]; then
  observer_args=(--idle-observer "$run_root/bin/tui-idle-observer")
fi
"$run_root/bin/verify-tui" \
  "${observer_args[@]}" \
  --pig "$(command -v pig)" \
  --extension "$run_root/bin/pig-litter" \
  --piglet "$root/piglet.yaml" \
  --evidence-root "$evidence_root" \
  --run-allocation "$allocation" "$@" &
verifier_pid=$!
wait "$verifier_pid"
verifier_pid=
ack="$build_evidence/cleanup-ack.json"
jq -e --arg token "$run_token" --arg digest "$allocation_digest" '.token == $token and .allocationSHA256 == $digest and .ok == true' "$ack" >/dev/null
actual_digest="$(sha256sum "$allocation")"
[[ "${actual_digest%% *}" == "$allocation_digest" ]]
result_path="$(jq -r '.result' "$ack")"
result_digest="$(sha256sum "$result_path")"
[[ "${result_digest%% *}" == "$(jq -r '.resultSHA256' "$ack")" ]]
jq -e '.ok and .cleanup.providerJoined and .cleanup.observedProcessesExited and .cleanup.paneJoined and .cleanup.tmuxSocketClosed and (.cleanup.tmuxServerAlive | not)' "$result_path" >/dev/null
while IFS=$'\t' read -r process_pid process_start; do
  if [[ -r "/proc/$process_pid/stat" ]]; then
    process_stat="$(cat "/proc/$process_pid/stat")"
    read -r -a process_fields <<< "${process_stat##*) }"
    [[ "${process_fields[19]}" != "$process_start" ]] || { printf 'owned process remains alive: %s\n' "$process_pid" >&2; exit 1; }
  fi
done < <(jq -r '.processes[] | [.pid, .startTicks] | @tsv' "$ack")
for index in "${!resource_paths[@]}"; do
  resource="${resource_paths[$index]}"
  [[ -d "$resource" && ! -L "$resource" && "$(stat -c '%d:%i:%u' "$resource")" == "${resource_identities[$index]}" ]] || {
    printf 'owned resource was replaced: %s\n' "$resource" >&2; exit 1;
  }
done
rm -rf -- "$run_root"
retained=false
jq -n --arg root "$run_root" --arg identity "$run_identity" --arg allocationSHA256 "$allocation_digest" '{removed:$root,identity:$identity,allocationSHA256:$allocationSHA256,acknowledged:true}' > "$build_evidence/cleanup.json"
