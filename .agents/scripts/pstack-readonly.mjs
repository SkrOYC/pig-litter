import {
  chmodSync,
  closeSync,
  existsSync,
  fstatSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  realpathSync,
  renameSync,
  rmdirSync,
  statSync,
  unlinkSync,
  writeFileSync,
  constants,
} from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { createHash, randomUUID } from "node:crypto";
import { collectExecution } from "./pstack-history-execution.mjs";
import { executionReceipt, parseRestrictions } from "./pstack-receipt.mjs";
import { runPreflight } from "./pstack-preflight.mjs";

function object(value, keys, name) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error(`${name} must be an object`);
  for (const key of Object.keys(value))
    if (!keys.includes(key)) throw new Error(`Unknown ${name} field: ${key}`);
  return value;
}

function parse(argv) {
  const options = {};
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i];
    if (options[key] !== undefined) throw new Error(`Duplicate option: ${key}`);
    if (key === "--ephemeral") {
      options[key] = true;
      continue;
    }
    if (
      ![
        "--cwd",
        "--prompt",
        "--output",
        "--model",
        "--effort",
        "--role",
        "--panel",
        "--preflight",
        "--restrictions",
      ].includes(key) ||
      argv[i + 1] === undefined
    )
      throw new Error(`Invalid option: ${key}`);
    options[key] = argv[++i];
  }
  if (options["--panel"]) {
    if (Object.keys(options).length !== 1)
      throw new Error(
        "--panel cannot be combined with single-reviewer options",
      );
    return object(
      JSON.parse(readFileSync(resolve(options["--panel"]), "utf8")),
      ["cwd", "jobs", "preflight", "restrictions"],
      "panel",
    );
  }
  return {
    cwd: options["--cwd"],
    preflight: options["--preflight"],
    restrictions: options["--restrictions"]
      ? JSON.parse(readFileSync(resolve(options["--restrictions"]), "utf8"))
      : {},
    jobs: [
      {
        prompt: options["--prompt"],
        output: options["--output"],
        model: options["--model"],
        effort: options["--effort"],
        role: options["--role"],
        ephemeral: options["--ephemeral"],
      },
    ],
  };
}

function normalize(panel) {
  if (typeof panel.cwd !== "string" || !panel.cwd)
    throw new Error("Required workspace cwd");
  const cwd = realpathSync(resolve(panel.cwd));
  if (!statSync(cwd).isDirectory())
    throw new Error("Workspace is not a directory");
  if (
    !Array.isArray(panel.jobs) ||
    !panel.jobs.length ||
    panel.jobs.length > 32
  )
    throw new Error("A panel needs 1 to 32 reviewer jobs");
  const jobs = panel.jobs.map((job) => {
    object(
      job,
      ["prompt", "output", "model", "effort", "role", "ephemeral"],
      "job",
    );
    for (const key of ["prompt", "output", "model"])
      if (typeof job[key] !== "string" || !job[key])
        throw new Error(`Required reviewer ${key}`);
    for (const key of ["effort", "role"])
      if (job[key] !== undefined && (typeof job[key] !== "string" || !job[key]))
        throw new Error(`Invalid reviewer ${key}`);
    if (job.ephemeral !== undefined && typeof job.ephemeral !== "boolean")
      throw new Error("ephemeral must be a boolean");
    const output = resolve(job.output);
    mkdirSync(dirname(output), { recursive: true, mode: 0o700 });
    return {
      ...job,
      prompt: resolve(job.prompt),
      output: join(realpathSync(dirname(output)), basename(output)),
    };
  });
  const destinations = jobs.flatMap((job) => [
    job.output,
    `${job.output}.events.jsonl`,
    `${job.output}.receipt.json`,
  ]);
  if (new Set(destinations).size !== destinations.length)
    throw new Error("Reviewer artifact destinations overlap");
  const restrictions = parseRestrictions(panel.restrictions);
  if (jobs.some((job) => restrictions.forbiddenModels?.includes(job.model)))
    throw new Error(
      "A requested reviewer model is forbidden by the panel restrictions",
    );
  if (
    panel.preflight !== undefined &&
    (typeof panel.preflight !== "string" || !panel.preflight)
  )
    throw new Error("preflight must name a project script");
  return {
    cwd,
    jobs,
    preflight: panel.preflight
      ? realpathSync(resolve(panel.preflight))
      : undefined,
    restrictions,
  };
}

function reserve(jobs) {
  const owned = [];
  const verify = () => {
    for (const item of owned) {
      const current = lstatSync(item.path);
      const owner = JSON.parse(
        readFileSync(join(item.path, "owner.json"), "utf8"),
      );
      if (
        !current.isDirectory() ||
        current.ino !== item.ino ||
        current.dev !== item.dev ||
        owner.token !== item.token
      )
        throw new Error(`Destination reservation was replaced: ${item.path}`);
    }
  };
  const release = () => {
    verify();
    for (const item of owned.reverse()) {
      unlinkSync(join(item.path, "owner.json"));
      rmdirSync(item.path);
    }
  };
  try {
    for (const job of jobs)
      for (const destination of [
        job.output,
        `${job.output}.events.jsonl`,
        `${job.output}.receipt.json`,
      ]) {
        if (existsSync(destination) && !lstatSync(destination).isFile())
          throw new Error(
            `Artifact destination is not a regular file: ${destination}`,
          );
        const path = `${destination}.pstack-lock`;
        try {
          mkdirSync(path, { mode: 0o700 });
        } catch {
          throw new Error(`Artifact destination is reserved: ${destination}`);
        }
        const { ino, dev } = lstatSync(path);
        const token = randomUUID();
        writeFileSync(
          join(path, "owner.json"),
          JSON.stringify({
            token,
            pid: process.pid,
            createdAt: new Date().toISOString(),
            destination,
          }),
          { mode: 0o600 },
        );
        owned.push({ path, ino, dev, token });
      }
  } catch (error) {
    release();
    throw error;
  }
  return { release, verify };
}

function rolePrompt(job, cwd) {
  const scope = readFileSync(job.prompt, "utf8");
  if (!job.role) return { prompt: scope };
  if (!/^[a-z0-9][a-z0-9-]*$/.test(job.role))
    throw new Error("Invalid native role name");
  const library = resolve(
    dirname(fileURLToPath(import.meta.url)),
    "../../.codex/agents",
  );
  const home = process.env.CODEX_HOME || join(homedir(), ".codex");
  const path = [
    join(cwd, ".codex/agents", `${job.role}.toml`),
    join(library, `${job.role}.toml`),
    join(home, "agents", `${job.role}.toml`),
  ].find(existsSync);
  if (!path) throw new Error(`Native role not found: ${job.role}`);
  const definition = Bun.TOML.parse(readFileSync(path, "utf8"));
  if (
    definition.name !== job.role ||
    typeof definition.developer_instructions !== "string"
  )
    throw new Error(`Invalid native role: ${job.role}`);
  return {
    instructions: definition.developer_instructions,
    prompt: `Native agent role: ${job.role}\n\n${definition.developer_instructions}\n\nParent scope:\n${scope}`,
  };
}

function readReport(path) {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const info = fstatSync(fd);
    if (!info.isFile() || !info.size || info.size > 16 * 1024 * 1024)
      throw new Error("Native report is missing, empty, or oversized");
    return readFileSync(fd, "utf8");
  } finally {
    closeSync(fd);
  }
}

async function review(
  job,
  panel,
  preparation,
  active,
  interrupted,
  verifyReservation,
) {
  const directory = mkdtempSync(join(dirname(job.output), ".pstack-review-"));
  chmodSync(directory, 0o700);
  const reportPath = join(directory, "report.md");
  const events = join(directory, "events.jsonl");
  const receiptPath = join(directory, "receipt.json");
  writeFileSync(events, "", { mode: 0o600 });
  const { prompt, instructions } = rolePrompt(job, panel.cwd);
  const command = [
    "codex",
    "exec",
    "--sandbox",
    "read-only",
    "--json",
    "-C",
    panel.cwd,
    "--model",
    job.model,
    "--output-last-message",
    reportPath,
    "-c",
    "allow_login_shell=false",
  ];
  if (job.effort)
    command.push("-c", `model_reasoning_effort=${JSON.stringify(job.effort)}`);
  if (instructions)
    command.push(
      "-c",
      `developer_instructions=${JSON.stringify(instructions)}`,
    );
  if (job.ephemeral) command.push("--ephemeral");
  command.push("-");
  const startedAt = new Date().toISOString();
  const child = Bun.spawn(command, {
    cwd: panel.cwd,
    env: process.env,
    stdin: new TextEncoder().encode(prompt),
    stdout: Bun.file(events),
    stderr: "inherit",
  });
  active.add(child);
  const code = await child.exited;
  active.delete(child);
  const endedAt = new Date().toISOString();
  try {
    if (code !== 0 || interrupted())
      throw new Error(`Native read-only session failed with exit ${code}`);
    if (!existsSync(reportPath))
      throw new Error("Native session returned no fresh report");
    const report = readReport(reportPath);
    const ids = new Set();
    if (statSync(events).size > 64 * 1024 * 1024)
      throw new Error("Native events exceed the identity-inspection bound");
    const eventText = readFileSync(events, "utf8");
    for (const line of eventText.split("\n").filter(Boolean)) {
      let event;
      try {
        event = JSON.parse(line);
      } catch {
        throw new Error("Malformed native event JSONL");
      }
      if (
        event.type === "thread.started" &&
        typeof event.thread_id === "string"
      )
        ids.add(event.thread_id);
    }
    if (ids.size !== 1)
      throw new Error(
        "Native events identify no unique canonical review thread",
      );
    const threadId = [...ids][0];
    let receipt;
    const deadline = Date.now() + 2000;
    do {
      const snapshot = collectExecution({ cwd: panel.cwd, threadId });
      writeFileSync(
        join(directory, "history-diagnostics.json"),
        JSON.stringify(
          {
            inventoryIssues: snapshot.inventoryIssues,
            historicalWarnings: snapshot.inventoryWarnings,
            threads: snapshot.threads.map((thread) => ({
              id: thread.id,
              issues: thread.issues,
            })),
          },
          null,
          2,
        ) + "\n",
        { mode: 0o600 },
      );
      receipt = executionReceipt(snapshot, {
        cwd: panel.cwd,
        threadId,
        requested: { model: job.model, effort: job.effort },
        report,
        restrictions: panel.restrictions,
        startedAt,
        endedAt,
      });
      if (
        receipt.status !== "inconclusive" ||
        job.ephemeral ||
        Date.now() >= deadline
      )
        break;
      await Bun.sleep(100);
    } while (!interrupted());
    receipt.preparation = preparation;
    const receiptText = JSON.stringify(receipt, null, 2) + "\n";
    writeFileSync(receiptPath, receiptText, { mode: 0o600 });
    if (receipt.status !== "verified") {
      const reasons = Object.values(receipt.checks)
        .filter((check) => ["inconclusive", "violated"].includes(check.status))
        .map((check) => check.reason);
      for (const claim of [
        receipt.coverage.taskLineage,
        receipt.coverage.effectiveModels,
      ])
        if (claim.status === "inconclusive") reasons.push(claim.reason);
      throw new Error(
        `Canonical receipt ${receipt.status}: ${reasons.join("; ")}`,
      );
    }
    for (const [content, destination] of [
      [report, job.output],
      [eventText, `${job.output}.events.jsonl`],
      [receiptText, `${job.output}.receipt.json`],
    ]) {
      verifyReservation();
      const temporary = join(directory, `publish-${basename(destination)}`);
      writeFileSync(temporary, content, { mode: 0o600 });
      renameSync(temporary, destination);
    }
    return report;
  } catch (error) {
    if (!existsSync(receiptPath))
      writeFileSync(
        receiptPath,
        JSON.stringify(
          {
            version: 1,
            status: "inconclusive",
            reason: error.message,
            preparation,
            launchedAt: startedAt,
            launcherExitedAt: endedAt,
          },
          null,
          2,
        ) + "\n",
        { mode: 0o600 },
      );
    throw new Error(`${error.message}; retained evidence: ${directory}`);
  }
}

async function main() {
  if (process.argv.includes("--help")) {
    console.log(
      "pstack-readonly.mjs --cwd PATH --prompt FILE --output FILE --model MODEL [--effort EFFORT] [--role NAME] [--preflight SCRIPT] [--restrictions JSON_FILE] [--ephemeral]\npstack-readonly.mjs --panel MANIFEST\nRun prepared panels inside: devenv shell -- bun .agents/scripts/pstack-readonly.mjs --panel MANIFEST\nReports require canonical native history. Receipts and JSONL events are written beside each report. Ephemeral history is inconclusive.",
    );
    return;
  }
  const panel = normalize(parse(process.argv.slice(2)));
  const reservation = reserve(panel.jobs);
  const active = new Set();
  const controller = new AbortController();
  let interrupted = false;
  let killTimer;
  const interrupt = () => {
    interrupted = true;
    controller.abort();
    for (const child of active) child.kill("SIGTERM");
    killTimer ??= setTimeout(() => {
      for (const child of active) child.kill("SIGKILL");
    }, 5000);
  };
  process.on("SIGINT", interrupt);
  process.on("SIGTERM", interrupt);
  let reports;
  try {
    let preparation = { status: "not-requested" };
    if (panel.preflight) {
      const directory = mkdtempSync(
        join(dirname(panel.jobs[0].output), ".pstack-preflight-"),
      );
      chmodSync(directory, 0o700);
      try {
        preparation = await runPreflight({
          cwd: panel.cwd,
          script: panel.preflight,
          log: join(directory, "server.stderr"),
          signal: controller.signal,
        });
        preparation = {
          ...preparation,
          scriptSha256: createHash("sha256")
            .update(readFileSync(panel.preflight))
            .digest("hex"),
          completedAt: new Date().toISOString(),
        };
        writeFileSync(
          join(directory, "result.json"),
          JSON.stringify(preparation, null, 2) + "\n",
          { mode: 0o600 },
        );
      } catch (error) {
        throw new Error(
          `${error.message}; retained preflight evidence: ${directory}`,
        );
      }
    }
    if (interrupted)
      throw new Error("Reviewer panel interrupted before launch");
    const results = await Promise.allSettled(
      panel.jobs.map((job) =>
        review(
          job,
          panel,
          preparation,
          active,
          () => interrupted,
          reservation.verify,
        ),
      ),
    );
    const errors = results.filter((result) => result.status === "rejected");
    if (errors.length)
      throw new Error(errors.map((result) => result.reason.message).join("\n"));
    reports = results.map((result) => result.value).join("\n");
  } finally {
    clearTimeout(killTimer);
    process.off("SIGINT", interrupt);
    process.off("SIGTERM", interrupt);
    reservation.release();
  }
  process.stdout.write(reports);
}

if (import.meta.main) {
  try {
    await main();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
