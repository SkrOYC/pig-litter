import { expect, test } from "bun:test";
import {
  chmodSync,
  existsSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

test("one supervised tick targets the selected thread and reports queue rejection", async () => {
  const directory = mkdtempSync(join(tmpdir(), "pstack-tick-"));
  const record = join(directory, "queued.json");
  const executable = join(directory, "codex");
  writeFileSync(
    executable,
    "#!/usr/bin/env bun\nawait Bun.write(process.env.PSTACK_TEST_RECORD, JSON.stringify(process.argv.slice(2))); process.exit(Number(process.env.PSTACK_TEST_EXIT ?? 0));\n",
  );
  chmodSync(executable, 0o755);
  const environment = {
    ...process.env,
    PATH: `${directory}:${process.env.PATH}`,
    PSTACK_TEST_RECORD: record,
  };
  try {
    const args = [
      process.execPath,
      resolve(".agents/scripts/pstack-tick.mjs"),
      "--thread",
      "thread-a",
      "--minutes",
      "0",
      "--message",
      "check the armed workflow",
    ];
    const accepted = Bun.spawn(args, {
      env: environment,
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(await accepted.exited).toBe(0);
    expect(JSON.parse(readFileSync(record, "utf8"))).toEqual([
      "queue",
      "--thread",
      "thread-a",
      "--message",
      "check the armed workflow",
    ]);
    expect(await new Response(accepted.stdout).text()).toContain(
      '"status":"queued"',
    );
    const rejected = Bun.spawn(args, {
      env: { ...environment, PSTACK_TEST_EXIT: "7" },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(await rejected.exited).toBe(1);
    expect(await new Response(rejected.stderr).text()).toContain(
      "not accepted",
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("interrupting an armed timer prevents queueing", async () => {
  const directory = mkdtempSync(join(tmpdir(), "pstack-tick-cancel-"));
  const record = join(directory, "unexpected.json");
  writeFileSync(
    join(directory, "codex"),
    "#!/usr/bin/env bun\nawait Bun.write(process.env.PSTACK_TEST_RECORD, 'unexpected');\n",
  );
  chmodSync(join(directory, "codex"), 0o755);
  try {
    const child = Bun.spawn(
      [
        process.execPath,
        resolve(".agents/scripts/pstack-tick.mjs"),
        "--thread",
        "thread-a",
        "--minutes",
        "30",
      ],
      {
        env: {
          ...process.env,
          PATH: `${directory}:${process.env.PATH}`,
          PSTACK_TEST_RECORD: record,
        },
        stdout: "pipe",
        stderr: "pipe",
      },
    );
    const reader = child.stdout.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toContain(
      '"status":"armed"',
    );
    child.kill("SIGTERM");
    await child.exited;
    reader.releaseLock();
    expect(existsSync(record)).toBe(false);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
