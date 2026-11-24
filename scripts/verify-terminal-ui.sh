#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_root="$root/.pstack/evidence/go-tui"
case_name= scenario= previous=
for argument in "$@"; do
  case "$argument" in
    --run-allocation|--run-allocation=*|-run-allocation|-run-allocation=*) printf 'the wrapper owns --run-allocation\n' >&2; exit 2 ;;
  esac
  if [[ "$previous" == --case ]]; then
    case_name="$argument"
  elif [[ "$previous" == --scenario ]]; then
    scenario="$argument"
  elif [[ "$argument" == --case=* ]]; then
    case_name="${argument#--case=}"
  elif [[ "$argument" == --scenario=* ]]; then
    scenario="${argument#--scenario=}"
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
snapshot_source() {
  (cd "$root" && git ls-files --cached --others --exclude-standard -z -- extensions/pig-litter scripts | LC_ALL=C sort -z | xargs -0 -r sha256sum) > "$1"
}
snapshot_source "$build_evidence/source-before.sha256"
git -C "$root" rev-parse HEAD > "$build_evidence/source-head.txt"
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
export GOMODCACHE="$run_root/modules" GOPATH="$run_root/gopath" GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local GOTELEMETRY=off
export XDG_CACHE_HOME="$run_root/cache" XDG_RUNTIME_DIR="$run_root/runtime"
export XDG_STATE_HOME="$run_root/state" XDG_DATA_HOME="$run_root/data"
export CCACHE_DIR="$run_root/cache/ccache" SCCACHE_DIR="$run_root/cache/sccache"
cd "$root"
overlay_args=()
if [[ "$scenario" == team-preview || "$scenario" == team-message || "$scenario" == team-controls || "$scenario" == team-tasks || "$scenario" == team-pause || "$scenario" == team-plans || "$scenario" == team-policy || "$scenario" == team-shutdown ]]; then
  setup_env=("GOPATH=$run_root/gopath" "GOCACHE=$run_root/cache" "GOMODCACHE=$run_root/modules" "GOTMPDIR=$run_root/gotmp" "TMPDIR=$run_root/tmp" GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local GOTELEMETRY=off)
  module_dir="$(devenv shell -- env "${setup_env[@]}" go -C "$root/scripts" mod download -modcacherw -json github.com/MichaelKinsy/PiG | jq -r '.Dir')"
  cp -a "$module_dir" "$run_root/pig-module"
  cp "$root/scripts/go.mod" "$run_root/p07.mod"
  cp "$root/scripts/go.sum" "$run_root/p07.sum"
  devenv shell -- env "${setup_env[@]}" go -C "$root/scripts" mod edit -modfile "$run_root/p07.mod" -replace "github.com/MichaelKinsy/PiG=$run_root/pig-module" -replace "github.com/SkrOYC/pig-litter/extensions/pig-litter=$root/extensions/pig-litter"
  devenv shell -- env "${setup_env[@]}" bun "$root/scripts/p07-resume-overlay.mjs" "$run_root/pig-module/coding/runtime.go" "$run_root" > "$build_evidence/resume-overlay.json"
  overlay_args=(-modfile "$run_root/p07.mod" -overlay "$run_root/p07-overlay.json")
fi
for command_name in pig-litter verify-tui tui-idle-observer; do
  build=(devenv shell -- env "GOPATH=$run_root/gopath" "GOCACHE=$run_root/cache" "GOMODCACHE=$run_root/modules" "GOTMPDIR=$run_root/gotmp" "TMPDIR=$run_root/tmp" GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local GOTELEMETRY=off go -C "$root/scripts" build "${overlay_args[@]}" -mod=readonly -modcacherw -o "$run_root/bin/$command_name" "./cmd/$command_name")
  printf '%q ' "${build[@]}" >> "$build_evidence/build-commands.txt"
  printf '\n' >> "$build_evidence/build-commands.txt"
  "${build[@]}" 2> >(tee -a "$build_evidence/build.stderr" >&2)
done
if [[ "$scenario" == team-preview || "$scenario" == team-message || "$scenario" == team-controls || "$scenario" == team-tasks || "$scenario" == team-pause || "$scenario" == team-plans || "$scenario" == team-policy || "$scenario" == team-shutdown ]]; then
  : > "$run_root/resume-probe.log"
  P07_RESUME_TRACE="$run_root/resume-probe.log" "$run_root/bin/verify-tui" --p07-resume-probe "$run_root"
  [[ "$(cat "$run_root/resume-probe.log")" == p07-missing-session ]] || { printf 'resume instrumentation positive control failed\n' >&2; exit 1; }
  cp "$run_root/resume-probe.log" "$build_evidence/resume-probe.log"
fi
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
snapshot_source "$build_evidence/source-after.sha256"
cmp "$build_evidence/source-before.sha256" "$build_evidence/source-after.sha256"
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
