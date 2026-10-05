#!/usr/bin/env bash
set -euo pipefail
root="${DEVENV_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
export PIG_HOME="$scratch/pig-home"
export PIG_CODING_AGENT_DIR="$PIG_HOME/agent"
export PIG_USE_PI_DIRS=0 PIG_OFFLINE=1 PI_OFFLINE=1 GOPROXY=off GOENV=off
export GOCACHE="$scratch/go-cache" GOMODCACHE="$scratch/go-mod-cache"
mkdir -p "$scratch/extension"
cp -R "$root/extensions/pig-litter/." "$scratch/extension/"
sdk_dir="$(pig reload --sdk-path go)"
test -f "$sdk_dir/go.mod"
cd "$scratch/extension"
go mod edit -replace "github.com/MichaelKinsy/PiG/extensions/sdk=$sdk_dir"
go test -race ./...
