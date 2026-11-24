#!/usr/bin/env bash
set -euo pipefail
umask 077

side=${1:?}
checkout=${2:?}
commit=${3:?}
output=${4:?}
evidence=${5:?}
case "$side" in parent|head) ;; *) printf 'Unknown side: %s\n' "$side" >&2; exit 2 ;; esac
packages=$(dirname "$0")/packages.json
expected_host=$(jq -er --arg side "$side" '.[$side].host' "$packages")
expected_patch=$(jq -er --arg side "$side" '.[$side].patch' "$packages")
expected_source=$(jq -er --arg side "$side" '.[$side].source' "$packages")
expected_source_hash=$(jq -er --arg side "$side" '.[$side].sourceHash' "$packages")
expected_sdk_hash=$(jq -er --arg side "$side" '.[$side].sdkHash' "$packages")
mkdir -p "$output" "$evidence"
[[ "$(git -C "$checkout" rev-parse HEAD)" == "$commit" ]]
[[ "$(jq -r '.patch.commit' "$checkout/pig-source.json")" == "$expected_patch" ]]
patch=$(jq -r '.patch.path' "$checkout/pig-source.json")
[[ "$(sha256sum "$checkout/$patch" | cut -d ' ' -f1)" == "$(jq -r '.patch.sha256' "$checkout/pig-source.json")" ]]
[[ "${PIG_LITTER_EXPECTED_PIG_SHA256:?}" == "$expected_host" ]]
[[ "$(sha256sum "${PIG_LITTER_EXPECTED_PIG:?}" | cut -d ' ' -f1)" == "$expected_host" ]]
[[ "$(cat "${PIG_LITTER_EXPECTED_PIG_SHA256_FILE:?}")" == "$expected_host" ]]
cmp "$checkout/pig-source.json" "$(dirname "$(dirname "$PIG_LITTER_EXPECTED_PIG")")/share/pig/source.json"
[[ "${PIG_LITTER_SOURCE_DIR:?}" == "$expected_source" ]]
[[ "${PIG_LITTER_SDK_DIR:?}" == "$PIG_LITTER_SOURCE_DIR/extensions/sdk" ]]
[[ "$(go env GOWORK)" == "${GOWORK:?}" ]]
[[ "$(go version)" == 'go version go1.27.4 linux/amd64' ]]
[[ -z "${GOFLAGS:-}" ]]

go version > "$evidence/go-version.txt"
go env -json > "$evidence/go-env.json"
cp "$checkout/pig-source.json" "$evidence/pig-source.json"
cp "$GOWORK" "$evidence/go.work"
go -C "$checkout/scripts" list -m -json all > "$evidence/modules.json"
jq -e -s --arg source "$PIG_LITTER_SOURCE_DIR" --arg sdk "$PIG_LITTER_SDK_DIR" 'any(.[]; .Path == "github.com/MichaelKinsy/PiG" and .Main == true and .Dir == $source) and any(.[]; .Path == "github.com/MichaelKinsy/PiG/extensions/sdk" and .Main == true and .Dir == $sdk)' "$evidence/modules.json" > /dev/null
nix path-info --json --json-format 1 "$PIG_LITTER_SOURCE_DIR" "$(dirname "$(dirname "$PIG_LITTER_EXPECTED_PIG")")" > "$evidence/nix-path-info.json"
nix hash path "$PIG_LITTER_SOURCE_DIR" > "$evidence/source.narhash"
nix hash path "$PIG_LITTER_SDK_DIR" > "$evidence/sdk.narhash"
[[ "$(cat "$evidence/source.narhash")" == "$expected_source_hash" ]]
[[ "$(cat "$evidence/sdk.narhash")" == "$expected_sdk_hash" ]]
go -C "$checkout/scripts" build -p 2 -mod=readonly -trimpath -buildvcs=false -o "$output/pig-litter" ./cmd/pig-litter
go version -m "$output/pig-litter" > "$evidence/product-go-metadata.txt"
go version -m "$PIG_LITTER_EXPECTED_PIG" > "$evidence/host-go-metadata.txt"
sha256sum "$output/pig-litter" "$PIG_LITTER_EXPECTED_PIG" > "$evidence/binaries.sha256"

jq -n \
  --arg commit "$commit" --arg tree "$(git -C "$checkout" rev-parse 'HEAD^{tree}')" \
  --arg checkout "$checkout" --arg product "$output/pig-litter" \
  --arg product_sha "$(sha256sum "$output/pig-litter" | cut -d ' ' -f1)" \
  --arg host "$PIG_LITTER_EXPECTED_PIG" --arg host_sha "$expected_host" \
  --arg source "$PIG_LITTER_SOURCE_DIR" --arg sdk "$PIG_LITTER_SDK_DIR" \
  --arg source_hash "$(cat "$evidence/source.narhash")" --arg sdk_hash "$(cat "$evidence/sdk.narhash")" \
  --arg source_declaration_sha "$(sha256sum "$checkout/pig-source.json" | cut -d ' ' -f1)" \
  --arg workspace "$GOWORK" --arg evidence "$evidence" \
  --arg go_tool "$(command -v go)" --arg go_tool_sha "$(sha256sum "$(command -v go)" | cut -d ' ' -f1)" \
  --argjson go_environment "$(go env -json GOOS GOARCH CGO_ENABLED CC)" \
  --slurpfile declaration "$checkout/pig-source.json" \
  '{commit:$commit, tree:$tree, checkout:$checkout, product:{path:$product, sha256:$product_sha, go_version:"go1.27.4", build_flags:["-p=2", "-mod=readonly", "-trimpath", "-buildvcs=false"]}, toolchain:{path:$go_tool, sha256:$go_tool_sha, environment:$go_environment}, host:{path:$host, sha256:$host_sha}, source:{path:$source, nar_hash:$source_hash, declaration_sha256:$source_declaration_sha, declaration:$declaration[0]}, sdk:{path:$sdk, nar_hash:$sdk_hash}, workspace:$workspace, evidence:$evidence}' > "$evidence/manifest.json"
