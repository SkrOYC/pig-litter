import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { handleHook, setMode } from "./pstack-mode.mjs";

test("mode reminders follow explicit activation, session identity, resume, and compaction", () => {
  const stateDir = mkdtempSync(join(tmpdir(), "pstack-mode-"));
  try {
    const input = {
      hook_event_name: "UserPromptSubmit",
      session_id: "session-a",
      prompt: "Do the next task",
    };
    expect(handleHook(input, stateDir)).toEqual({});
    setMode("session-a", true, stateDir);
    const expected = {
      hookSpecificOutput: {
        hookEventName: "UserPromptSubmit",
        additionalContext:
          "New task? Playbook match or rigor needed -> apply $poteto-mode. Casual turn or user opts out -> don't.",
      },
    };
    expect(handleHook(input, stateDir)).toEqual(expected);
    expect(handleHook({ ...input, session_id: "session-b" }, stateDir)).toEqual(
      {},
    );
    for (const source of ["resume", "compact"]) {
      expect(
        handleHook(
          { hook_event_name: "SessionStart", session_id: "session-a", source },
          stateDir,
        ).hookSpecificOutput.additionalContext,
      ).toBe(expected.hookSpecificOutput.additionalContext);
    }
    expect(
      handleHook(
        { ...input, prompt: "Don't stop using poteto mode." },
        stateDir,
      ),
    ).toEqual(expected);
    expect(
      handleHook({ ...input, prompt: "Stop using poteto mode." }, stateDir),
    ).toEqual({});
    expect(handleHook(input, stateDir)).toEqual({});
  } finally {
    rmSync(stateDir, { recursive: true, force: true });
  }
});

test("clear and explicit deactivation remove mode state without enabling unrelated hooks", () => {
  const stateDir = mkdtempSync(join(tmpdir(), "pstack-mode-"));
  try {
    setMode("session-a", true, stateDir);
    expect(
      handleHook(
        {
          hook_event_name: "UserPromptSubmit",
          session_id: "session-a",
          prompt: "Don't use poteto mode.",
        },
        stateDir,
      ),
    ).toEqual({});
    setMode("session-a", true, stateDir);
    expect(
      handleHook(
        {
          hook_event_name: "SessionStart",
          session_id: "session-a",
          source: "clear",
        },
        stateDir,
      ),
    ).toEqual({});
    expect(
      handleHook(
        {
          hook_event_name: "UserPromptSubmit",
          session_id: "session-a",
          prompt: "Explain what $poteto-mode does",
        },
        stateDir,
      ),
    ).toEqual({});
    expect(
      handleHook(
        {
          hook_event_name: "UserPromptSubmit",
          session_id: "session-a",
          prompt: "$poteto-mode inspect this",
        },
        stateDir,
      ).hookSpecificOutput.additionalContext,
    ).toContain("apply $poteto-mode");
    setMode("session-a", true, stateDir);
    setMode("session-a", false, stateDir);
    expect(
      handleHook(
        {
          hook_event_name: "SessionStart",
          session_id: "session-a",
          source: "resume",
        },
        stateDir,
      ),
    ).toEqual({});
    expect(
      handleHook(
        { hook_event_name: "Stop", session_id: "session-a" },
        stateDir,
      ),
    ).toEqual({});
    expect(() => setMode("../../other", true, stateDir)).toThrow(
      "native session ID",
    );
  } finally {
    rmSync(stateDir, { recursive: true, force: true });
  }
});
