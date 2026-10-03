import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const reminder =
  "New task? Playbook match or rigor needed -> apply $poteto-mode. Casual turn or user opts out -> don't.";
const sessionPattern = /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/;
const activationPattern =
  /^\s*(?:\$poteto-mode(?:\s|$)|(?:use|enable|activate|enter|apply|switch to|work in)\s+(?:the\s+)?(?:poteto[ -]mode|poteto(?:'s)?(?: agent)? style|poteto\b))/i;
const optOutPattern =
  /^\s*(?:please\s+)?(?:(?:stop|quit|exit|leave|disable|deactivate)\s+(?:using\s+)?(?:the\s+)?poteto(?:[ -]mode)?\b|(?:don't|do not)\s+(?:use|apply)\s+(?:the\s+)?poteto(?:[ -]mode)?\b|turn\s+off\s+(?:the\s+)?poteto(?:[ -]mode)?\b|(?:opt out|back to normal)\s*[.!]?\s*$)/i;

function statePath(session, stateDir) {
  if (typeof session !== "string" || !sessionPattern.test(session))
    throw new Error("A native session ID is required.");
  return join(stateDir, `${session}.json`);
}

function readState(path) {
  if (!existsSync(path)) return false;
  const state = JSON.parse(readFileSync(path, "utf8"));
  if (typeof state.active !== "boolean")
    throw new Error("Invalid pstack mode state.");
  return state.active;
}

export function setMode(
  session,
  active,
  stateDir = join(projectRoot, ".pstack/state"),
) {
  const path = statePath(session, stateDir);
  if (!active) {
    if (existsSync(path)) rmSync(path, { force: true });
    return;
  }
  if (readState(path)) return;
  mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  const temporary = `${path}.${process.pid}.tmp`;
  writeFileSync(temporary, JSON.stringify({ active: true }), { mode: 0o600 });
  renameSync(temporary, path);
}

export function handleHook(
  input,
  stateDir = join(projectRoot, ".pstack/state"),
) {
  if (input === null || typeof input !== "object")
    throw new Error("Hook input must be an object.");
  const event = input.hook_event_name;
  if (event !== "SessionStart" && event !== "UserPromptSubmit") return {};
  const path = statePath(input.session_id, stateDir);
  if (event === "SessionStart" && input.source === "clear") {
    setMode(input.session_id, false, stateDir);
    return {};
  }
  if (
    event === "UserPromptSubmit" &&
    typeof input.prompt === "string" &&
    optOutPattern.test(input.prompt)
  ) {
    setMode(input.session_id, false, stateDir);
    return {};
  }
  if (
    event === "UserPromptSubmit" &&
    typeof input.prompt === "string" &&
    activationPattern.test(input.prompt)
  )
    setMode(input.session_id, true, stateDir);
  if (!readState(path)) return {};
  return {
    hookSpecificOutput: { hookEventName: event, additionalContext: reminder },
  };
}

async function main() {
  const [command, ...args] = process.argv.slice(2);
  if (command === "--help" || command === "help") {
    process.stdout.write(
      "pstack-mode.mjs [activate|deactivate|status] [--session ID] [--cwd PATH]\nNo arguments: process Codex hook JSON from stdin. Session defaults to CODEX_THREAD_ID.\n",
    );
    return;
  }
  if (!command) {
    const input = JSON.parse(await Bun.stdin.text());
    process.stdout.write(`${JSON.stringify(handleHook(input))}\n`);
    return;
  }
  if (!["activate", "deactivate", "status"].includes(command))
    throw new Error(`Unknown operation: ${command}`);
  let session = process.env.CODEX_THREAD_ID;
  for (let index = 0; index < args.length; index += 2) {
    if (!args[index + 1]) throw new Error(`Missing value for ${args[index]}`);
    if (args[index] === "--session") session = args[index + 1];
    else if (args[index] === "--cwd") {
      const cwd = resolve(args[index + 1]);
      if (cwd !== projectRoot && !cwd.startsWith(`${projectRoot}/`))
        throw new Error("Mode state belongs to this checkout.");
    } else throw new Error(`Unknown option: ${args[index]}`);
  }
  const path = statePath(session, join(projectRoot, ".pstack/state"));
  if (command !== "status") setMode(session, command === "activate");
  process.stdout.write(
    `${JSON.stringify({ session, active: readState(path) })}\n`,
  );
}

if (import.meta.main) {
  try {
    await main();
  } catch (error) {
    process.stderr.write(
      `${error instanceof Error ? error.message : String(error)}\n`,
    );
    process.exitCode = 1;
  }
}
