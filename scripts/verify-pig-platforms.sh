#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $# -ne 4 ]]; then
  printf 'usage: %s CANDIDATE_SHA GOOS GOARCH EVIDENCE_DIR\n' "$0" >&2
  exit 2
fi

candidate_sha="$1"
expected_os="$2"
expected_arch="$3"
evidence_dir="$4"
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$evidence_dir"

finish() {
  result_code=$?
  trap - EXIT
  printf 'exit_code=%s\n' "$result_code" > "$evidence_dir/result.txt"
  if [[ "$result_code" -ne 0 ]]; then
    git -C "$root_dir" status --short > "$evidence_dir/failure-git-status.txt" || true
  fi
  exit "$result_code"
}
trap finish EXIT
trap 'printf "verification failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2' ERR

case "$expected_os/$expected_arch" in
  linux/amd64) runner_os=Linux; runner_arch=X64; machine=x86_64; nix_platform=x86_64-linux ;;
  linux/arm64) runner_os=Linux; runner_arch=ARM64; machine=aarch64; nix_platform=aarch64-linux ;;
  darwin/amd64) runner_os=macOS; runner_arch=X64; machine=x86_64; nix_platform=x86_64-darwin ;;
  darwin/arm64) runner_os=macOS; runner_arch=ARM64; machine=arm64; nix_platform=aarch64-darwin ;;
  *) printf 'unsupported platform: %s/%s\n' "$expected_os" "$expected_arch" >&2; exit 1 ;;
esac

[[ "$candidate_sha" =~ ^[0-9a-f]{40}$ ]]
[[ "$(git -C "$root_dir" rev-parse HEAD)" == "$candidate_sha" ]]
[[ -z "$(git -C "$root_dir" status --porcelain --untracked-files=all)" ]]
[[ "${RUNNER_OS:-}" == "$runner_os" && "${RUNNER_ARCH:-}" == "$runner_arch" ]]
[[ "$(uname -m)" == "$machine" ]]
if [[ "$expected_os" == linux ]]; then
  [[ "$(uname -s)" == Linux ]]
else
  [[ "$(uname -s)" == Darwin ]]
  translated="$(sysctl -n sysctl.proc_translated 2>/dev/null || true)"
  hardware_arm="$(sysctl -n hw.optional.arm64 2>/dev/null || true)"
  [[ -z "$translated" || "$translated" == 0 ]]
  if [[ "$expected_arch" == arm64 ]]; then
    [[ "$hardware_arm" == 1 ]]
  else
    [[ "$hardware_arm" != 1 ]]
  fi
  printf 'translated=%s\nhardware_arm=%s\n' "$translated" "$hardware_arm" >> "$evidence_dir/runner.txt"
  sw_vers >> "$evidence_dir/runner.txt"
fi

scratch_dir="$(mktemp -d /tmp/pig-platform.XXXXXXXX)"
printf 'scratch_dir=%s\n' "$scratch_dir" >> "$evidence_dir/runner.txt"
if [[ -n "${PIG_PLATFORM_CACHE_DIR:-}" ]]; then
  build_dir="$(mktemp -d "$PIG_PLATFORM_CACHE_DIR/pig-platform.XXXXXXXX")"
else
  build_dir="$scratch_dir"
fi
printf 'build_dir=%s\n' "$build_dir" >> "$evidence_dir/runner.txt"
mkdir -p "$scratch_dir/tmp" "$build_dir/go-build" "$build_dir/go-mod" "$build_dir/go-tmp" "$scratch_dir/pig-home/agent"
export TMPDIR="$scratch_dir/tmp" GOCACHE="$build_dir/go-build" GOMODCACHE="$build_dir/go-mod" GOTMPDIR="$build_dir/go-tmp"
export GOPATH="$build_dir/go" GOTOOLCHAIN=local CGO_ENABLED=1 GOENV=off
export PIG_HOME="$scratch_dir/pig-home" PIG_CODING_AGENT_DIR="$scratch_dir/pig-home/agent"
export PIG_OFFLINE=1 PI_OFFLINE=1 PIG_USE_PI_DIRS=0
unset GOOS GOARCH GOFLAGS GOWORK

go version | tee "$evidence_dir/go-version.txt"
go env -json GOVERSION GOHOSTOS GOHOSTARCH GOOS GOARCH CGO_ENABLED GOROOT > "$evidence_dir/go-env.json"
[[ "$(go env GOVERSION)" == go1.27.4 ]]
[[ "$(go env GOHOSTOS)" == "$expected_os" && "$(go env GOOS)" == "$expected_os" ]]
[[ "$(go env GOHOSTARCH)" == "$expected_arch" && "$(go env GOARCH)" == "$expected_arch" ]]
shasum -a 256 "$(command -v go)" > "$evidence_dir/go-binary.sha256"

pig_module="$(awk '$1 == "github.com/MichaelKinsy/PiG" { print $2 }' "$root_dir/scripts/go.mod")"
sdk_module="$(awk '$1 == "github.com/MichaelKinsy/PiG/extensions/sdk" { print $2 }' "$root_dir/scripts/go.mod")"
[[ "$pig_module" == v0.7.0 && "$sdk_module" == "$pig_module" ]]
pig_version="${pig_module#v}"
archive="pig-$pig_version-$expected_os-$expected_arch.tar.gz"

if [[ -f "$root_dir/pig-source.json" ]]; then
  source_input="$root_dir/pig-source.json"
  jq --exit-status --arg version "$pig_version" '
    .kind == "patched-source-build" and .version == $version
    and (.archive.sha256 | test("^[0-9a-f]{64}$"))
    and (.patch.sha256 | test("^[0-9a-f]{64}$"))
  ' "$source_input" >/dev/null
  cp "$source_input" "$evidence_dir/pig-source.json"
  printf 'patched-source-build\n' > "$evidence_dir/evidence-kind.txt"
  source_url="$(jq -er '.archive.url' "$source_input")"
  source_sha="$(jq -er '.archive.sha256' "$source_input")"
  source_root="$(jq -er '.archive.root' "$source_input")"
  patch_path="$(jq -er '.patch.path' "$source_input")"
  patch_sha="$(jq -er '.patch.sha256' "$source_input")"
  [[ "$source_root" == "pig-$pig_version" ]]
  [[ "$patch_path" == patches/* && "$patch_path" != *..* ]]
  actual_patch_sha="$(shasum -a 256 "$root_dir/$patch_path" | awk '{ print $1 }')"
  [[ "$actual_patch_sha" == "$patch_sha" ]]
  source_archive="$build_dir/pig-source.tar.gz"
  curl --fail --silent --show-error --location --retry 3 "$source_url" -o "$source_archive"
  actual_source_sha="$(shasum -a 256 "$source_archive" | awk '{ print $1 }')"
  printf 'source_url=%s\nsource_sha256=%s\nactual_source_sha256=%s\npatch=%s\npatch_sha256=%s\nactual_patch_sha256=%s\n' "$source_url" "$source_sha" "$actual_source_sha" "$patch_path" "$patch_sha" "$actual_patch_sha" | tee "$evidence_dir/source-checksums.txt"
  [[ "$actual_source_sha" == "$source_sha" ]]
  tar -tzf "$source_archive" > "$evidence_dir/archive-contents.txt"
  tar -xzf "$source_archive" -C "$build_dir"
  source_dir="$build_dir/$source_root"
  git -C "$source_dir" apply --check "$root_dir/$patch_path"
  git -C "$source_dir" apply "$root_dir/$patch_path"
  go -C "$source_dir" env -json GOWORK > "$evidence_dir/source-workspace.json"
  [[ "$(go -C "$source_dir" env GOWORK)" == "$source_dir/go.work" ]]
  pig_build="pig-litter-patch-$patch_sha"
  mkdir -p "$build_dir/bin"
  printf 'CGO_ENABLED=0 go -C %q build -trimpath -buildvcs=false -mod=readonly -ldflags %q -o %q ./cmd/pig\n' "$source_dir" "-s -w -X github.com/MichaelKinsy/PiG/coding/cli.Build=$pig_build" "$build_dir/bin/pig" > "$evidence_dir/pig-build-command.txt"
  CGO_ENABLED=0 go -C "$source_dir" build -trimpath -buildvcs=false -mod=readonly -ldflags "-s -w -X github.com/MichaelKinsy/PiG/coding/cli.Build=$pig_build" -o "$build_dir/bin/pig" ./cmd/pig
  export PATH="$build_dir/bin:$PATH"
  export PIG_LITTER_EXPECTED_PIG="$build_dir/bin/pig"
  export PIG_LITTER_EXPECTED_PIG_SHA256="$(shasum -a 256 "$PIG_LITTER_EXPECTED_PIG" | awk '{ print $1 }')"
  export PIG_LITTER_EXPECTED_PIG_BUILD="$pig_build"
else
printf 'stock-release-archive\n' > "$evidence_dir/evidence-kind.txt"
nix_value() {
  awk -F '"' -v platform="$nix_platform" -v field="$1" '
    $2 == platform && $0 ~ /= *[{]/ { inside = 1; next }
    inside && /^[[:space:]]*};/ { exit }
    inside && $1 ~ ("^[[:space:]]*" field "[[:space:]]*=") { print $2; found++ }
    END { if (found != 1) exit 1 }
  ' "$root_dir/devenv.nix"
}

pinned_archive="$(nix_value archive)"
pinned_root="$(nix_value root)"
pinned_sri="$(nix_value hash)"
[[ "$pinned_archive" == "$archive" && "$pinned_root" == "${archive%.tar.gz}" ]]
release_url="https://github.com/MichaelKinsy/PiG/releases/download/$pig_module"
curl --fail --silent --show-error --location --retry 3 "$release_url/SHA256SUMS" -o "$evidence_dir/SHA256SUMS"
curl --fail --silent --show-error --location --retry 3 "$release_url/$archive" -o "$scratch_dir/$archive"
official_sha="$(awk -v name="$archive" '$2 == name { print tolower($1) }' "$evidence_dir/SHA256SUMS")"
actual_sha="$(shasum -a 256 "$scratch_dir/$archive" | awk '{ print $1 }')"
actual_sri="$(perl -MMIME::Base64 -e 'print "sha256-", encode_base64(pack("H*", $ARGV[0]), "")' "$actual_sha")"
printf 'archive=%s\nofficial_sha256=%s\nactual_sha256=%s\npinned_sri=%s\nactual_sri=%s\n' "$archive" "$official_sha" "$actual_sha" "$pinned_sri" "$actual_sri" | tee "$evidence_dir/archive-checksums.txt"
[[ "$official_sha" =~ ^[0-9a-f]{64}$ && "$actual_sha" == "$official_sha" && "$actual_sri" == "$pinned_sri" ]]

tar -tzf "$scratch_dir/$archive" > "$evidence_dir/archive-contents.txt"
grep -Fx "$pinned_root/pig" "$evidence_dir/archive-contents.txt" >/dev/null
mkdir -p "$scratch_dir/extract" "$scratch_dir/verified-pig-$pig_version/bin"
tar -xzf "$scratch_dir/$archive" -C "$scratch_dir/extract" "$pinned_root/pig"
install -m 755 "$scratch_dir/extract/$pinned_root/pig" "$scratch_dir/verified-pig-$pig_version/bin/pig"
export PATH="$scratch_dir/verified-pig-$pig_version/bin:$PATH"
unset PIG_LITTER_EXPECTED_PIG PIG_LITTER_EXPECTED_PIG_SHA256 PIG_LITTER_EXPECTED_PIG_BUILD
fi
pig_file="$(file -b "$(command -v pig)")"
case "$expected_os/$expected_arch" in
  linux/amd64) [[ "$pig_file" == *ELF* && "$pig_file" == *x86-64* ]] ;;
  linux/arm64) [[ "$pig_file" == *ELF* && "$pig_file" == *aarch64* ]] ;;
  darwin/amd64) [[ "$pig_file" == *Mach-O* && "$pig_file" == *x86_64* ]] ;;
  darwin/arm64) [[ "$pig_file" == *Mach-O* && "$pig_file" == *arm64* ]] ;;
esac
printf '%s\n' "$pig_file" > "$evidence_dir/pig-binary.txt"
shasum -a 256 "$(command -v pig)" >> "$evidence_dir/pig-binary.txt"
pig --version | tee -a "$evidence_dir/pig-binary.txt"

go -C "$root_dir/extensions/pig-litter" mod download
go -C "$root_dir/scripts" mod download
export GOPROXY=off
resolved_pig="$(go -C "$root_dir/scripts" list -m -f '{{.Version}}' github.com/MichaelKinsy/PiG)"
resolved_sdk="$(go -C "$root_dir/scripts" list -m -f '{{.Version}}' github.com/MichaelKinsy/PiG/extensions/sdk)"
printf 'pig=%s\nsdk=%s\n' "$resolved_pig" "$resolved_sdk" > "$evidence_dir/modules.txt"
[[ "$resolved_pig" == "$pig_module" && "$resolved_sdk" == "$sdk_module" ]]

bash "$root_dir/scripts/check-pig-extension.sh" --registration-only --evidence-dir "$evidence_dir" 2>&1 | tee "$evidence_dir/check-pig-extension.log"

cat > "$scratch_dir/observer.go" <<'GO'
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func main() {
	ext := sdk.New("pig-platform-observer")
	ext.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		tools, err := ctx.GetAllTools()
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(tools)
		if err != nil {
			return nil, err
		}
		return nil, os.WriteFile(os.Getenv("PIG_PLATFORM_TOOLS"), data, 0o600)
	})
	if err := ext.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GO
go -C "$root_dir/scripts" build -mod=readonly -o "$scratch_dir/pig-platform-observer" "$scratch_dir/observer.go"
export PIG_PLATFORM_TOOLS="$evidence_dir/piglet-tools.json"
printf '%s\n' '{"id":"commands","type":"get_commands"}' '{"id":"extensions","type":"get_extensions"}' > "$evidence_dir/piglet-rpc-input.jsonl"
pig --piglet "$root_dir/piglet.yaml" -e "$scratch_dir/pig-platform-observer" --mode rpc --no-session < "$evidence_dir/piglet-rpc-input.jsonl" > "$evidence_dir/piglet-rpc.jsonl" 2> "$evidence_dir/piglet-rpc.stderr"
jq --exit-status '[.[] | .name | select(startswith("litter_"))] | sort == ["litter_inspect", "litter_list", "litter_message", "litter_spawn", "litter_stop", "litter_wait"]' "$evidence_dir/piglet-tools.json" >/dev/null
jq --slurp --exit-status 'any(.[]; .id == "commands" and .success == true and any(.data.commands[]?; .name == "pig-litter" and .source == "extension")) and any(.[]; .id == "extensions" and .success == true and any(.data.paths[]?; endswith("/extensions/pig-litter")))' "$evidence_dir/piglet-rpc.jsonl" >/dev/null

[[ "$(git -C "$root_dir" rev-parse HEAD)" == "$candidate_sha" ]]
[[ -z "$(git -C "$root_dir" status --porcelain --untracked-files=all)" ]]
printf 'verified candidate %s on %s/%s\n' "$candidate_sha" "$expected_os" "$expected_arch"
