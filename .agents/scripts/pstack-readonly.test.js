import { expect, test } from "bun:test";
import { join } from "node:path";
import { readFileSync, writeFileSync } from "node:fs";
import { reviewerFixture } from "./pstack-reviewer-test-fixture.js";

test("native read-only adapter preserves the role, prompt, model, and report", async () => {
  const f = reviewerFixture();
  try {
    const result = await f.result(f.launch(["--role", "comment-sicko"]));
    expect(result).toEqual({ code: 0, stdout: "one finding", stderr: "" });
    const invocation = f.calls().find((call) => call.method === "review");
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
    expect(invocation.args).toContain("allow_login_shell=false");
    expect(
      invocation.args.find((a) => a.startsWith("developer_instructions=")),
    ).toContain("Yes... Ha ha ha... Yes!");
    f.env.PSTACK_FIXTURE_SCENARIO = "exit-failure";
    const rejected = await f.result(f.launch());
    expect(rejected.code).toBe(1);
    expect(rejected.stdout).toBe("");
    expect(rejected.stderr).toContain("exit 7");
  } finally {
    f.close();
  }
});

test("one command-only preflight finishes before both unchanged reviewer prompts", async () => {
  const f = reviewerFixture();
  try {
    const probe = join(f.directory, "probe.mjs");
    writeFileSync(probe, 'process.stdout.write("ready");');
    const second = join(f.directory, "second.md");
    const manifest = join(f.directory, "panel.json");
    writeFileSync(
      manifest,
      JSON.stringify({
        cwd: f.cwd,
        preflight: probe,
        restrictions: {
          taskDelegation: "forbid",
          allowedTools: ["exec_command"],
        },
        jobs: [
          {
            prompt: f.prompt,
            output: f.output,
            model: "gpt-6-luna",
            effort: "high",
          },
          {
            prompt: f.prompt,
            output: second,
            model: "gpt-6-luna",
            effort: "high",
          },
        ],
      }),
    );
    const child = Bun.spawn(
      [process.execPath, f.args[1], "--panel", manifest],
      { env: f.env, stdout: "pipe", stderr: "pipe" },
    );
    const result = await f.result(child);
    expect(result).toEqual({
      code: 0,
      stdout: "one finding\none finding",
      stderr: "",
    });
    const calls = f.calls();
    expect(calls.map((call) => call.method)).toEqual([
      "initialize",
      "initialized",
      "command/exec",
      "review",
      "review",
    ]);
    expect(calls[2].params.sandboxPolicy).toEqual({
      type: "readOnly",
      networkAccess: false,
    });
    expect(calls[2].params.command).toEqual([process.execPath, probe]);
    expect(calls.slice(3).map((call) => call.prompt)).toEqual([
      "Review sample.js, report only.",
      "Review sample.js, report only.",
    ]);
    const receipts = [
      f.receipt(),
      JSON.parse(readFileSync(`${second}.receipt.json`, "utf8")),
    ];
    expect(receipts.map((receipt) => receipt.preparation.status)).toEqual([
      "verified",
      "verified",
    ]);
    expect(receipts[0].threadId).not.toBe(receipts[1].threadId);
  } finally {
    f.close();
  }
});
