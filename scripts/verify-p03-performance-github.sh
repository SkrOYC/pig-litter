#!/usr/bin/env bash
set -euo pipefail
umask 077

candidate=${CANDIDATE_SHA:?}
baseline=2dcb8f4ea03e941df6ca543af8c862d0d283b40b
repo=${CANDIDATE_CHECKOUT:?}
base=${BASELINE_CHECKOUT:?}
evidence=${EVIDENCE_DIR:?}
work=${RUNNER_TEMP:?}/p03-build
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
printf 'candidate=%s\nbaseline=%s\ngithub_sha=%s\nworkflow_file_sha=%s\nrun_id=%s\nrelease=%s\n' "$candidate" "$baseline" "${GITHUB_SHA:?}" "${GITHUB_WORKFLOW_SHA:?}" "${GITHUB_RUN_ID:?}" "${P03_RELEASE:?}" > "$evidence/inputs.txt"
[[ "$P03_RELEASE" == true ]] || { printf 'P03 measurement was not explicitly released\n' >&2; exit 2; }
[[ "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || { printf 'P03 measurement requires a GitHub-hosted runner\n' >&2; exit 2; }
[[ "$candidate" =~ ^[0-9a-f]{40}$ && "$candidate" == "$GITHUB_SHA" ]] || { printf 'candidate does not match the workflow SHA\n' >&2; exit 2; }
[[ "$(git -C "$repo" rev-parse HEAD)" == "$candidate" ]] || { printf 'candidate checkout does not match its SHA\n' >&2; exit 2; }
[[ "$(git -C "$base" rev-parse HEAD)" == "$baseline" ]] || { printf 'baseline checkout does not match its SHA\n' >&2; exit 2; }
[[ "$(git -C "$repo" merge-base "$candidate" "$baseline")" == "$baseline" ]] || { printf 'baseline is not an ancestor of the candidate\n' >&2; exit 2; }
[[ -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]] || { printf 'candidate checkout is not clean\n' >&2; exit 2; }
[[ -z "$(git -C "$base" status --porcelain=v1 --untracked-files=all)" ]] || { printf 'baseline checkout is not clean\n' >&2; exit 2; }
cmp "$repo/pig-source.json" "$base/pig-source.json"
git -C "$repo" rev-parse 'HEAD^{tree}' > "$evidence/candidate-tree.txt"
git -C "$base" rev-parse 'HEAD^{tree}' > "$evidence/baseline-tree.txt"

for directory in /tmp "$RUNNER_TEMP"; do
  filesystem=$(stat -f -c %T "$directory")
  printf '%s %s\n' "$directory" "$filesystem" >> "$evidence/filesystems.txt"
  case "$filesystem" in
    tmpfs|ramfs) printf '%s is memory-backed\n' "$directory" >&2; exit 1 ;;
  esac
done
scratch=$(mktemp -d /tmp/p03.XXXXXXXX)
scenario_evidence="$scratch/scenarios"
mkdir -m 700 "$scenario_evidence"
printf 'scenario_evidence=%s\n' "$scenario_evidence" > "$evidence/scenario-root.txt"
printf 'P03_SCENARIO_EVIDENCE=%s\n' "$scenario_evidence" >> "${GITHUB_ENV:?}"
export TMPDIR="$scratch" GOTMPDIR="$scratch" GOENV=off GOFLAGS='' GOTOOLCHAIN=local GOTELEMETRY=off PYTHONDONTWRITEBYTECODE=1
mkdir -p "$work/bin/candidate" "$work/bin/baseline" "$work/bin/instrumented" "$work/cache" "$work/modules" "$work/gopath"
export GOCACHE="$work/cache" GOMODCACHE="$work/modules" GOPATH="$work/gopath"
uname -a > "$evidence/uname.txt"
lscpu > "$evidence/lscpu.txt"
printf 'environment=%s\nname=%s\nos=%s\narch=%s\nimage_os=%s\nimage_version=%s\nref=%s\nattempt=%s\n' "${RUNNER_ENVIRONMENT:-unknown}" "${RUNNER_NAME:-unknown}" "${RUNNER_OS:-unknown}" "${RUNNER_ARCH:-unknown}" "${ImageOS:-unknown}" "${ImageVersion:-unknown}" "${GITHUB_REF:-unknown}" "${GITHUB_RUN_ATTEMPT:-unknown}" > "$evidence/runner.txt"
cat /proc/loadavg > "$evidence/load-before.txt"
sha256sum "$repo/pig-source.json" "$repo/scripts/p03-performance/prepare.py" "$repo/scripts/p03-performance/measure.py" "$repo/scripts/verify-p03-performance-github.sh" > "$evidence/source-sha256.txt"

python3 "$repo/scripts/p03-performance/prepare.py" prepare --checkout "$repo" --candidate "$candidate" --output "$scratch/overlay"
cp "$scratch/overlay/manifest.json" "$evidence/overlay-manifest.json"
for source in candidate baseline; do
  if [[ "$source" == candidate ]]; then checkout=$repo; else checkout=$base; fi
  (
    cd "$checkout"
    devenv shell -- bash -s > "$evidence/$source-environment.txt" <<'BASH'
set -euo pipefail
go version
printf 'host=%s\nworkspace=%s\nsource=%s\nsdk=%s\nexpected_host_sha256=%s\n' "$PIG_LITTER_EXPECTED_PIG" "$GOWORK" "$PIG_LITTER_SOURCE_DIR" "$PIG_LITTER_SDK_DIR" "$PIG_LITTER_EXPECTED_PIG_SHA256"
test "$PIG_LITTER_SDK_DIR" = "$PIG_LITTER_SOURCE_DIR/extensions/sdk"
test "$(go env GOWORK)" = "$GOWORK"
test "$(sha256sum "$PIG_LITTER_EXPECTED_PIG" | cut -d' ' -f1)" = "$PIG_LITTER_EXPECTED_PIG_SHA256"
sha256sum "$PIG_LITTER_EXPECTED_PIG"
cat "$GOWORK"
BASH
    devenv shell -- env GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOPATH="$GOPATH" GOTMPDIR="$GOTMPDIR" go -C scripts build -p 2 -mod=readonly -trimpath -buildvcs=false -o "$work/bin/$source/pig-litter" ./cmd/pig-litter
  )
  grep -Fx 'go version go1.27.4 linux/amd64' "$evidence/$source-environment.txt" > /dev/null
done
candidate_host=$(sed -n 's/^host=//p' "$evidence/candidate-environment.txt")
baseline_host=$(sed -n 's/^host=//p' "$evidence/baseline-environment.txt")
[[ "$(sha256sum "$candidate_host" | cut -d' ' -f1)" == "$(sha256sum "$baseline_host" | cut -d' ' -f1)" ]] || { printf 'packaged host binaries differ\n' >&2; exit 2; }
(
  cd "$repo"
  devenv shell -- env GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOPATH="$GOPATH" GOTMPDIR="$GOTMPDIR" go -C scripts build -p 2 -mod=readonly -trimpath -buildvcs=false -o "$work/bin/core-driver" ./cmd/verify-lifecycle
  devenv shell -- env GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOPATH="$GOPATH" GOTMPDIR="$GOTMPDIR" go -C scripts build -p 2 -mod=readonly -trimpath -buildvcs=false -overlay "$scratch/overlay/driver-overlay.json" -o "$work/bin/team-driver" ./cmd/verify-lifecycle
  devenv shell -- env GOCACHE="$GOCACHE" GOMODCACHE="$GOMODCACHE" GOPATH="$GOPATH" GOTMPDIR="$GOTMPDIR" go -C scripts build -p 2 -mod=readonly -trimpath -buildvcs=false -overlay "$scratch/overlay/extension-overlay.json" -o "$work/bin/instrumented/pig-litter" ./cmd/pig-litter
)
python3 "$repo/scripts/p03-performance/prepare.py" verify --candidate "$candidate" --output "$scratch/overlay"
sha256sum "$work/bin/"*/pig-litter "$work/bin/core-driver" "$work/bin/team-driver" "$candidate_host" "$baseline_host" > "$evidence/binaries.sha256"
python3 "$repo/scripts/p03-performance/measure.py" smoke --checkout "$repo" --candidate "$candidate" --overlay "$scratch/overlay" --instrumented-extension "$work/bin/instrumented/pig-litter" --team-driver "$work/bin/team-driver" --host "$candidate_host" --output "$scenario_evidence/smoke" | tee "$evidence/smoke.log"

jq -n --arg baseline "$baseline" --arg candidate "$candidate" --arg run_id "$GITHUB_RUN_ID" '{baseline:$baseline,candidate:$candidate,run_id:$run_id,root_released:true}' > "$evidence/release.json"
vmstat 1 10 > "$evidence/pre-measurement-vmstat.txt"
vmstat -n 5 > "$evidence/measurement-vmstat.txt" &
monitor_pid=$!
python3 "$repo/scripts/p03-performance/measure.py" run --checkout "$repo" --candidate "$candidate" --baseline-checkout "$base" --baseline-extension "$work/bin/baseline/pig-litter" --candidate-extension "$work/bin/candidate/pig-litter" --instrumented-extension "$work/bin/instrumented/pig-litter" --core-driver "$work/bin/core-driver" --team-driver "$work/bin/team-driver" --host "$candidate_host" --overlay "$scratch/overlay" --release "$evidence/release.json" --output "$scenario_evidence/measurement" | tee "$evidence/measurement.log"
