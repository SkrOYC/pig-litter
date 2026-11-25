#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
directory="$root/extensions/pig-litter/dist"
mkdir -p "$directory"
temporary="$(mktemp "$directory/.pig-litter.XXXXXXXX")"
trap 'rm -f "$temporary"' EXIT
go -C "$root/scripts" build -mod=readonly -tags teammate_native -trimpath -buildvcs=false -o "$temporary" ./cmd/pig-litter
[[ "$("$temporary" --pig-litter-member-check)" == "pig-litter-native-member-v1" ]]
mv "$temporary" "$directory/pig-litter"
