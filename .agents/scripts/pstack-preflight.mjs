import { spawn } from "node:child_process";
import { closeSync, openSync } from "node:fs";
import { createInterface } from "node:readline";

export async function runPreflight({
  cwd,
  script,
  log,
  signal,
  env = process.env,
  timeoutMs = 20000,
  killAfterMs = 1000,
}) {
  const descriptor = openSync(log, "wx", 0o600);
  const child = spawn(
    "codex",
    ["app-server", "--listen", "stdio://", "-c", "allow_login_shell=false"],
    {
      cwd,
      env,
      stdio: ["pipe", "pipe", descriptor],
    },
  );
  const lines = createInterface({ input: child.stdout });
  let result;
  let failure;
  let initialized = false;
  let bytes = 0;
  let killTimer;
  const stop = (message) => {
    failure ??= new Error(message);
    child.kill("SIGTERM");
    killTimer ??= setTimeout(() => {
      child.kill("SIGKILL");
      child.stdin.destroy();
      child.stdout.destroy();
    }, killAfterMs);
  };
  const abort = () => stop("Preflight App Server cancelled");
  if (signal?.aborted) abort();
  else signal?.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(
    () => stop("Preflight App Server timed out"),
    timeoutMs,
  );
  const send = (message) => child.stdin.write(`${JSON.stringify(message)}\n`);
  child.stdin.on("error", (error) =>
    stop(`Preflight input failed: ${error.message}`),
  );
  lines.on("line", (line) => {
    bytes += Buffer.byteLength(line);
    if (bytes > 1024 * 1024)
      return stop("Preflight App Server exceeded its response bound");
    try {
      const message = JSON.parse(line);
      if (message.id === 0) {
        if (initialized || message.error || !message.result)
          throw new Error("Preflight initialization failed");
        initialized = true;
        send({ method: "initialized", params: {} });
        send({
          id: 1,
          method: "command/exec",
          params: {
            command: [process.execPath, script],
            cwd,
            sandboxPolicy: { type: "readOnly", networkAccess: false },
            timeoutMs: 10000,
            outputBytesCap: 8192,
          },
        });
      } else if (message.id === 1) {
        if (!initialized || result || message.error || !message.result)
          throw new Error("Preflight command/exec failed or is unsupported");
        result = message.result;
        child.stdin.end();
      }
    } catch (error) {
      stop(error.message);
    }
  });
  const closed = new Promise((resolve) => {
    child.once("error", (error) => {
      failure = error;
    });
    child.once("close", (code) => resolve(code));
  });
  send({
    id: 0,
    method: "initialize",
    params: { clientInfo: { name: "pstack_preflight", version: "1" } },
  });
  const code = await closed;
  clearTimeout(timeout);
  clearTimeout(killTimer);
  signal?.removeEventListener("abort", abort);
  lines.close();
  closeSync(descriptor);
  if (failure) throw failure;
  if (code !== 0 || !Number.isInteger(result?.exitCode))
    throw new Error("Preflight App Server returned no completed command");
  if (result.exitCode !== 0)
    throw new Error(
      `Preflight failed with exit ${result.exitCode}: ${String(result.stderr || result.stdout || "no diagnostic").slice(0, 8192)}`,
    );
  return {
    status: "verified",
    method: "command/exec",
    sandbox: "readOnly",
    networkAccess: false,
    exitCode: result.exitCode,
  };
}
