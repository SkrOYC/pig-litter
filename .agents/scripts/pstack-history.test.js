import { afterEach, describe, expect, it } from "bun:test";
import { Database } from "bun:sqlite";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { main, parseArgs } from "./pstack-history.mjs";

const previousCodexHome = process.env.CODEX_HOME;
afterEach(() => {
  if (previousCodexHome === undefined) delete process.env.CODEX_HOME;
  else process.env.CODEX_HOME = previousCodexHome;
});

async function fixture({
  stateName = "state_99.sqlite",
  historyName = "thread_history_7.sqlite",
  includeHistory = true,
} = {}) {
  const root = await mkdtemp(join(tmpdir(), "pstack-history-"));
  process.env.CODEX_HOME = root;
  const workspace = join(root, "checkout");
  const subdir = join(workspace, "packages", "app");
  const sibling = join(root, "checkout-worktree");
  await mkdir(subdir, { recursive: true });
  await mkdir(sibling, { recursive: true });
  const state = new Database(join(root, stateName));
  state.exec(`CREATE TABLE threads (
    id TEXT PRIMARY KEY, cwd TEXT, history_mode TEXT, rollout_path TEXT,
    updated_at_ms INTEGER, created_at_ms INTEGER, archived INTEGER
  )`);
  const legacyPath = join(root, "sessions", "legacy.jsonl");
  await mkdir(join(root, "sessions"), { recursive: true });
  await writeFile(
    legacyPath,
    [
      JSON.stringify({ type: "session_meta", id: "legacy" }),
      JSON.stringify({
        rollout_ordinal: 10,
        item_id: "legacy-item-a",
        type: "userMessage",
        message: { content: [{ text: "alpha legacy note" }] },
      }),
      JSON.stringify({
        rollout_ordinal: 20,
        item_id: "legacy-item-b",
        type: "agentMessage",
        text: "beta response",
      }),
    ].join("\n"),
  );
  const threads = [
    [
      "thread-new",
      workspace,
      "paginated",
      null,
      Date.parse("2026-10-02T12:00:00Z"),
      Date.parse("2026-10-01T00:00:00Z"),
      0,
    ],
    [
      "thread-child",
      subdir,
      "paginated",
      null,
      Date.parse("2026-10-01T12:00:00Z"),
      Date.parse("2026-09-30T00:00:00Z"),
      0,
    ],
    [
      "thread-old",
      workspace,
      "legacy",
      legacyPath,
      Date.parse("2026-09-01T12:00:00Z"),
      Date.parse("2026-08-31T00:00:00Z"),
      1,
    ],
    [
      "thread-sibling",
      sibling,
      "paginated",
      null,
      Date.parse("2026-10-03T12:00:00Z"),
      Date.parse("2026-10-02T00:00:00Z"),
      0,
    ],
  ];
  const insert = state.query(
    "INSERT INTO threads VALUES (?, ?, ?, ?, ?, ?, ?)",
  );
  for (const thread of threads) insert.run(...thread);
  state.close();
  if (includeHistory) {
    const history = new Database(join(root, historyName));
    history.exec(`CREATE TABLE thread_items (
      thread_id TEXT, item_id TEXT, rollout_ordinal INTEGER,
      item_type TEXT, item_json TEXT, created_at_ms INTEGER
    )`);
    const add = history.query(
      "INSERT INTO thread_items VALUES (?, ?, ?, ?, ?, ?)",
    );
    for (const [ordinal, text] of [
      [3, "first needle"],
      [3, "same ordinal needle"],
      [8, "second needle"],
      [9, "last item"],
    ]) {
      add.run(
        "thread-new",
        `item-${ordinal}-${text}`,
        ordinal,
        "userMessage",
        JSON.stringify({ content: [{ text }] }),
        Date.now(),
      );
    }
    add.run(
      "thread-child",
      "child-item",
      1,
      "agentMessage",
      JSON.stringify({ text: "child answer" }),
      Date.now(),
    );
    history.close();
  }
  return { root, workspace, subdir, sibling, legacyPath };
}

describe("pstack-history", () => {
  it("uses the newest duplicate thread metadata across compatible state databases", async () => {
    const f = await fixture();
    try {
      const old = new Database(join(f.root, "state_1.sqlite"));
      old.exec(
        "CREATE TABLE threads (id TEXT, cwd TEXT, history_mode TEXT, updated_at_ms INTEGER)",
      );
      old
        .query("INSERT INTO threads VALUES (?, ?, ?, ?)")
        .run(
          "thread-new",
          f.workspace,
          "legacy",
          Date.parse("2026-09-01T00:00:00Z"),
        );
      old.close();
      const rows = JSON.parse(
        await main(parseArgs(["list", "--cwd", f.workspace, "--limit", "10"])),
      );
      expect(rows.filter((row) => row.thread_id === "thread-new")).toHaveLength(
        1,
      );
      expect(rows[0]).toMatchObject({
        thread_id: "thread-new",
        history_mode: "paginated",
        updatedAt: "2026-10-02T12:00:00.000Z",
      });
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });
  it("parses required scoped commands and rejects unsafe limits", () => {
    expect(
      parseArgs([
        "list",
        "--cwd",
        "/tmp/project",
        "--since",
        "2026-01-01T00:00:00Z",
      ]),
    ).toEqual({
      command: "list",
      cwd: "/tmp/project",
      since: "2026-01-01T00:00:00Z",
    });
    expect(
      parseArgs([
        "read",
        "--id",
        "thread",
        "--after",
        "3",
        "--after-id",
        "item-b",
      ]),
    ).toEqual({
      command: "read",
      id: "thread",
      after: 3,
      "after-id": "item-b",
    });
    expect(() =>
      parseArgs(["read", "--id", "thread", "--after-id", "item-b"]),
    ).toThrow("--after-id requires --after");
    expect(() =>
      parseArgs([
        "search",
        "--cwd",
        "/tmp",
        "--pattern",
        "x",
        "--limit",
        "1000",
      ]),
    ).toThrow("--limit must be an integer");
  });

  it("scopes exact workspace and descendants, sorts newest first, filters since, and excludes sibling worktrees", async () => {
    const f = await fixture();
    try {
      const rows = JSON.parse(
        await main(
          parseArgs([
            "list",
            "--cwd",
            f.workspace,
            "--since",
            "2026-10-01T00:00:00Z",
            "--limit",
            "10",
          ]),
        ),
      );
      expect(rows.map((row) => row.thread_id)).toEqual([
        "thread-new",
        "thread-child",
      ]);
      expect(rows[0].cwd).toBe(f.workspace);
      expect(rows[0].history_mode).toBe("paginated");
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("reads SQLite in bounded pages with stable ordinal and identity citations", async () => {
    const f = await fixture();
    try {
      const first = JSON.parse(
        await main(parseArgs(["read", "--id", "thread-new", "--limit", "1"])),
      );
      expect(first).toHaveLength(1);
      expect(first[0].ordinal).toBe(3);
      expect(first[0].citation).toBe("thread-new/item-3-first needle@3");
      const tied = JSON.parse(
        await main(
          parseArgs([
            "read",
            "--id",
            "thread-new",
            "--after",
            "3",
            "--after-id",
            first[0].item_id,
            "--limit",
            "1",
          ]),
        ),
      );
      expect(tied.map((item) => item.citation)).toEqual([
        "thread-new/item-3-same ordinal needle@3",
      ]);
      const next = JSON.parse(
        await main(
          parseArgs([
            "read",
            "--id",
            "thread-new",
            "--after",
            "3",
            "--after-id",
            tied[0].item_id,
            "--limit",
            "2",
          ]),
        ),
      );
      expect(next.map((item) => item.ordinal)).toEqual([8, 9]);
      expect(
        await main(parseArgs(["read", "--id", "thread-new", "--after", "9"])),
      ).toBe("[]");
      await expect(
        main(parseArgs(["read", "--id", "thread-sibling"])),
      ).rejects.toThrow("No readable history records found");
      await expect(main(parseArgs(["read", "--id", "absent"]))).rejects.toThrow(
        "Thread not found",
      );
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("reads legacy JSONL using rollout identities and reports malformed records", async () => {
    const f = await fixture({ includeHistory: false });
    try {
      const items = JSON.parse(
        await main(parseArgs(["read", "--id", "thread-old", "--limit", "2"])),
      );
      expect(items.map((item) => item.citation)).toEqual([
        "thread-old/legacy-item-a@10",
        "thread-old/legacy-item-b@20",
      ]);
      await writeFile(f.legacyPath, "{broken\n");
      await expect(
        main(parseArgs(["read", "--id", "thread-old"])),
      ).rejects.toThrow("Malformed JSONL record");
      await writeFile(
        f.legacyPath,
        [
          JSON.stringify({
            type: "userMessage",
            rollout_ordinal: 1,
            item_id: "early",
            text: "early item",
          }),
          JSON.stringify({
            type: "userMessage",
            rollout_ordinal: 2,
            item_id: "later",
            text: "later item",
          }),
          "{malformed trailing record",
        ].join("\n"),
      );
      const early = JSON.parse(
        await main(parseArgs(["read", "--id", "thread-old", "--limit", "1"])),
      );
      expect(early[0].item_id).toBe("early");
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("continues legacy pages through equal ordinals using item identity", async () => {
    const f = await fixture({ includeHistory: false });
    try {
      await writeFile(
        f.legacyPath,
        [
          JSON.stringify({
            rollout_ordinal: 4,
            item_id: "z-item",
            type: "userMessage",
            text: "z",
          }),
          JSON.stringify({
            rollout_ordinal: 4,
            item_id: "a-item",
            type: "userMessage",
            text: "a",
          }),
        ].join("\n"),
      );
      const first = JSON.parse(
        await main(parseArgs(["read", "--id", "thread-old", "--limit", "1"])),
      );
      expect(first[0].item_id).toBe("a-item");
      const second = JSON.parse(
        await main(
          parseArgs([
            "read",
            "--id",
            "thread-old",
            "--after",
            "4",
            "--after-id",
            "a-item",
            "--limit",
            "1",
          ]),
        ),
      );
      expect(second.map((item) => item.item_id)).toEqual(["z-item"]);
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("bounds search output, preserves item citations, and returns most recent audit timestamp", async () => {
    const f = await fixture();
    try {
      const matches = JSON.parse(
        await main(
          parseArgs([
            "search",
            "--cwd",
            f.workspace,
            "--pattern",
            "needle",
            "--limit",
            "1",
          ]),
        ),
      );
      expect(matches).toHaveLength(1);
      expect(matches[0].citation).toBe("thread-new/item-3-first needle@3");
      const last = JSON.parse(
        await main(parseArgs(["last", "--cwd", f.workspace])),
      );
      expect(last.thread_id).toBe("thread-new");
      expect(last.timestamp).toBe("2026-10-02T12:00:00.000Z");
      expect(last.createdAt).toBe("2026-10-01T00:00:00.000Z");
      expect(last.updatedAt).toBe("2026-10-02T12:00:00.000Z");
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("continues bounded search pages when the match is beyond the first page", async () => {
    const f = await fixture();
    try {
      const db = new Database(join(f.root, "thread_history_7.sqlite"));
      const add = db.query(
        "INSERT INTO thread_items VALUES (?, ?, ?, ?, ?, ?)",
      );
      for (let ordinal = 10; ordinal < 260; ordinal += 1) {
        add.run(
          "thread-new",
          `filler-${ordinal}`,
          ordinal,
          "userMessage",
          JSON.stringify({ text: "ordinary filler" }),
          Date.now(),
        );
      }
      add.run(
        "thread-new",
        "late-match",
        261,
        "userMessage",
        JSON.stringify({ text: "find me after paging" }),
        Date.now(),
      );
      db.close();
      const matches = JSON.parse(
        await main(
          parseArgs([
            "search",
            "--cwd",
            f.workspace,
            "--pattern",
            "after paging",
            "--limit",
            "1",
          ]),
        ),
      );
      expect(matches).toHaveLength(1);
      expect(matches[0].citation).toBe("thread-new/late-match@261");
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("returns no search matches when a thread ends at an exact page boundary", async () => {
    const f = await fixture();
    try {
      const db = new Database(join(f.root, "thread_history_7.sqlite"));
      const add = db.query(
        "INSERT INTO thread_items VALUES (?, ?, ?, ?, ?, ?)",
      );
      for (let ordinal = 10; ordinal < 256; ordinal += 1) {
        add.run(
          "thread-new",
          `exact-page-${ordinal}`,
          ordinal,
          "userMessage",
          JSON.stringify({ text: "ordinary filler" }),
          Date.now(),
        );
      }
      db.close();
      const matches = JSON.parse(
        await main(
          parseArgs([
            "search",
            "--cwd",
            f.workspace,
            "--pattern",
            "no-such-text",
            "--limit",
            "1",
          ]),
        ),
      );
      expect(matches).toEqual([]);
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("selects the history database containing the selected thread", async () => {
    const f = await fixture();
    try {
      const first = new Database(join(f.root, "thread_history_7.sqlite"));
      first
        .query("DELETE FROM thread_items WHERE thread_id = ?")
        .run("thread-new");
      first.close();
      const other = new Database(join(f.root, "thread_history_8.sqlite"));
      other.exec(
        "CREATE TABLE thread_items (thread_id TEXT, item_id TEXT, rollout_ordinal INTEGER, item_type TEXT, item_json TEXT)",
      );
      other
        .query("INSERT INTO thread_items VALUES (?, ?, ?, ?, ?)")
        .run(
          "thread-new",
          "right-store",
          1,
          "userMessage",
          JSON.stringify({ text: "right database" }),
        );
      other.close();
      const items = JSON.parse(
        await main(parseArgs(["read", "--id", "thread-new"])),
      );
      expect(items[0].item_id).toBe("right-store");
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("handles renamed storage schemas and fails clearly when the state database is corrupt", async () => {
    const f = await fixture({
      stateName: "state_123.sqlite",
      historyName: "thread_history_8.sqlite",
    });
    try {
      const result = JSON.parse(
        await main(parseArgs(["last", "--cwd", f.workspace])),
      );
      expect(result.thread_id).toBe("thread-new");
      const badRoot = await mkdtemp(join(tmpdir(), "pstack-history-corrupt-"));
      process.env.CODEX_HOME = badRoot;
      await writeFile(join(badRoot, "state_999.sqlite"), "not sqlite");
      await expect(
        main(parseArgs(["list", "--cwd", f.workspace])),
      ).rejects.toThrow("No readable state database");
      await rm(badRoot, { recursive: true, force: true });
    } finally {
      await rm(f.root, { recursive: true, force: true });
    }
  });

  it("discovers compatible schemas without optional identity columns and reports malformed items", async () => {
    const root = await mkdtemp(join(tmpdir(), "pstack-history-schema-"));
    process.env.CODEX_HOME = root;
    const state = new Database(join(root, "state_2.sqlite"));
    state.exec("CREATE TABLE threads (id TEXT, cwd TEXT, created_at TEXT)");
    state
      .query("INSERT INTO threads VALUES (?, ?, ?)")
      .run("minimal", root, "2026-10-01T10:00:00Z");
    state.close();
    const history = new Database(join(root, "thread_history_2.sqlite"));
    history.exec("CREATE TABLE thread_items (thread_id TEXT, item_json TEXT)");
    history
      .query("INSERT INTO thread_items VALUES (?, ?)")
      .run("minimal", "{broken");
    history.close();
    try {
      await expect(
        main(parseArgs(["read", "--id", "minimal"])),
      ).rejects.toThrow("Malformed item_json");
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
});
