import { expect, test } from "bun:test";
import { mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { resolveRole } from "./pstack-models.mjs";

test("literal model-resolution calls in skills resolve to supported roles", async () => {
  const directory = await mkdtemp(join(tmpdir(), "pstack-caller-contract-"));
  try {
    const files = [];
    async function walk(path) {
      for (const entry of await readdir(path, { withFileTypes: true })) {
        const file = join(path, entry.name);
        if (entry.isDirectory()) await walk(file);
        else if (entry.name.endsWith(".md")) files.push(file);
      }
    }
    await walk(".agents/skills");
    const roles = new Set();
    for (const file of files) {
      const text = await readFile(file, "utf8");
      for (const match of text.matchAll(
        /pstack-models\.mjs resolve "([^"\n]+)"/g,
      )) {
        if (match[1] !== "ROLE LABEL") roles.add(match[1]);
      }
    }
    expect(roles.size).toBeGreaterThan(0);
    for (const role of roles) {
      expect(
        (await resolveRole(role, { cwd: directory, codexHome: directory }))
          .role,
      ).toBe(role);
    }
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("the shipped multi-phase plan template passes its own validator when filled", async () => {
  const directory = await mkdtemp(join(tmpdir(), "pstack-plan-contract-"));
  try {
    const text = await readFile(
      ".agents/skills/poteto-mode/playbooks/multi-phase-plan.md",
      "utf8",
    );
    const skeleton = text
      .match(/```markdown\n([\s\S]*?)\n```/)[1]
      .replace(/<[^<>]+>/g, "example");
    const file = join(directory, "plan.md");
    await writeFile(file, skeleton);
    const result = Bun.spawnSync([
      process.execPath,
      resolve(".agents/skills/poteto-mode/scripts/check-plan.mjs"),
      file,
    ]);
    expect({
      exitCode: result.exitCode,
      errors: result.stderr.toString(),
    }).toEqual({ exitCode: 0, errors: "" });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("unslop retains its stable rule identities including numbering gaps", async () => {
  const text = await readFile(".agents/skills/unslop/SKILL.md", "utf8");
  const ids = [...text.matchAll(/^(\d+)\. \*\*/gm)].map((match) =>
    Number(match[1]),
  );
  expect(ids).toEqual([
    3, 5, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 22, 23, 24, 25,
    26, 27, 28, 29, 30, 31, 32, 33,
  ]);
});
