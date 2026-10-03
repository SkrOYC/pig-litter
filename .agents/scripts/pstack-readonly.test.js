import { expect, test } from "bun:test";
import {
  chmodSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

test("native read-only adapter preserves the role, prompt, model, and report", async () => {
  const directory = mkdtempSync(join(tmpdir(), "pstack-readonly-"));
  const prompt = join(directory, "prompt.txt");
  const report = join(directory, "report.txt");
  const record = join(directory, "launch.json");
  writeFileSync(prompt, "Review sample.js, report only.");
  writeFileSync(
    join(directory, "codex"),
    "#!/usr/bin/env bun\nconst args=process.argv.slice(2); const prompt=await Bun.stdin.text(); await Bun.write(process.env.PSTACK_TEST_RECORD,JSON.stringify({args,prompt})); if(process.env.PSTACK_TEST_REJECT)process.exit(7); await Bun.write(args[args.indexOf('--output-last-message')+1],'one finding');\n",
  );
  chmodSync(join(directory, "codex"), 0o755);
  const env = {
    ...process.env,
    PATH: `${directory}:${process.env.PATH}`,
    PSTACK_TEST_RECORD: record,
  };
  const args = [
    process.execPath,
    resolve(".agents/scripts/pstack-readonly.mjs"),
    "--cwd",
    directory,
    "--prompt",
    prompt,
    "--output",
    report,
    "--model",
    "gpt-6-luna",
    "--effort",
    "high",
    "--role",
    "comment-sicko",
  ];
  try {
    const child = Bun.spawn(args, { env, stdout: "pipe", stderr: "pipe" });
    expect(await child.exited).toBe(0);
    expect(await new Response(child.stdout).text()).toBe("one finding");
    const invocation = JSON.parse(readFileSync(record, "utf8"));
    expect(invocation.prompt).toStartWith("Native agent role: comment-sicko\n");
    expect(invocation.prompt).toContain("Yes... Ha ha ha... Yes!");
    expect(invocation.prompt).toEndWith(
      "Parent scope:\nReview sample.js, report only.",
    );
    expect(invocation.args.slice(0, 4)).toEqual([
      "exec",
      "--sandbox",
      "read-only",
      "--json",
    ]);
    expect(invocation.args[invocation.args.indexOf("--model") + 1]).toBe(
      "gpt-6-luna",
    );
    expect(
      invocation.args.find((a) => a.startsWith("developer_instructions=")),
    ).toContain("Yes... Ha ha ha... Yes!");
    const rejected = Bun.spawn(args, {
      env: { ...env, PSTACK_TEST_REJECT: "1" },
      stdout: "pipe",
      stderr: "pipe",
    });
    expect(await rejected.exited).toBe(1);
    expect(await new Response(rejected.stdout).text()).toBe("");
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
