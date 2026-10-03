import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";

async function main() {
  const args = process.argv.slice(2);
  if (args.includes("--help")) {
    process.stdout.write(
      "pstack-readonly.mjs --cwd PATH --prompt FILE --output FILE --model MODEL [--effort EFFORT] [--role NAME] [--ephemeral]\nRuns a native Codex read-only session. Output is the original final report; detailed events go to FILE.events.jsonl.\n",
    );
    return;
  }
  const options = {};
  for (let index = 0; index < args.length; index += 1) {
    const key = args[index];
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
      ].includes(key) ||
      args[index + 1] === undefined ||
      options[key] !== undefined
    )
      throw new Error(`Invalid option: ${key}`);
    options[key] = args[++index];
  }
  for (const key of ["--cwd", "--prompt", "--output", "--model"])
    if (!options[key]) throw new Error(`Required option: ${key}`);
  const cwd = resolve(options["--cwd"]);
  const output = resolve(options["--output"]);
  const events = `${output}.events.jsonl`;
  const command = [
    "codex",
    "exec",
    "--sandbox",
    "read-only",
    "--json",
    "-C",
    cwd,
    "--model",
    options["--model"],
    "--output-last-message",
    output,
  ];
  let roleInstructions;
  if (options["--effort"])
    command.push(
      "-c",
      `model_reasoning_effort=${JSON.stringify(options["--effort"])}`,
    );
  if (options["--ephemeral"]) command.push("--ephemeral");
  if (options["--role"]) {
    const role = options["--role"];
    if (!/^[a-z0-9][a-z0-9-]*$/.test(role))
      throw new Error("Invalid native role name.");
    const library = resolve(
      dirname(fileURLToPath(import.meta.url)),
      "../../.codex/agents",
    );
    const codexHome = process.env.CODEX_HOME || join(homedir(), ".codex");
    const candidates = [
      join(cwd, ".codex/agents", `${role}.toml`),
      join(library, `${role}.toml`),
      join(codexHome, "agents", `${role}.toml`),
    ];
    const file = candidates.find(existsSync);
    if (!file) throw new Error(`Native role not found: ${role}`);
    const definition = Bun.TOML.parse(readFileSync(file, "utf8"));
    if (
      definition.name !== role ||
      typeof definition.developer_instructions !== "string"
    )
      throw new Error(`Invalid native role: ${role}`);
    roleInstructions = definition.developer_instructions;
    command.push(
      "-c",
      `developer_instructions=${JSON.stringify(definition.developer_instructions)}`,
    );
  }
  command.push("-");
  const scope = readFileSync(resolve(options["--prompt"]), "utf8");
  const prompt = roleInstructions
    ? `Native agent role: ${options["--role"]}\n\n${roleInstructions}\n\nParent scope:\n${scope}`
    : scope;
  mkdirSync(dirname(output), { recursive: true, mode: 0o700 });
  writeFileSync(events, "", { mode: 0o600 });
  const child = Bun.spawn(command, {
    stdin: new TextEncoder().encode(prompt),
    stdout: Bun.file(events),
    stderr: "inherit",
  });
  const interrupt = () => child.kill("SIGTERM");
  process.on("SIGINT", interrupt);
  process.on("SIGTERM", interrupt);
  const exitCode = await child.exited;
  process.off("SIGINT", interrupt);
  process.off("SIGTERM", interrupt);
  if (exitCode !== 0)
    throw new Error(
      `Native read-only session failed with exit ${exitCode}; events: ${events}`,
    );
  if (!existsSync(output))
    throw new Error(
      `Native session returned no final report; events: ${events}`,
    );
  chmodSync(output, 0o600);
  process.stdout.write(readFileSync(output, "utf8"));
}

try {
  await main();
} catch (error) {
  process.stderr.write(
    `${error instanceof Error ? error.message : String(error)}\n`,
  );
  process.exitCode = 1;
}
