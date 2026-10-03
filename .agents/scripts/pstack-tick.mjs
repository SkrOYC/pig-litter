const defaultMessage =
  "[pstack audit tick] Run the armed audit tick from the current pstack playbook and standing orders.";

async function main() {
  const args = process.argv.slice(2);
  if (args.includes("--help")) {
    process.stdout.write(
      "pstack-tick.mjs --thread ID [--minutes 30] [--message TEXT]\nOne supervised timer queues one audit prompt through codex queue. Stop its process to cancel.\n",
    );
    return;
  }
  const options = {};
  for (let index = 0; index < args.length; index += 2) {
    const key = args[index];
    if (
      !["--thread", "--minutes", "--message"].includes(key) ||
      args[index + 1] === undefined ||
      options[key] !== undefined
    )
      throw new Error(`Invalid option: ${key}`);
    options[key] = args[index + 1];
  }
  const thread = options["--thread"] ?? process.env.CODEX_THREAD_ID;
  if (
    typeof thread !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(thread)
  )
    throw new Error("A native thread ID is required.");
  const minutes = Number(options["--minutes"] ?? 30);
  if (
    !Number.isFinite(minutes) ||
    minutes < 0 ||
    !Number.isSafeInteger(Math.round(minutes * 60000))
  )
    throw new Error("Minutes must be a finite non-negative duration.");
  process.stdout.write(
    `${JSON.stringify({ status: "armed", thread, minutes, pid: process.pid })}\n`,
  );
  await Bun.sleep(minutes * 60000);
  const message = options["--message"] ?? defaultMessage;
  const child = Bun.spawn(
    ["codex", "queue", "--thread", thread, "--message", message],
    { stdin: "ignore", stdout: "inherit", stderr: "inherit" },
  );
  const stop = () => {
    child.kill("SIGTERM");
  };
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
  const exitCode = await child.exited;
  process.off("SIGINT", stop);
  process.off("SIGTERM", stop);
  if (exitCode !== 0)
    throw new Error(
      `Audit tick was not accepted: codex queue exited ${exitCode}.`,
    );
  process.stdout.write(`${JSON.stringify({ status: "queued", thread })}\n`);
}

try {
  await main();
} catch (error) {
  process.stderr.write(
    `${error instanceof Error ? error.message : String(error)}\n`,
  );
  process.exitCode = 1;
}
