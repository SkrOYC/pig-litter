#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
test_root="$(mktemp -d /tmp/pig-litter-doctor-test.XXXXXXXX)"
trap 'rm -rf -- "$test_root"' EXIT
fixture="$test_root/repo"
bin="$test_root/bin"
mkdir -p "$bin" "$fixture/scripts/verify-pig-litter" "$fixture/scripts" "$fixture/extensions/pig-litter"
cp "$script_dir/verify.sh" "$fixture/scripts/verify-pig-litter/verify.sh"
touch "$fixture/extensions/pig-litter/adapter.go" "$fixture/piglet.yaml" "$fixture/scripts/verify-terminal-ui.sh"
ln -s "$(command -v bash)" "$bin/bash"
ln -s "$(command -v dirname)" "$bin/dirname"
ln -s "$(command -v jq)" "$bin/jq"
export REAL_GO="$(command -v go)"
export GOTOOLCHAIN=local GOENV=off GOFLAGS=

cat > "$bin/pig" <<'PIG'
#!/usr/bin/env bash
[[ "$*" == '--version' ]]
printf '%s\n' "$TEST_PIG_VERSION"
PIG
cat > "$bin/tmux" <<'TMUX'
#!/usr/bin/env bash
[[ "$*" == '-V' ]]
printf '%s\n' "$TEST_TMUX_VERSION"
TMUX
cat > "$bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
module_dir=
if [[ "${1:-}" == '-C' ]]; then
  module_dir="$2"
  shift 2
fi
if [[ "$*" == 'env GOVERSION' ]]; then
  if [[ -n "${TEST_EXPECT_GOENV+x}" ]] && [[ "${GOENV-}" != "$TEST_EXPECT_GOENV" || "${GOFLAGS-}" != "$TEST_EXPECT_GOFLAGS" ]]; then
    printf 'Go identity environment changed\n' >&2
    exit 1
  fi
  printf '%s\n' "$TEST_GO_VERSION"
else
  if [[ -n "$module_dir" ]]; then
    set -- "$@" "$module_dir/go.mod"
  fi
  exec "$REAL_GO" "$@"
fi
GO
chmod +x "$bin/pig" "$bin/go" "$bin/tmux"

write_mod() {
  cat > "$fixture/scripts/go.mod" <<MOD
module example.com/fixture

go 1.26.0

toolchain $2

require github.com/MichaelKinsy/PiG v$1
MOD
}

doctor() {
  PATH="$bin" "$fixture/scripts/verify-pig-litter/verify.sh" doctor 2>&1
}

expect_success() {
  local output
  if ! output="$(doctor)"; then
    printf 'expected doctor success, got: %s\n' "$output" >&2
    exit 1
  fi
  for value in "$TEST_PIG_VERSION" "${TEST_GO_VERSION#go}" "$TEST_TMUX_VERSION"; do
    [[ "$output" == *"$value"* ]] || { printf 'doctor omitted %s: %s\n' "$value" "$output" >&2; exit 1; }
  done
  printf 'pass: %s, %s, %s\n' "$TEST_PIG_VERSION" "$TEST_GO_VERSION" "$TEST_TMUX_VERSION"
}

expect_failure() {
  local expected="$1" output
  if output="$(doctor)"; then
    printf 'expected doctor failure, got: %s\n' "$output" >&2
    exit 1
  fi
  [[ "$output" == *"$expected"* ]] || { printf 'expected failure containing %s, got: %s\n' "$expected" "$output" >&2; exit 1; }
  printf 'pass: rejected %s\n' "$output"
}

export TEST_PIG_VERSION='0.4.1+1.0.3' TEST_GO_VERSION='go1.27.1' TEST_TMUX_VERSION='tmux 3.7c'
write_mod '0.4.1' 'go1.27.1'
expect_success

export TEST_PIG_VERSION='0.7.0+1.5.0' TEST_GO_VERSION='go1.27.4' TEST_TMUX_VERSION='tmux 3.9a'
write_mod '0.7.0' 'go1.27.4'
expect_success

export TEST_PIG_VERSION='0.7.01+1.5.0'
expect_failure 'expected PiG 0.7.0'
export TEST_PIG_VERSION='0.7.0+1.5.0' TEST_GO_VERSION='go1.27.1'
expect_failure 'expected Go go1.27.4'
export TEST_GO_VERSION='go1.27.4'
mv "$bin/tmux" "$test_root/tmux"
expect_failure 'missing required command: tmux'
mv "$test_root/tmux" "$bin/tmux"
rm "$fixture/piglet.yaml"
expect_failure 'required file is missing: piglet.yaml'
touch "$fixture/piglet.yaml"
rm "$fixture/scripts/go.mod"
expect_failure 'required file is missing: scripts/go.mod'
cat > "$fixture/scripts/go.mod" <<'MOD'
module example.com/fixture

go 1.26.0

toolchain go1.27.4
MOD
expect_failure 'missing PiG root module requirement in scripts/go.mod'
cat > "$fixture/scripts/go.mod" <<'MOD'
module example.com/fixture

go 1.26.0

require github.com/MichaelKinsy/PiG v0.7.0
MOD
expect_failure 'missing toolchain directive in scripts/go.mod'

set_ambient_flags() {
  local source="$1" flags="$2"
  if [[ "$source" == 'inherited' ]]; then
    export GOENV=off GOFLAGS="$flags"
  else
    printf 'GOFLAGS=%s\n' "$flags" > "$test_root/go.env"
    export GOENV="$test_root/go.env" GOFLAGS=
  fi
  export TEST_EXPECT_GOENV="$GOENV" TEST_EXPECT_GOFLAGS="$GOFLAGS"
}

export TEST_PIG_VERSION='0.4.1+1.0.3' TEST_GO_VERSION='go1.27.1' TEST_TMUX_VERSION='tmux 3.7c'
regression_failures=0
for flag_source in inherited saved; do
  write_mod '0.4.1' 'go1.27.4'
  set_ambient_flags "$flag_source" '-toolchain=go1.27.1'
  (expect_failure 'expected Go go1.27.4') || ((regression_failures += 1))

  cat > "$fixture/scripts/go.mod" <<'MOD'
module example.com/fixture

go 1.26.0

require github.com/MichaelKinsy/PiG v0.4.1
MOD
  (expect_failure 'missing toolchain directive in scripts/go.mod') || ((regression_failures += 1))

  write_mod '0.4.1' 'go1.27.1'
  set_ambient_flags "$flag_source" '-toolchain=go1.27.4'
  (expect_success) || ((regression_failures += 1))

  write_mod '0.7.0' 'go1.27.1'
  set_ambient_flags "$flag_source" '-require=github.com/MichaelKinsy/PiG@v0.4.1'
  (expect_failure 'expected PiG 0.7.0') || ((regression_failures += 1))
done

if ((regression_failures)); then
  printf '%s GOFLAGS regression checks failed\n' "$regression_failures" >&2
  exit 1
fi
