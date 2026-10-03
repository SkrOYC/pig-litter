import { afterEach, describe, expect, test } from "bun:test";
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { TOML } from "bun";
import { Database } from "bun:sqlite";
import {
  BUDGETS,
  effortForBudget,
  readModelCatalog,
  readActiveParent,
  resolveRole,
  show,
  writeConfiguration,
} from "./pstack-models.mjs";

const roots = [];

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), "pstack-models-"));
  roots.push(root);
  const cwd = join(root, "project");
  const codexHome = join(root, "codex");
  await mkdir(cwd, { recursive: true });
  await mkdir(codexHome, { recursive: true });
  await writeFile(
    join(codexHome, "models_cache.json"),
    JSON.stringify({
      fetched_at: "2026-10-02T10:00:00Z",
      client_version: "test",
      models: [
        {
          slug: "gpt-6-luna",
          default_reasoning_level: "medium",
          visibility: "list",
          supported_in_api: true,
          supported_reasoning_levels: [
            { effort: "low" },
            { effort: "medium" },
            { effort: "xhigh" },
          ],
        },
        {
          slug: "gpt-6-astra",
          default_reasoning_level: "high",
          visibility: "list",
          supported_in_api: true,
          supported_reasoning_levels: [
            { effort: "medium" },
            { effort: "high" },
            { effort: "max" },
          ],
        },
        {
          slug: "hidden",
          visibility: "hide",
          supported_in_api: true,
          supported_reasoning_levels: [{ effort: "max" }],
        },
        {
          slug: "not-api",
          visibility: "list",
          supported_in_api: false,
          supported_reasoning_levels: [{ effort: "max" }],
        },
      ],
    }),
  );
  return { root, cwd, codexHome };
}

afterEach(async () => {
  await Promise.all(
    roots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  );
});

describe("pstack model configuration", () => {
  test("parent settings use the newest state row and reject conflicting unversioned records", async () => {
    const { codexHome } = await fixture();
    for (const [name, model, effort, timestamp] of [
      [
        "state_1.sqlite",
        "gpt-6-luna",
        "medium",
        Date.parse("2026-09-01T00:00:00Z"),
      ],
      [
        "state_99.sqlite",
        "gpt-6-astra",
        "max",
        Date.parse("2026-10-03T00:00:00Z"),
      ],
    ]) {
      const db = new Database(join(codexHome, name));
      db.exec(
        "CREATE TABLE threads (id TEXT, model TEXT, reasoning_effort TEXT, updated_at_ms INTEGER)",
      );
      db.query("INSERT INTO threads VALUES (?, ?, ?, ?)").run(
        "active-thread",
        model,
        effort,
        timestamp,
      );
      db.close();
    }
    expect(
      await readActiveParent({ codexHome, threadId: "active-thread" }),
    ).toMatchObject({ model: "gpt-6-astra", effort: "max" });
    const old = new Database(join(codexHome, "state_1.sqlite"));
    old.exec("UPDATE threads SET updated_at_ms = NULL");
    old.close();
    await expect(
      readActiveParent({ codexHome, threadId: "active-thread" }),
    ).rejects.toThrow("ambiguous settings");
  });

  test("aliases and runtime-verified choices work without a model cache", async () => {
    const { cwd, codexHome } = await fixture();
    await rm(join(codexHome, "models_cache.json"));
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: { "bug-fix": "inherit-parent" } },
    });
    expect(
      (await resolveRole("bug-fix", { cwd, codexHome })).choices[0],
    ).toMatchObject({ choice: "inherit-parent" });
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        roles: { "bug-fix": { model: "runtime-model", effort: "high" } },
        verifiedPairs: [
          {
            model: "runtime-model",
            effort: "high",
            evidence: "Confirmed by the active runtime.",
          },
        ],
      },
    });
    expect(
      (await resolveRole("bug-fix", { cwd, codexHome })).choices[0],
    ).toMatchObject({ model: "runtime-model", effort: "high" });
    await expect(
      writeConfiguration({
        scope: "project",
        cwd,
        codexHome,
        input: {
          roles: { "bug-fix": { model: "unverified", effort: "high" } },
        },
      }),
    ).rejects.toThrow("not in the verified");
  });
  test("reads the installed cache schema and exposes only listed API models with their effort pairs", async () => {
    const { codexHome } = await fixture();
    const catalog = await readModelCatalog({ codexHome });
    expect(catalog.models).toEqual([
      {
        model: "gpt-6-luna",
        efforts: ["low", "medium", "xhigh"],
        defaultEffort: "medium",
      },
      {
        model: "gpt-6-astra",
        efforts: ["medium", "high", "max"],
        defaultEffort: "high",
      },
    ]);
    expect(catalog.fetchedAt).toBe("2026-10-02T10:00:00Z");
  });

  test("resolves fallback defaults and applies project-over-user role isolation", async () => {
    const { cwd, codexHome } = await fixture();
    expect(await resolveRole("bug-fix", { cwd, codexHome })).toEqual({
      role: "bug-fix",
      scope: "fallback",
      panel: false,
      choices: [{ model: "gpt-6-luna", effort: "xhigh", budget: null }],
    });
    await writeConfiguration({
      scope: "user",
      cwd,
      codexHome,
      input: {
        budget: BUDGETS[0],
        roles: { "bug-fix": { model: "gpt-6-astra", effort: "max" } },
      },
    });
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        budget: BUDGETS[0],
        roles: { "bug-fix": { model: "gpt-6-luna", effort: "medium" } },
      },
    });
    expect((await resolveRole("bug-fix", { cwd, codexHome })).choices).toEqual([
      { model: "gpt-6-luna", effort: "medium", budget: BUDGETS[0] },
    ]);
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: { "bug-fix": null } },
    });
    expect((await resolveRole("bug-fix", { cwd, codexHome })).choices).toEqual([
      { model: "gpt-6-astra", effort: "max", budget: BUDGETS[0] },
    ]);
  });

  test("finds project agent configuration from a nested working directory", async () => {
    const { cwd, codexHome } = await fixture();
    const nested = join(cwd, "src", "features");
    await mkdir(join(cwd, ".git"), { recursive: true });
    await mkdir(nested, { recursive: true });
    await writeConfiguration({
      scope: "project",
      cwd: nested,
      codexHome,
      input: {
        roles: { "bug-fix": { model: "gpt-6-luna", effort: "medium" } },
      },
    });
    expect(
      await readFile(
        join(cwd, ".codex", "agents", "pstack-bug-fix.toml"),
        "utf8",
      ),
    ).toContain('model = "gpt-6-luna"');
    expect(
      (await show({ cwd: nested, codexHome })).roles["bug-fix"].choices,
    ).toEqual([{ model: "gpt-6-luna", effort: "medium" }]);
  });

  test("keeps panel seats and aliases, counts aliases, and removes only obsolete owned seats", async () => {
    const { cwd, codexHome } = await fixture();
    const directory = join(cwd, ".codex", "agents");
    await mkdir(directory, { recursive: true });
    await writeFile(
      join(directory, "pstack-arena-runners-04.toml"),
      `name = "pstack-arena-runners-04"\ndescription = "old seat"\ndeveloper_instructions = "Pstack role: arena runners (seat 4). Follow the role instructions supplied by the invoking workflow."\nmodel = "gpt-6-luna"\nmodel_reasoning_effort = "medium"\n`,
    );
    await writeFile(
      join(directory, "pstack-arena-runners-not-seat.toml"),
      "leave me alone",
    );
    await writeFile(
      join(directory, "pstack-other-tool.toml"),
      "leave me alone too",
    );
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        budget: BUDGETS[0],
        roles: {
          "arena runners": [
            "auto",
            { model: "gpt-6-astra", effort: "max" },
            "inherit-parent",
          ],
        },
      },
    });
    const resolved = await resolveRole("arena runners", { cwd, codexHome });
    expect(
      resolved.choices.map((item) => item.choice ?? [item.model, item.effort]),
    ).toEqual(["auto", ["gpt-6-astra", "max"], "inherit-parent"]);
    const names = await readdir(directory);
    expect(names).toContain("pstack-arena-runners-01.toml");
    expect(names).toContain("pstack-arena-runners-02.toml");
    expect(names).toContain("pstack-arena-runners-03.toml");
    expect(names).not.toContain("pstack-arena-runners-04.toml");
    expect(names).toContain("pstack-arena-runners-not-seat.toml");
    expect(names).toContain("pstack-other-tool.toml");
    const alias = TOML.parse(
      await readFile(join(directory, "pstack-arena-runners-01.toml"), "utf8"),
    );
    expect(alias).toMatchObject({
      name: "pstack-arena-runners-01",
      description: expect.any(String),
      developer_instructions: expect.stringContaining(
        "Pstack setup choice: auto.",
      ),
    });
    expect(alias.model).toBeUndefined();
    expect(alias.model_reasoning_effort).toBeUndefined();
  });

  test("lists retired pstack agents and removes only explicitly dropped files in the selected scope", async () => {
    const { cwd, codexHome } = await fixture();
    const directory = join(cwd, ".codex", "agents");
    await mkdir(directory, { recursive: true });
    await writeFile(
      join(directory, "pstack-how-critics.toml"),
      'name = "pstack-how-critics"\ndescription = "retired"\ndeveloper_instructions = "old role"\n',
    );
    await writeFile(join(directory, "other-agent.toml"), "leave me alone");
    const current = await show({ cwd, codexHome });
    expect(current.retiredFiles).toContainEqual({
      scope: "project",
      file: "pstack-how-critics.toml",
    });
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: {} },
    });
    expect(await readdir(directory)).toContain("pstack-how-critics.toml");
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: {}, retiredFiles: ["pstack-how-critics.toml"] },
    });
    expect(await readdir(directory)).toEqual(["other-agent.toml"]);
  });

  test("retains omitted role overrides on rerun and deletes only an explicitly removed role", async () => {
    const { cwd, codexHome } = await fixture();
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        roles: {
          "bug-fix": { model: "gpt-6-luna", effort: "xhigh" },
          "how explainer": { model: "gpt-6-astra", effort: "max" },
        },
      },
    });
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        roles: { "bug-fix": { model: "gpt-6-luna", effort: "medium" } },
      },
    });
    expect(
      (await resolveRole("how explainer", { cwd, codexHome })).choices[0],
    ).toMatchObject({ model: "gpt-6-astra", effort: "max" });
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: { "how explainer": null } },
    });
    expect((await resolveRole("how explainer", { cwd, codexHome })).scope).toBe(
      "fallback",
    );
    expect(
      (await resolveRole("how explainer", { cwd, codexHome })).choices[0],
    ).toMatchObject({ model: "gpt-6-astra", effort: "max" });
  });

  test("maps budgets to supported effort pairs without inventing model suffixes", async () => {
    const { codexHome } = await fixture();
    const catalog = await readModelCatalog({ codexHome });
    expect(effortForBudget("gpt-6-luna", "xhigh", BUDGETS[3], catalog)).toBe(
      "medium",
    );
    expect(effortForBudget("gpt-6-astra", "max", BUDGETS[1], catalog)).toBe(
      "high",
    );
    expect(effortForBudget("gpt-6-astra", "max", BUDGETS[2], catalog)).toBe(
      "high",
    );
    expect(effortForBudget("gpt-6-astra", "max", BUDGETS[0], catalog)).toBe(
      "max",
    );
  });

  test("resolves the exact parent thread pair and uses only a verified catalog default when effort is absent", async () => {
    const { codexHome } = await fixture();
    const db = new Database(join(codexHome, "state_test.sqlite"));
    db.exec(
      "CREATE TABLE threads (id TEXT PRIMARY KEY, model TEXT, reasoning_effort TEXT)",
    );
    db.query("INSERT INTO threads VALUES (?, ?, ?)").run(
      "other-thread",
      "gpt-6-astra",
      "max",
    );
    db.query("INSERT INTO threads VALUES (?, ?, ?)").run(
      "active-thread",
      "gpt-6-luna",
      "xhigh",
    );
    db.query("INSERT INTO threads VALUES (?, ?, ?)").run(
      "default-effort-thread",
      "gpt-6-luna",
      null,
    );
    db.query("INSERT INTO threads VALUES (?, ?, ?)").run(
      "unknown-model-thread",
      "unlisted-model",
      null,
    );
    db.close();
    expect(
      await readActiveParent({ codexHome, threadId: "active-thread" }),
    ).toEqual({
      threadId: "active-thread",
      model: "gpt-6-luna",
      effort: "xhigh",
      effortSource: "thread",
    });
    expect(
      await readActiveParent({ codexHome, threadId: "default-effort-thread" }),
    ).toEqual({
      threadId: "default-effort-thread",
      model: "gpt-6-luna",
      effort: "medium",
      effortSource: "verified-catalog-default",
    });
    await expect(
      readActiveParent({ codexHome, threadId: "unknown-model-thread" }),
    ).rejects.toThrow("no verified catalog default exists");
  });

  test("fails clearly when parent identity or required state fields are unavailable", async () => {
    const { codexHome } = await fixture();
    await expect(readActiveParent({ codexHome, threadId: "" })).rejects.toThrow(
      "CODEX_THREAD_ID is not set",
    );
    const db = new Database(join(codexHome, "state_test.sqlite"));
    db.exec("CREATE TABLE threads (id TEXT PRIMARY KEY, model TEXT)");
    db.close();
    await expect(
      readActiveParent({ codexHome, threadId: "active-thread" }),
    ).rejects.toThrow(
      "threads.id, threads.model, and threads.reasoning_effort",
    );
  });

  test("rejects unknown models and efforts before writing any role file", async () => {
    const { cwd, codexHome } = await fixture();
    await expect(
      writeConfiguration({
        scope: "project",
        cwd,
        codexHome,
        input: {
          roles: { "bug-fix": { model: "not-verified", effort: "max" } },
        },
      }),
    ).rejects.toThrow("not in the verified Codex model catalog");
    await expect(
      writeConfiguration({
        scope: "project",
        cwd,
        codexHome,
        input: { roles: { "bug-fix": { model: "gpt-6-luna", effort: "max" } } },
      }),
    ).rejects.toThrow("not supported by model 'gpt-6-luna'");
    expect(
      await readdir(join(cwd, ".codex", "agents")).catch((error) =>
        error.code === "ENOENT" ? [] : Promise.reject(error),
      ),
    ).toEqual([]);
  });

  test("accepts a catalog-missing pair only with explicit runtime verification evidence", async () => {
    const { cwd, codexHome } = await fixture();
    await expect(
      writeConfiguration({
        scope: "project",
        cwd,
        codexHome,
        input: {
          roles: { "bug-fix": { model: "runtime-only-model", effort: "high" } },
        },
      }),
    ).rejects.toThrow("not in the verified Codex model catalog");
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: {
        roles: { "bug-fix": { model: "runtime-only-model", effort: "high" } },
        verifiedPairs: [
          {
            model: "runtime-only-model",
            effort: "high",
            evidence:
              "A child using this exact pair completed in the active runtime.",
          },
        ],
      },
    });
    expect(
      (await resolveRole("bug-fix", { cwd, codexHome })).choices[0],
    ).toMatchObject({ model: "runtime-only-model", effort: "high" });
  });

  test("validates all choices before changing existing files", async () => {
    const { cwd, codexHome } = await fixture();
    await writeConfiguration({
      scope: "project",
      cwd,
      codexHome,
      input: { roles: { "bug-fix": { model: "gpt-6-luna", effort: "xhigh" } } },
    });
    const path = join(cwd, ".codex", "agents", "pstack-bug-fix.toml");
    const before = await readFile(path, "utf8");
    await expect(
      writeConfiguration({
        scope: "project",
        cwd,
        codexHome,
        input: {
          roles: {
            "how explorer": { model: "gpt-6-luna", effort: "medium" },
            "bug-fix": { model: "gpt-6-astra", effort: "xhigh" },
          },
        },
      }),
    ).rejects.toThrow("not supported by model 'gpt-6-astra'");
    expect(await readFile(path, "utf8")).toBe(before);
    expect(await readdir(join(cwd, ".codex", "agents"))).toEqual([
      "pstack-bug-fix.toml",
    ]);
  });

  test("CLI write reads the explicit input file and writes only its selected temporary scope", async () => {
    const { root, cwd, codexHome } = await fixture();
    const inputPath = join(root, "accepted.json");
    await writeFile(
      inputPath,
      JSON.stringify({
        roles: { "bug-fix": { model: "gpt-6-luna", effort: "medium" } },
      }),
    );
    const scriptPath = new URL("./pstack-models.mjs", import.meta.url).pathname;
    const result = Bun.spawnSync(
      [
        "bun",
        scriptPath,
        "write",
        "--scope",
        "project",
        "--input",
        `@${inputPath}`,
      ],
      {
        cwd,
        env: { ...process.env, CODEX_HOME: codexHome },
      },
    );
    expect(result.exitCode).toBe(0);
    expect(result.stdout.toString()).toContain('"scope": "project"');
    expect(
      await readFile(
        join(cwd, ".codex", "agents", "pstack-bug-fix.toml"),
        "utf8",
      ),
    ).toContain('model_reasoning_effort = "medium"');
    expect(
      await readdir(join(codexHome, "agents")).catch((error) =>
        error.code === "ENOENT" ? [] : Promise.reject(error),
      ),
    ).toEqual([]);
  });
});
