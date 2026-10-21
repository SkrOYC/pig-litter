import { expect, test } from "bun:test";
import { readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { reviewerFixture } from "./pstack-reviewer-test-fixture.js";

function restricted(f, value) {
  const path = join(f.directory, "restrictions.json");
  writeFileSync(path, JSON.stringify(value));
  return ["--restrictions", path];
}

function diagnosticReceipt(f) {
  const directory = readdirSync(f.directory).find((name) =>
    name.startsWith(".pstack-review-"),
  );
  return JSON.parse(
    readFileSync(join(f.directory, directory, "receipt.json"), "utf8"),
  );
}

test("an old report is never returned by a launcher that writes nothing", async () => {
  const f = reviewerFixture("stale");
  try {
    writeFileSync(f.output, "STALE_REPORT_SENTINEL");
    const result = await f.result(f.launch());
    expect(result.code).toBe(1);
    expect(result.stdout).toBe("");
    expect(result.stderr).toContain("report");
  } finally {
    f.close();
  }
});

test("fresh output must equal the completed canonical final message", async () => {
  const f = reviewerFixture("mismatch");
  try {
    const result = await f.result(f.launch());
    expect(result.code).toBe(1);
    expect(result.stderr).toContain("canonical");
    expect(result.stdout).toBe("");
  } finally {
    f.close();
  }
});

test("a failed command-only preflight prevents every panel reviewer", async () => {
  const f = reviewerFixture("preflight-failure");
  try {
    const probe = join(f.directory, "probe.mjs");
    writeFileSync(probe, 'process.stdout.write("ready");');
    const manifest = join(f.directory, "panel.json");
    writeFileSync(
      manifest,
      JSON.stringify({
        cwd: f.cwd,
        preflight: probe,
        jobs: [
          {
            prompt: f.prompt,
            output: f.output,
            model: "gpt-6-luna",
            effort: "high",
          },
          {
            prompt: f.prompt,
            output: join(f.directory, "other.md"),
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
    expect(result.code).toBe(1);
    expect(result.stderr).toContain("Go is unavailable");
    expect(f.calls().map((c) => c.method)).toEqual([
      "initialize",
      "initialized",
      "command/exec",
    ]);
  } finally {
    f.close();
  }
});

test("a generic non-Go reviewer publishes only its exact canonical report", async () => {
  const f = reviewerFixture();
  try {
    const result = await f.result(f.launch());
    expect(result).toEqual({ code: 0, stdout: "one finding", stderr: "" });
    const receipt = f.receipt();
    expect(receipt.status).toBe("verified");
    expect(receipt.checks.finalMessage.status).toBe("verified");
    expect(receipt.effectiveModels.map((c) => c.model)).toEqual(["gpt-6-luna"]);
    expect(receipt.nativeTools.map((t) => t.name)).toEqual(["exec_command"]);
    expect(receipt.preparation.status).toBe("not-requested");
    expect(JSON.stringify(receipt)).not.toContain("PRIVATE_ARGUMENT");
    expect(JSON.stringify(receipt)).not.toContain("PRIVATE_OUTPUT");
    expect(JSON.stringify(receipt)).not.toContain("Review sample.js");
  } finally {
    f.close();
  }
});

for (const scenario of ["hidden-child", "unindexed-child", "large-inventory"]) {
  test(`canonical task lineage catches ${scenario}`, async () => {
    const f = reviewerFixture(scenario);
    try {
      const result = await f.result(
        f.launch(restricted(f, { taskDelegation: "forbid" })),
      );
      expect(result.code).toBe(1);
      const receipt = diagnosticReceipt(f);
      expect(receipt.checks.taskDelegation.status).toBe("violated");
      expect(receipt.boundary.threads.length).toBe(2);
      expect(f.calls().filter((c) => c.method === "review").length).toBe(1);
    } finally {
      f.close();
    }
  });
}

test("guardians are excluded from task restrictions", async () => {
  const f = reviewerFixture("guardian");
  try {
    const result = await f.result(
      f.launch(
        restricted(f, {
          taskDelegation: "forbid",
          forbiddenModels: ["forbidden-model"],
        }),
      ),
    );
    expect(result.code).toBe(0);
    expect(f.receipt().checks.taskDelegation.status).toBe("verified");
    expect(f.receipt().coverage.guardianThreadsExcluded).toBe(1);
  } finally {
    f.close();
  }
});

test("tool restrictions check native names without claiming subprocess behavior", async () => {
  const f = reviewerFixture();
  try {
    expect(
      (await f.result(f.launch(restricted(f, { allowedTools: ["read_file"] }))))
        .code,
    ).toBe(1);
    expect(diagnosticReceipt(f).checks.allowedTools.status).toBe("violated");
    expect(
      (
        await f.result(
          f.launch(restricted(f, { allowedTools: ["exec_command"] })),
        )
      ).code,
    ).toBe(0);
    expect(f.receipt().checks.allowedTools.status).toBe("verified");
  } finally {
    f.close();
  }
});

test("effective model checks inspect every turn context", async () => {
  const f = reviewerFixture("changed-model");
  try {
    expect(
      (
        await f.result(
          f.launch(restricted(f, { forbiddenModels: ["forbidden-model"] })),
        )
      ).code,
    ).toBe(1);
    const receipt = diagnosticReceipt(f);
    expect(receipt.checks.requestedModel.status).toBe("violated");
    expect(receipt.checks.forbiddenModels.status).toBe("violated");
  } finally {
    f.close();
  }
});

for (const scenario of [
  "missing-context",
  "missing-history",
  "malformed",
  "incomplete",
]) {
  test(`${scenario} history remains inconclusive`, async () => {
    const f = reviewerFixture(scenario);
    try {
      const result = await f.result(f.launch());
      expect(result.code).toBe(1);
      expect(result.stderr).toContain("inconclusive");
      expect(diagnosticReceipt(f).status).toBe("inconclusive");
    } finally {
      f.close();
    }
  });
}

test("tied native ordinals retain all model and tool evidence", async () => {
  const f = reviewerFixture("tied-ordinals");
  try {
    expect(
      (
        await f.result(
          f.launch(restricted(f, { allowedTools: ["exec_command"] })),
        )
      ).code,
    ).toBe(0);
    expect(f.receipt().nativeTools.map((t) => t.name)).toEqual([
      "exec_command",
    ]);
  } finally {
    f.close();
  }
});

test("concurrent publishers cannot overwrite the same report", async () => {
  const f = reviewerFixture("hold");
  try {
    const first = f.launch();
    while (f.calls().length === 0) await Bun.sleep(5);
    const second = await f.result(f.launch());
    expect(second.code).toBe(1);
    expect(second.stderr).toContain("reserved");
    expect((await f.result(first)).code).toBe(0);
    expect(f.calls().filter((c) => c.method === "review").length).toBe(1);
    expect(readFileSync(f.output, "utf8")).toBe("one finding");
  } finally {
    f.close();
  }
});

test("unknown restriction keys fail before starting a reviewer", async () => {
  const f = reviewerFixture();
  try {
    const result = await f.result(
      f.launch(restricted(f, { allowTool: ["exec_command"] })),
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain("Unknown receipt restriction: allowTool");
    expect(f.calls()).toEqual([]);
  } finally {
    f.close();
  }
});

test("late canonical persistence is retried before accepting the report", async () => {
  const f = reviewerFixture("late-history");
  try {
    const result = await f.result(f.launch());
    expect(result).toEqual({ code: 0, stdout: "one finding", stderr: "" });
    expect(f.receipt().checks.completion.status).toBe("verified");
  } finally {
    f.close();
  }
});

for (const scenario of ["corrupt-source", "unknown-lineage"]) {
  test(`${scenario} prevents a claimed complete task census`, async () => {
    const f = reviewerFixture(scenario);
    try {
      expect(
        (await f.result(f.launch(restricted(f, { taskDelegation: "forbid" }))))
          .code,
      ).toBe(1);
      const receipt = diagnosticReceipt(f);
      expect(receipt.checks.taskDelegation.status).toBe("inconclusive");
      expect(receipt.coverage.issues.length).toBeGreaterThan(0);
    } finally {
      f.close();
    }
  });
}

test("complete history retains tied records and includes canonical contexts", async () => {
  const f = reviewerFixture("tied-ordinals");
  try {
    expect((await f.result(f.launch())).code).toBe(0);
    const child = Bun.spawn(
      [
        process.execPath,
        f.args[1].replace("pstack-readonly", "pstack-history"),
        "read",
        "--cwd",
        f.cwd,
        "--id",
        f.receipt().threadId,
        "--complete",
      ],
      { env: f.env, stdout: "pipe", stderr: "pipe" },
    );
    const result = await f.result(child);
    expect(result.code).toBe(0);
    const history = JSON.parse(result.stdout);
    expect(history.status).toBe("verified");
    expect(history.items.map((item) => item.item_type)).toEqual([
      "session_meta",
      "event_msg",
      "turn_context",
      "response_item",
      "response_item",
      "response_item",
      "event_msg",
    ]);
  } finally {
    f.close();
  }
});

test("native Unix-second task boundaries match a completed reviewer", async () => {
  const f = reviewerFixture("numeric-time");
  try {
    expect((await f.result(f.launch())).code).toBe(0);
    const receipt = f.receipt();
    expect(receipt.checks.completion.status).toBe("verified");
    expect(receipt.boundary.threads[0].turns[0].startedAt).toMatch(
      /^\d{4}-\d\d-\d\dT/,
    );
  } finally {
    f.close();
  }
});

test("a native spawn reference without its child history is inconclusive", async () => {
  const f = reviewerFixture("spawn-reference");
  try {
    expect(
      (
        await f.result(
          f.launch(restricted(f, { forbiddenModels: ["forbidden-model"] })),
        )
      ).code,
    ).toBe(1);
    const receipt = diagnosticReceipt(f);
    expect(receipt.checks.forbiddenModels.status).toBe("inconclusive");
    expect(receipt.nativeTools.map((tool) => tool.name)).toEqual([
      "exec_command",
      "spawn_agent",
    ]);
  } finally {
    f.close();
  }
});

test("a forbidden requested model is rejected before any model launch", async () => {
  const f = reviewerFixture();
  try {
    const result = await f.result(
      f.launch(restricted(f, { forbiddenModels: ["gpt-6-luna"] })),
    );
    expect(result.code).toBe(1);
    expect(result.stderr).toContain("requested reviewer model is forbidden");
    expect(f.calls()).toEqual([]);
  } finally {
    f.close();
  }
});

test("publication uses the bytes checked against canonical history", async () => {
  const f = reviewerFixture("late-report-substitution");
  try {
    const result = await f.result(f.launch());
    expect(result).toEqual({ code: 0, stdout: "one finding", stderr: "" });
    expect(readFileSync(f.output, "utf8")).toBe("one finding");
    expect(f.receipt().checks.finalMessage.status).toBe("verified");
    const stage = readdirSync(f.directory).find((name) =>
      name.startsWith(".pstack-review-"),
    );
    expect(readFileSync(join(f.directory, stage, "report.md"), "utf8")).toBe(
      "SUBSTITUTED_AFTER_READ",
    );
  } finally {
    f.close();
  }
});

test("default unrestricted receipts still require complete native history", async () => {
  const f = reviewerFixture("corrupt-source");
  try {
    const result = await f.result(f.launch());
    expect(result.code).toBe(1);
    const receipt = diagnosticReceipt(f);
    expect(receipt.checks.finalMessage.status).toBe("verified");
    expect(receipt.coverage.taskLineage.status).toBe("inconclusive");
    expect(receipt.status).toBe("inconclusive");
    expect(result.stdout).toBe("");
  } finally {
    f.close();
  }
});
