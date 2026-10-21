import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { runPreflight } from "./pstack-preflight.mjs";
import { reviewerFixture } from "./pstack-reviewer-test-fixture.js";

function exited(f) {
  const pid = Number(readFileSync(join(f.home, "preflight.pid"), "utf8"));
  try {
    process.kill(pid, 0);
    return false;
  } catch (error) {
    return error.code === "ESRCH";
  }
}

test("a timed-out preflight joins a server that ignores SIGTERM", async () => {
  const f = reviewerFixture("stubborn-preflight");
  try {
    const began = performance.now();
    await expect(
      runPreflight({
        cwd: f.cwd,
        script: f.prompt,
        log: join(f.directory, "server.stderr"),
        env: f.env,
        timeoutMs: 200,
        killAfterMs: 50,
      }),
    ).rejects.toThrow("timed out");
    expect(performance.now() - began).toBeLessThan(2000);
    expect(exited(f)).toBe(true);
    expect(f.calls().map((call) => call.method)).toEqual(["initialize"]);
  } finally {
    f.close();
  }
});

test("cancelling a stubborn preflight joins it before returning", async () => {
  const f = reviewerFixture("stubborn-preflight");
  const controller = new AbortController();
  try {
    const running = runPreflight({
      cwd: f.cwd,
      script: f.prompt,
      log: join(f.directory, "server.stderr"),
      env: f.env,
      signal: controller.signal,
      killAfterMs: 50,
    });
    const deadline = Date.now() + 1500;
    while (!existsSync(join(f.home, "preflight.pid")) && Date.now() < deadline)
      await Bun.sleep(5);
    const began = performance.now();
    controller.abort();
    await expect(running).rejects.toThrow("cancelled");
    expect(performance.now() - began).toBeLessThan(1500);
    expect(exited(f)).toBe(true);
    expect(f.calls().map((call) => call.method)).toEqual(["initialize"]);
  } finally {
    controller.abort();
    f.close();
  }
});
