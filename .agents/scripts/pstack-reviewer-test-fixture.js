import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

export function reviewerFixture(scenario = "normal") {
  const directory = mkdtempSync(join(tmpdir(), "pstack-reviewer-"));
  const cwd = join(directory, "workspace");
  const home = join(directory, "native");
  mkdirSync(cwd);
  mkdirSync(home);
  const executable = join(directory, "codex");
  writeFileSync(
    executable,
    `#!/bin/sh\nexec '${process.execPath}' '${resolve(".agents/scripts/fixtures/native-reviewer.mjs")}' "$@"\n`,
  );
  chmodSync(executable, 0o700);
  const prompt = join(directory, "prompt.md");
  const output = join(directory, "report.md");
  const record = join(directory, "invocations.jsonl");
  writeFileSync(prompt, "Review sample.js, report only.");
  writeFileSync(record, "");
  const env = {
    ...process.env,
    CODEX_HOME: home,
    PATH: `${directory}:${process.env.PATH}`,
    PSTACK_FIXTURE_SCENARIO: scenario,
    PSTACK_FIXTURE_RECORD: record,
  };
  const args = [
    process.execPath,
    resolve(".agents/scripts/pstack-readonly.mjs"),
    "--cwd",
    cwd,
    "--prompt",
    prompt,
    "--output",
    output,
    "--model",
    "gpt-6-luna",
    "--effort",
    "high",
  ];
  return {
    directory,
    cwd,
    home,
    output,
    prompt,
    record,
    env,
    args,
    launch(extra = []) {
      return Bun.spawn([...args, ...extra], {
        env,
        stdout: "pipe",
        stderr: "pipe",
      });
    },
    async result(child) {
      const [code, stdout, stderr] = await Promise.all([
        child.exited,
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
      ]);
      return { code, stdout, stderr };
    },
    calls() {
      return readFileSync(record, "utf8")
        .trim()
        .split("\n")
        .filter(Boolean)
        .map(JSON.parse);
    },
    receipt() {
      return JSON.parse(readFileSync(`${output}.receipt.json`, "utf8"));
    },
    close() {
      rmSync(directory, { recursive: true, force: true });
    },
  };
}
