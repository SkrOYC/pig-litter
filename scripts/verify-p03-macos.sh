#!/usr/bin/env bash
set -euo pipefail
umask 077

candidate=${CANDIDATE_SHA:?}
repo=${CANDIDATE_ROOT:?}
driver=${DRIVER_ROOT:?}
evidence=${EVIDENCE_DIR:?}
scenario=${P03_SCENARIO:?}
mkdir -p "$evidence"

if [[ "${1:-}" == --run-prepared ]]; then
  [[ $# -eq 1 ]]
  actual_arch=$(go env GOARCH)
  case "$(uname -m):$actual_arch" in x86_64:amd64|arm64:arm64) ;; *) exit 1 ;; esac
  [[ "$(go version)" == "go version go1.27.4 darwin/$actual_arch" ]]
  [[ "${PIG_LITTER_SDK_DIR:?}" == "${PIG_LITTER_SOURCE_DIR:?}/extensions/sdk" ]]
  [[ "$(go env GOWORK)" == "${GOWORK:?}" && "$GOWORK" != off ]]
  [[ "$(shasum -a 256 "${PIG_LITTER_EXPECTED_PIG:?}" | awk '{print $1}')" == "${PIG_LITTER_EXPECTED_PIG_SHA256:?}" ]]
  cd "$repo"
  {
    go version
    go env GOOS GOARCH GOWORK GOTOOLCHAIN CGO_ENABLED
    printf 'source=%s\nsdk=%s\nhost=%s\n' "$PIG_LITTER_SOURCE_DIR" "$PIG_LITTER_SDK_DIR" "$PIG_LITTER_EXPECTED_PIG"
    cat "$GOWORK"
    shasum -a 256 pig-source.json patches/pig-0.7.0-integrated.patch "$PIG_LITTER_EXPECTED_PIG"
    file "$PIG_LITTER_EXPECTED_PIG"
  } > "$evidence/build-identity.txt"
  go -C scripts build -p 2 -mod=readonly -trimpath -buildvcs=false -o "$P03_SCRATCH/bin/pig-litter" ./cmd/pig-litter
  {
    shasum -a 256 "$P03_SCRATCH/bin/pig-litter"
    file "$P03_SCRATCH/bin/pig-litter"
    go version -m "$P03_SCRATCH/bin/pig-litter"
  } >> "$evidence/build-identity.txt"
  python3 "$driver/scripts/p03-macos.py" --scenario "$scenario" --pig "$PIG_LITTER_EXPECTED_PIG" --extension "$P03_SCRATCH/bin/pig-litter" --scratch "$P03_SCRATCH" --evidence "$evidence"
  [[ "$(shasum -a 256 "$PIG_LITTER_EXPECTED_PIG" | awk '{print $1}')" == "$PIG_LITTER_EXPECTED_PIG_SHA256" ]]
  exit
fi
[[ $# -eq 0 ]]

[[ "$candidate" =~ ^[0-9a-f]{40}$ ]]
[[ "$(git -C "$repo" rev-parse HEAD)" == "$candidate" ]]
[[ -z "$(git -C "$repo" status --porcelain=v1 --untracked-files=all)" ]]
[[ "$(uname -s)" == Darwin ]]
case "${PROBE_RUNNER_LABEL:?}:$(uname -m)" in
  macos-15-intel:x86_64) expected_arch=amd64 ;;
  macos-15:arm64) expected_arch=arm64 ;;
  *) printf 'Runner label and architecture differ.\n' >&2; exit 1 ;;
esac
case "$scenario" in
  p03-all|p03-team|p03-policy|p03-unselected) ;;
  *) printf 'Unknown P03 scenario.\n' >&2; exit 2 ;;
esac

git -C "$repo" rev-parse 'HEAD^{tree}' > "$evidence/candidate-tree.txt"
git -C "$driver" rev-parse HEAD > "$evidence/driver-commit.txt"
git -C "$driver" rev-parse 'HEAD^{tree}' > "$evidence/driver-tree.txt"
uname -a > "$evidence/uname.txt"
sw_vers > "$evidence/macos-version.txt"
df -P /private/tmp > "$evidence/scratch-filesystem.txt"
device=$(df -P /private/tmp | tail -1 | awk '{print $1}')
case "$device" in
  /dev/disk*) ;;
  *) printf 'The /private/tmp filesystem is not an identified disk: %s\n' "$device" >&2; exit 1 ;;
esac
[[ "$(cd /private/tmp && pwd -P)" == /private/tmp ]]
scratch=$(mktemp -d /private/tmp/p03.XXXXXXXX)
scratch_id=$(stat -f '%d:%i' "$scratch")
printf 'path=%s\nidentity=%s\n' "$scratch" "$scratch_id" > "$evidence/scratch.txt"
finish() {
  result=$?
  trap - EXIT
  printf '%s\n' "$result" > "$evidence/runner-exit.txt"
  if [[ "$result" == 0 && -f "$evidence/cleanup-ok" && "$(stat -f '%d:%i' "$scratch")" == "$scratch_id" ]]; then
    rm -rf "$scratch"
    printf 'removed verified private scratch\n' >> "$evidence/scratch.txt"
  else
    printf 'retained for failure analysis\n' >> "$evidence/scratch.txt"
  fi
  exit "$result"
}
trap finish EXIT

mkdir -p "$scratch/bin"
export GOTELEMETRY=off
export P03_SCRATCH="$scratch"

PIG_PLATFORM_ROOT_DIR="$repo" PIG_PLATFORM_SCRATCH_PARENT="$scratch" PIG_PLATFORM_CACHE_DIR='' \
  bash "$driver/scripts/verify-pig-platforms.sh" "$candidate" darwin "$expected_arch" "$evidence/native-platform" -- \
  bash "$driver/scripts/verify-p03-macos.sh" --run-prepared 2>&1 | tee "$evidence/build.log"
