#!/usr/bin/env bash
set -euo pipefail
umask 077

candidate=${CANDIDATE_SHA:?}
baseline=939b4c3f5f2ba8c7f5becef66dbb0ca6eeb14ea7
repo=${CANDIDATE_CHECKOUT:?}
base=${BASELINE_CHECKOUT:?}
evidence=${EVIDENCE_DIR:?}
work=${RUNNER_TEMP:?}/p02-build
mkdir -p "$evidence"

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

date -u +%FT%TZ > "$evidence/started-at.txt"
printf 'candidate=%s\nbaseline=%s\nworkflow_sha=%s\nrepository=%s\nbaseline_checkout=%s\nrunner_temp=%s\n' "$candidate" "$baseline" "${GITHUB_SHA:?}" "$repo" "$base" "$RUNNER_TEMP" > "$evidence/inputs.txt"
[[ "$candidate" =~ ^[0-9a-f]{40}$ && "$candidate" == "$GITHUB_SHA" ]] || { printf 'candidate does not match the workflow SHA\n' >&2; exit 2; }
[[ "$(git -C "$repo" rev-parse HEAD)" == "$candidate" ]] || { printf 'candidate checkout does not match its SHA\n' >&2; exit 2; }
[[ "$(git -C "$base" rev-parse HEAD)" == "$baseline" ]] || { printf 'baseline checkout does not match its SHA\n' >&2; exit 2; }
[[ "$(git -C "$repo" merge-base "$candidate" "$baseline")" == "$baseline" ]] || { printf 'baseline is not the candidate merge base\n' >&2; exit 2; }
[[ -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]] || { printf 'candidate checkout is not clean\n' >&2; exit 2; }
[[ -z "$(git -C "$base" status --porcelain=v1 --untracked-files=all)" ]] || { printf 'baseline checkout is not clean\n' >&2; exit 2; }
[[ -n "$(git -C "$repo" for-each-ref --contains "$candidate" --format='%(refname)' refs/heads/test/collaboration-parity-contract refs/remotes/origin/test/collaboration-parity-contract)" ]] || { printf 'candidate is not on the declared branch\n' >&2; exit 2; }
[[ -n "$(git -C "$base" for-each-ref --contains "$baseline" --format='%(refname)' refs/heads/master refs/remotes/origin/master)" ]] || { printf 'baseline is not on master\n' >&2; exit 2; }
git -C "$repo" rev-parse HEAD^{tree} > "$evidence/candidate-tree.txt"
git -C "$base" rev-parse HEAD^{tree} > "$evidence/baseline-tree.txt"

for directory in /tmp "$RUNNER_TEMP"; do
  filesystem=$(stat -f -c %T "$directory")
  printf '%s %s\n' "$directory" "$filesystem" >> "$evidence/filesystems.txt"
  case "$filesystem" in
    tmpfs|ramfs) printf '%s is memory-backed\n' "$directory" >&2; exit 1 ;;
  esac
done
scratch=$(mktemp -d /tmp/p02.XXXXXXXX)
scenario_evidence="$scratch/run"
mkdir -m 700 "$scenario_evidence"
printf 'original_scenario_evidence=%s\n' "$scenario_evidence" > "$scenario_evidence/workflow-root.txt"
if [[ -n "${GITHUB_ENV:-}" ]]; then printf 'P02_SCENARIO_EVIDENCE=%s\n' "$scenario_evidence" >> "$GITHUB_ENV"; fi
export TMPDIR="$scratch" GOTMPDIR="$scratch" GOENV=off GOFLAGS= GOTOOLCHAIN=local GOTELEMETRY=off
mkdir -p "$work/candidate/bin" "$work/candidate/cache" "$work/candidate/modules" "$work/candidate/gopath"
mkdir -p "$work/base/bin" "$work/base/cache" "$work/base/modules" "$work/base/gopath"
ffmpeg -hide_banner -devices > "$evidence/ffmpeg-devices.txt" 2>&1
grep x11grab "$evidence/ffmpeg-devices.txt" > "$evidence/x11grab.txt"
for tool in ffmpeg ffprobe Xvfb xterm tmux jq; do command -v "$tool"; done > "$evidence/tools.txt"
uname -a > "$evidence/uname.txt"
lscpu > "$evidence/lscpu.txt"
cat /proc/loadavg > "$evidence/load-before.txt"

(
  cd "$repo"
  devenv shell -- env GOCACHE="$work/candidate/cache" GOMODCACHE="$work/candidate/modules" GOPATH="$work/candidate/gopath" GOTMPDIR="$scratch" go -C scripts build -p 2 -mod=readonly -buildvcs=false -trimpath -o "$work/candidate/bin/pig-litter" ./cmd/pig-litter
  devenv shell -- bash -c 'command -v pig; go version; printf "workspace=%s\nsource=%s\n" "$GOWORK" "$PIG_LITTER_SOURCE_DIR"' > "$evidence/candidate-environment.txt"
)
(
  cd "$base"
  devenv shell -- env GOCACHE="$work/base/cache" GOMODCACHE="$work/base/modules" GOPATH="$work/base/gopath" GOTMPDIR="$scratch" go -C scripts build -p 2 -mod=readonly -buildvcs=false -trimpath -o "$work/base/bin/pig-litter" ./cmd/pig-litter
  devenv shell -- bash -c 'command -v pig; go version; printf "workspace=%s\nsource=%s\n" "$GOWORK" "$PIG_LITTER_SOURCE_DIR"' > "$evidence/baseline-environment.txt"
)
grep -Fx 'go version go1.27.4 linux/amd64' "$evidence/candidate-environment.txt" > /dev/null
grep -Fx 'go version go1.27.4 linux/amd64' "$evidence/baseline-environment.txt" > /dev/null
candidate_host=$(head -n 1 "$evidence/candidate-environment.txt")
baseline_host=$(head -n 1 "$evidence/baseline-environment.txt")
candidate_sha=$(sha256sum "$work/candidate/bin/pig-litter" | cut -d' ' -f1)
baseline_sha=$(sha256sum "$work/base/bin/pig-litter" | cut -d' ' -f1)
candidate_host_sha=$(sha256sum "$candidate_host" | cut -d' ' -f1)
baseline_host_sha=$(sha256sum "$baseline_host" | cut -d' ' -f1)
catalog_sha=$(sha256sum "$repo/scripts/testdata/collaboration/requirements.json" | cut -d' ' -f1)
jq -n --arg product "$candidate_sha" --arg host "$candidate_host_sha" --arg catalog "$catalog_sha" --arg commit "$candidate" '{branch:"test/collaboration-parity-contract",product_commit:$commit,driver_commit:$commit,product_binary_sha256:$product,host_binary_sha256:$host,catalog_sha256:$catalog}' > "$evidence/target.json"
jq -n --arg product "$baseline_sha" --arg host "$baseline_host_sha" --arg catalog "$catalog_sha" --arg commit "$baseline" --arg driver "$candidate" '{branch:"master",product_commit:$commit,driver_commit:$driver,product_binary_sha256:$product,host_binary_sha256:$host,catalog_sha256:$catalog}' > "$evidence/baseline-target.json"
mkdir -p "$evidence/selected-binaries"
cp -L "$work/candidate/bin/pig-litter" "$evidence/selected-binaries/candidate-pig-litter"
cp -L "$work/base/bin/pig-litter" "$evidence/selected-binaries/baseline-pig-litter"
cp -L "$candidate_host" "$evidence/selected-binaries/candidate-pig"
cp -L "$baseline_host" "$evidence/selected-binaries/baseline-pig"
sha256sum "$evidence"/selected-binaries/* > "$evidence/selected-binaries.sha256"
vmstat 1 10 > "$evidence/pre-measurement-vmstat.txt"
cat /proc/loadavg > "$evidence/pre-measurement-load.txt"
vmstat -n 5 > "$evidence/measurement-vmstat.txt" &
monitor_pid=$!

cd "$repo"
run=(devenv shell -- scripts/verify-collaboration.sh --scenario capture-performance
  --target "$evidence/target.json" --target-checkout "$repo"
  --pig-bin "$candidate_host" --extension "$work/candidate/bin/pig-litter"
  --baseline-target "$evidence/baseline-target.json" --baseline-checkout "$base"
  --baseline-pig-bin "$baseline_host" --baseline-extension "$work/base/bin/pig-litter"
  --evidence-root "$scenario_evidence" --performance-pairs 20 --performance-warmups 5
  --media-ffmpeg "$(command -v ffmpeg)" --media-ffprobe "$(command -v ffprobe)"
  --media-xvfb "$(command -v Xvfb)" --media-xterm "$(command -v xterm)")
jq -n --args '$ARGS.positional' -- "${run[@]}" > "$evidence/scenario-argv.json"
"${run[@]}" > >(tee "$evidence/scenario.stdout") 2> >(tee "$evidence/scenario.stderr" >&2)
