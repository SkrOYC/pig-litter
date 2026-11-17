import { expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";

const probe = fileURLToPath(new URL("./pstack-toolchain-probe.mjs", import.meta.url));
const realGo = Bun.which("go");

if (!realGo) throw new Error("Go is required to run the toolchain probe tests");

function runProbe({
  module = "module example.com/probe\n\ngo 1.26.0\n\ntoolchain go1.27.1\n",
  goVersion = "go1.27.1",
  cgo = "1",
  toolchain = "local",
  goEnvFails = false,
  goModFails = false,
  compilerFails = false,
} = {}) {
  const root = mkdtempSync(join(tmpdir(), "pstack-toolchain-test-"));
  const workspace = join(root, "workspace");
  const scripts = join(workspace, "scripts");
  const bin = join(root, "bin");
  const goMod = join(scripts, "go.mod");
  const compiler = join(bin, "fixture-cc");

  try {
    mkdirSync(scripts, { recursive: true });
    mkdirSync(bin);
    mkdirSync(join(root, "home"));
    writeFileSync(goMod, module);
    writeFileSync(
      join(bin, "go"),
      `#!/bin/sh
for argument in "$@"; do
  if [ "$argument" = env ]; then
    if [ "$PSTACK_TEST_GO_ENV_FAILS" = 1 ]; then
      printf '%s\n' 'fixture go env failed' >&2
      exit 1
    fi
    printf '%s\n' "$PSTACK_TEST_GO_ENV"
    exit 0
  fi
done
if [ "$PSTACK_TEST_GO_MOD_FAILS" = 1 ]; then
  printf '%s\n' 'fixture go mod failed' >&2
  exit 1
fi
exec "$PSTACK_TEST_REAL_GO" "$@"
`,
      { mode: 0o755 },
    );
    writeFileSync(
      compiler,
      `#!/bin/sh
if [ "$PSTACK_TEST_COMPILER_FAILS" = 1 ]; then
  printf '%s\n' 'fixture compiler failed' >&2
  exit 1
fi
case " $* " in
  *' -fsyntax-only '*) ;;
  *) printf '%s\n' 'missing syntax-only flag' >&2; exit 1 ;;
esac
case " $* " in
  *' -x c - '*) ;;
  *) printf '%s\n' 'missing C input from stdin' >&2; exit 1 ;;
esac
source=$(cat)
case "$source" in
  *'#include <stdio.h>'*'#include <stdlib.h>'*) exit 0 ;;
  *) printf '%s\n' 'missing C headers in compiler input' >&2; exit 1 ;;
esac
`,
      { mode: 0o755 },
    );

    const result = spawnSync(process.execPath, [probe], {
      cwd: workspace,
      encoding: "utf8",
      timeout: 10_000,
      env: {
        ...process.env,
        PATH: `${bin}${delimiter}${process.env.PATH}`,
        HOME: join(root, "home"),
        GOENV: "off",
        GOWORK: "off",
        GOTOOLCHAIN: "local",
        GOPROXY: "off",
        GOSUMDB: "off",
        GOCACHE: join(root, "go-cache"),
        GOMODCACHE: join(root, "go-mod-cache"),
        PSTACK_TEST_REAL_GO: realGo,
        PSTACK_TEST_GO_ENV: JSON.stringify({
          GOVERSION: goVersion,
          CGO_ENABLED: cgo,
          GOTOOLCHAIN: toolchain,
          CC: compiler,
        }),
        PSTACK_TEST_GO_ENV_FAILS: goEnvFails ? "1" : "0",
        PSTACK_TEST_GO_MOD_FAILS: goModFails ? "1" : "0",
        PSTACK_TEST_COMPILER_FAILS: compilerFails ? "1" : "0",
      },
    });
    if (result.error) throw result.error;
    return {
      status: result.status,
      stdout: result.stdout,
      stderr: result.stderr,
      moduleAfter: readFileSync(goMod, "utf8"),
      workspaceFiles: readdirSync(workspace),
      scriptFiles: readdirSync(scripts),
    };
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

function expectSuccess(result, goVersion) {
  expect(result.status, result.stderr).toBe(0);
  expect(JSON.parse(result.stdout)).toEqual({
    go: goVersion,
    cgo: "1",
    toolchain: "local",
    compiler: "syntax-and-headers-verified",
  });
  expect(result.stderr).toBe("");
}

function expectFailure(result, diagnostic) {
  expect(result.status, result.stdout).toBe(1);
  expect(result.stderr).toMatch(diagnostic);
}

test("accepts the matching baseline without writing project files", () => {
  const result = runProbe();
  expectSuccess(result, "go1.27.1");
  expect(result.moduleAfter).toBe("module example.com/probe\n\ngo 1.26.0\n\ntoolchain go1.27.1\n");
  expect(result.workspaceFiles).toEqual(["scripts"]);
  expect(result.scriptFiles).toEqual(["go.mod"]);
});

test("accepts a matching upgraded toolchain declaration", () => {
  const result = runProbe({
    module: "module example.com/probe\n\ngo 1.26.0\n\ntoolchain go1.27.4\n",
    goVersion: "go1.27.4",
  });
  expectSuccess(result, "go1.27.4");
});

test("accepts a declaration in a module with many dependencies", () => {
  const requirements = Array.from(
    { length: 100 },
    (_, index) => `\texample.com/dependency${index} v1.0.0`,
  ).join("\n");
  const result = runProbe({
    module: `module example.com/probe\n\ngo 1.26.0\n\ntoolchain go1.27.1\n\nrequire (\n${requirements}\n)\n`,
  });
  expectSuccess(result, "go1.27.1");
});

test("rejects a Go version that differs from the declaration", () => {
  const result = runProbe({
    module: "module example.com/probe\n\ngo 1.26.0\n\ntoolchain go1.27.4\n",
  });
  expectFailure(result, /GOVERSION|go1\.27\.4/);
});

test("rejects a missing toolchain declaration", () => {
  const result = runProbe({ module: "module example.com/probe\n\ngo 1.26.0\n" });
  expectFailure(result, /scripts\/go\.mod|directive/i);
});

test("rejects an invalid toolchain declaration", () => {
  const result = runProbe({
    module: "module example.com/probe\n\ngo 1.26.0\n\ntoolchain invalid\n",
  });
  expectFailure(result, /invalid|go\.mod/i);
});

test("rejects a failed Go environment command", () => {
  expectFailure(runProbe({ goEnvFails: true }), /fixture go env failed/);
});

test("rejects a failed Go module command", () => {
  expectFailure(runProbe({ goModFails: true }), /fixture go mod failed/);
});

test("rejects disabled CGO", () => {
  expectFailure(runProbe({ cgo: "0" }), /CGO_ENABLED/);
});

test("rejects automatic Go toolchain selection", () => {
  expectFailure(runProbe({ toolchain: "auto" }), /GOTOOLCHAIN/);
});

test("rejects an unusable configured C compiler", () => {
  expectFailure(runProbe({ compilerFails: true }), /fixture compiler failed/);
});
