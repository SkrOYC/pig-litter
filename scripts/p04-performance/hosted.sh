#!/usr/bin/env bash
set -euo pipefail
umask 077

candidate=${CANDIDATE_SHA:?}
parent=bea36acff7b55ea4ed0f4a4feccde6b638eea4db
product=e52ab08e3efd31eb74e405bb745350fa463d7da1
repo=${CANDIDATE_CHECKOUT:?}
base=${BASELINE_CHECKOUT:?}
evidence=${EVIDENCE_DIR:?}
work=${RUNNER_TEMP:?}/p04-build
monitor_pid=

finish() {
  result=$?
  trap - EXIT
  if [[ -n "$monitor_pid" ]]; then
    kill "$monitor_pid" 2>/dev/null || true
    wait "$monitor_pid" 2>/dev/null || true
  fi
  cat /proc/loadavg > "$evidence/load-after.txt" || true
  date -u +%FT%TZ > "$evidence/finished-at.txt" || true
  printf '%s\n' "$result" > "$evidence/runner-exit.txt" || true
  exit "$result"
}
trap finish EXIT

mkdir -p "$evidence"
date -u +%FT%TZ > "$evidence/started-at.txt"
printf 'candidate=%s\nparent=%s\nproduct=%s\ngithub_sha=%s\nworkflow_sha=%s\nrun_id=%s\nrelease=%s\n' "$candidate" "$parent" "$product" "${GITHUB_SHA:?}" "${GITHUB_WORKFLOW_SHA:?}" "${GITHUB_RUN_ID:?}" "${P04_RELEASE:?}" > "$evidence/inputs.txt"
[[ "$P04_RELEASE" == true ]]
[[ "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
[[ "$candidate" =~ ^[0-9a-f]{40}$ && "$candidate" == "$GITHUB_SHA" ]]
[[ "$(git -C "$repo" rev-parse HEAD)" == "$candidate" ]]
[[ "$(git -C "$base" rev-parse HEAD)" == "$parent" ]]
[[ "$(git -C "$repo" merge-base "$candidate" "$parent")" == "$parent" ]]
[[ "$(git -C "$repo" merge-base "$candidate" "$product")" == "$product" ]]
[[ -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]]
[[ -z "$(git -C "$base" status --porcelain=v1 --untracked-files=all)" ]]
for directory in /tmp "$RUNNER_TEMP"; do
  filesystem=$(stat -f -c %T "$directory")
  printf '%s %s\n' "$directory" "$filesystem" >> "$evidence/filesystems.txt"
  [[ "$filesystem" != tmpfs && "$filesystem" != ramfs ]]
done

scratch=$(mktemp -d /tmp/p04.XXXXXXXX)
scenario="$scratch/scenarios"
mkdir -m 700 "$scenario"
printf 'P04_SCENARIO_EVIDENCE=%s\n' "$scenario" >> "${GITHUB_ENV:?}"
printf '%s\n' "$scenario" > "$evidence/scenario-root.txt"
export TMPDIR="$scratch" GOTMPDIR="$scratch" GOENV=off GOFLAGS='' GOTOOLCHAIN=local GOTELEMETRY=off
mkdir -p "$work/bin/parent" "$work/bin/head" "$work/cache" "$work/modules" "$work/gopath"
export GOCACHE="$work/cache" GOMODCACHE="$work/modules" GOPATH="$work/gopath"
uname -a > "$evidence/uname.txt"
lscpu > "$evidence/lscpu.txt"
ps -eo pid,ppid,comm > "$evidence/processes-before.txt"
printf 'environment=%s\nname=%s\nos=%s\narch=%s\nimage_os=%s\nimage_version=%s\nref=%s\nattempt=%s\n' "${RUNNER_ENVIRONMENT:-unknown}" "${RUNNER_NAME:-unknown}" "${RUNNER_OS:-unknown}" "${RUNNER_ARCH:-unknown}" "${ImageOS:-unknown}" "${ImageVersion:-unknown}" "${GITHUB_REF:-unknown}" "${GITHUB_RUN_ATTEMPT:-unknown}" > "$evidence/runner.txt"
cat /proc/loadavg > "$evidence/load-before.txt"
git -C "$repo" rev-parse 'HEAD^{tree}' > "$evidence/candidate-tree.txt"
git -C "$base" rev-parse 'HEAD^{tree}' > "$evidence/parent-tree.txt"
sha256sum "$repo"/scripts/p04-performance/* > "$evidence/harness.sha256"

for side in parent head; do
  if [[ "$side" == parent ]]; then checkout=$base; commit=$parent; else checkout=$repo; commit=$candidate; fi
  (
    cd "$checkout"
    devenv shell -- bash "$repo/scripts/p04-performance/build-side.sh" "$side" "$checkout" "$commit" "$work/bin/$side" "$evidence/$side-build"
  ) > "$evidence/$side-build.log" 2>&1
done
jq -n --slurpfile parent "$evidence/parent-build/manifest.json" --slurpfile head "$evidence/head-build/manifest.json" '{parent:$parent[0],head:$head[0]}' > "$evidence/build-manifest.json"
P04_EXPECTED_HEAD_TREE=$(cat "$evidence/candidate-tree.txt")
export P04_SOURCE_ROOT="$repo" P04_EXPECTED_HEAD="$candidate" P04_EXPECTED_HEAD_TREE P04_BUILD_MANIFEST="$evidence/build-manifest.json"
P04_RUN_MODE=smoke P04_OUTPUT="$scenario/smoke" bun "$repo/scripts/p04-performance/run.mjs" | tee "$evidence/smoke.log"

jq -n --arg candidate "$candidate" --arg parent "$parent" --arg run_id "$GITHUB_RUN_ID" --arg smoke "$scenario/smoke" \
  --arg manifest_sha256 "$(sha256sum "$evidence/build-manifest.json" | cut -d ' ' -f1)" \
  --arg smoke_identity_sha256 "$(sha256sum "$scenario/smoke/identity.json" | cut -d ' ' -f1)" \
  --arg smoke_summary_sha256 "$(sha256sum "$scenario/smoke/summary.json" | cut -d ' ' -f1)" \
  '{candidate:$candidate,parent:$parent,run_id:$run_id,smoke:$smoke,manifest_sha256:$manifest_sha256,smoke_identity_sha256:$smoke_identity_sha256,smoke_summary_sha256:$smoke_summary_sha256,root_released:true}' > "$evidence/release.json"
vmstat 1 10 > "$evidence/pre-measurement-vmstat.txt"
ps -eo pid,ppid,comm > "$evidence/processes-measurement.txt"
vmstat -n 5 > "$evidence/measurement-vmstat.txt" &
monitor_pid=$!
P04_RUN_MODE=measure P04_RELEASE_PATH="$evidence/release.json" P04_OUTPUT="$scenario/measurement" bun "$repo/scripts/p04-performance/run.mjs" | tee "$evidence/measurement.log"
