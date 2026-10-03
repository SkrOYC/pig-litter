#!/usr/bin/env bun
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import {
  access,
  mkdir,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { TOML } from "bun";
import { Database } from "bun:sqlite";

export const BUDGETS = [
  "unlimited — keep max",
  "large — xhigh reasoning",
  "medium — high reasoning",
  "small — medium reasoning",
];

const CODE = { model: "gpt-6-luna", effort: "xhigh" };
const JUDGMENT = { model: "gpt-6-astra", effort: "max" };
const GPT = { model: "gpt-6.1-sol", effort: "max" };
const PANEL = [JUDGMENT, GPT, CODE];

export const ROLE_DEFAULTS = {
  "feature, refactoring": CODE,
  "bug-fix": CODE,
  "perf-issue": CODE,
  hillclimb: CODE,
  "judgment and prose": JUDGMENT,
  "hardest tasks": JUDGMENT,
  "how explorer": CODE,
  "how explainer": JUDGMENT,
  "why investigators": CODE,
  "why synthesizer": JUDGMENT,
  "reflect tooling": GPT,
  "reflect judgment, divergent, synthesizer": JUDGMENT,
  "arena runners": PANEL,
  "arena cross-judge pool": PANEL,
  "swarm workers": CODE,
  "architect runners": PANEL,
  "interrogate reviewers": PANEL,
};

export const PANEL_ROLES = new Set([
  "arena runners",
  "arena cross-judge pool",
  "architect runners",
  "interrogate reviewers",
]);

function isKnownAgentFile(name) {
  for (const label of Object.keys(ROLE_DEFAULTS)) {
    if (PANEL_ROLES.has(label)) {
      const match = new RegExp(`^${agentName(label)}-(\\d+)\\.toml$`).exec(
        name,
      );
      if (match && Number(match[1]) > 0) return true;
    } else {
      if (name === `${agentName(label)}.toml`) return true;
    }
  }
  return false;
}

const EFFORT_ORDER = ["low", "medium", "high", "xhigh", "max", "ultra"];
const BUDGET_EFFORT = new Map([
  [BUDGETS[0], null],
  [BUDGETS[1], "xhigh"],
  [BUDGETS[2], "high"],
  [BUDGETS[3], "medium"],
]);
const ALIASES = new Set(["inherit-parent", "auto"]);
function roleId(label) {
  return label
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

function agentName(label, seat) {
  return `pstack-${roleId(label)}${seat === undefined ? "" : `-${String(seat).padStart(2, "0")}`}`;
}

async function projectRoot(cwd) {
  let directory = resolve(cwd);
  let nearestCodexRoot = null;
  while (true) {
    try {
      await access(join(directory, ".git"));
      return directory;
    } catch {}
    try {
      await access(join(directory, ".codex"));
      nearestCodexRoot ??= directory;
    } catch {}
    const parent = dirname(directory);
    if (parent === directory) return nearestCodexRoot ?? resolve(cwd);
    directory = parent;
  }
}

async function pathsFor(scope, cwd, codexHome) {
  if (scope === "project")
    return join(await projectRoot(cwd), ".codex", "agents");
  if (scope === "user") return join(codexHome, "agents");
  throw new Error(`Unknown scope '${scope}'. Use project or user.`);
}

export function getCodexHome(env = process.env) {
  return resolve(env.CODEX_HOME || join(homedir(), ".codex"));
}

export async function readModelCatalog({ codexHome = getCodexHome() } = {}) {
  const path = join(codexHome, "models_cache.json");
  let raw;
  try {
    raw = JSON.parse(await readFile(path, "utf8"));
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error(
        `Codex model catalog not found at ${path}; refresh or provide a runtime-verified catalog before configuring models.`,
      );
    }
    throw new Error(
      `Cannot read Codex model catalog at ${path}: ${error.message}`,
    );
  }
  if (!raw || !Array.isArray(raw.models)) {
    throw new Error(
      `Unsupported Codex model catalog schema at ${path}: expected an object with a models array.`,
    );
  }
  const models = [];
  const metadataModels = [];
  for (const entry of raw.models) {
    if (
      typeof entry?.slug !== "string" ||
      !Array.isArray(entry.supported_reasoning_levels)
    )
      continue;
    if (entry.supported_in_api !== true) continue;
    const efforts = entry.supported_reasoning_levels
      .map((level) => level?.effort)
      .filter((effort) => EFFORT_ORDER.includes(effort));
    const defaultEffort = efforts.includes(entry.default_reasoning_level)
      ? entry.default_reasoning_level
      : null;
    const model = { model: entry.slug, efforts, defaultEffort };
    metadataModels.push(model);
    if (entry.visibility === "list") models.push(model);
  }
  return {
    source: path,
    fetchedAt: raw.fetched_at ?? null,
    clientVersion: raw.client_version ?? null,
    models,
    metadataModels,
  };
}

export async function readActiveParent({
  codexHome = getCodexHome(),
  threadId = process.env.CODEX_THREAD_ID,
  catalog,
} = {}) {
  if (typeof threadId !== "string" || !threadId.trim()) {
    throw new Error(
      "Cannot resolve the active parent model: CODEX_THREAD_ID is not set. Supply the active thread ID through that variable.",
    );
  }
  const files = await readdir(codexHome).catch((error) => {
    if (error.code === "ENOENT")
      throw new Error(`Codex state directory not found at ${codexHome}.`);
    throw error;
  });
  const databases = files.filter((name) => /^state_.*\.sqlite$/.test(name));
  let compatibleSchema = false;
  for (const name of databases) {
    const path = join(codexHome, name);
    let db;
    try {
      db = new Database(path, { readonly: true });
      const exists = db
        .query(
          "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'threads'",
        )
        .get();
      if (!exists) continue;
      const columns = db
        .query('PRAGMA table_info("threads")')
        .all()
        .map((column) => column.name);
      if (
        !["id", "model", "reasoning_effort"].every((column) =>
          columns.includes(column),
        )
      )
        continue;
      compatibleSchema = true;
      const row = db
        .query(
          "SELECT model, reasoning_effort FROM threads WHERE id = ? LIMIT 1",
        )
        .get(threadId);
      if (!row) continue;
      if (typeof row.model !== "string" || !row.model.trim()) {
        throw new Error(
          `Active parent thread ${threadId} has no model value in the Codex state database.`,
        );
      }
      if (
        typeof row.reasoning_effort === "string" &&
        row.reasoning_effort.trim()
      ) {
        return {
          threadId,
          model: row.model,
          effort: row.reasoning_effort,
          effortSource: "thread",
        };
      }
      if (!catalog) catalog = await readModelCatalog({ codexHome });
      const model = [...catalog.models, ...(catalog.metadataModels ?? [])].find(
        (entry) => entry.model === row.model,
      );
      if (
        !model?.defaultEffort ||
        !model.efforts.includes(model.defaultEffort)
      ) {
        throw new Error(
          `Active parent thread ${threadId} omits reasoning effort, and no verified catalog default exists for '${row.model}'.`,
        );
      }
      return {
        threadId,
        model: row.model,
        effort: model.defaultEffort,
        effortSource: "verified-catalog-default",
      };
    } finally {
      db?.close();
    }
  }
  if (!compatibleSchema) {
    throw new Error(
      `Cannot resolve the active parent model: no state database under ${codexHome} exposes threads.id, threads.model, and threads.reasoning_effort.`,
    );
  }
  throw new Error(
    `Active parent thread ${threadId} was not found in a compatible Codex state database under ${codexHome}.`,
  );
}

export function effortForBudget(model, effort, budget, catalog) {
  if (!BUDGET_EFFORT.has(budget))
    throw new Error(`Unknown budget '${budget}'.`);
  const target = BUDGET_EFFORT.get(budget);
  if (target === null) return effort;
  const entry = catalog.models.find((candidate) => candidate.model === model);
  if (!entry)
    throw new Error(
      `Model '${model}' is not in the verified Codex model catalog.`,
    );
  const targetIndex = EFFORT_ORDER.indexOf(target);
  const supported = entry.efforts.filter(
    (item) => EFFORT_ORDER.indexOf(item) <= targetIndex,
  );
  if (!supported.length) {
    throw new Error(
      `Model '${model}' has no supported reasoning effort at or below '${target}'. Choose another model or budget.`,
    );
  }
  return supported.reduce((best, item) =>
    EFFORT_ORDER.indexOf(item) > EFFORT_ORDER.indexOf(best) ? item : best,
  );
}

function normalizeChoice(value, { catalog, budget, validate = true } = {}) {
  if (typeof value === "string" && ALIASES.has(value)) return { choice: value };
  if (
    !value ||
    typeof value !== "object" ||
    typeof value.model !== "string" ||
    typeof value.effort !== "string"
  ) {
    throw new Error(
      "A model choice must be 'auto', 'inherit-parent', or an object with model and effort strings.",
    );
  }
  if (validate) {
    const found = catalog.models.find((item) => item.model === value.model);
    if (!found)
      throw new Error(
        `Model '${value.model}' is not in the verified Codex model catalog.`,
      );
    if (!found.efforts.includes(value.effort))
      throw new Error(
        `Effort '${value.effort}' is not supported by model '${value.model}'.`,
      );
  }
  const effort = budget
    ? effortForBudget(value.model, value.effort, budget, catalog)
    : value.effort;
  return { model: value.model, effort };
}

function parseMarker(instructions) {
  const match = /^Pstack setup choice: (auto|inherit-parent)\.$/m.exec(
    instructions ?? "",
  );
  return match?.[1] ?? null;
}

async function readAgentFile(path, expectedName) {
  let text;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
  let data;
  try {
    data = TOML.parse(text);
  } catch (error) {
    throw new Error(`Invalid TOML in ${path}: ${error.message}`);
  }
  if (
    data.name !== expectedName ||
    typeof data.description !== "string" ||
    typeof data.developer_instructions !== "string"
  ) {
    throw new Error(
      `Invalid pstack agent file ${path}: required native custom-agent fields are missing or do not match its filename.`,
    );
  }
  const alias = parseMarker(data.developer_instructions);
  if (alias) {
    if (data.model !== undefined || data.model_reasoning_effort !== undefined) {
      throw new Error(
        `Invalid pstack alias entry ${path}: an inherited-model choice cannot set model or effort.`,
      );
    }
    return { choice: alias, budget: budgetFrom(text), path };
  }
  if (data.model === undefined && data.model_reasoning_effort === undefined) {
    return { choice: "inherit-parent", budget: budgetFrom(text), path };
  }
  if (
    typeof data.model !== "string" ||
    typeof data.model_reasoning_effort !== "string"
  ) {
    throw new Error(
      `Invalid pstack model entry ${path}: model and model_reasoning_effort must both be strings.`,
    );
  }
  return {
    model: data.model,
    effort: data.model_reasoning_effort,
    budget: budgetFrom(text),
    path,
  };
}

function budgetFrom(text) {
  const match = /^# pstack budget: (.+)$/m.exec(text);
  return match?.[1] ?? null;
}

async function roleEntries(scope, label, { cwd, codexHome }) {
  const directory = await pathsFor(scope, cwd, codexHome);
  if (!PANEL_ROLES.has(label)) {
    const entry = await readAgentFile(
      join(directory, `${agentName(label)}.toml`),
      agentName(label),
    );
    return entry ? [entry] : null;
  }
  const names = await readdir(directory).catch((error) =>
    error.code === "ENOENT" ? [] : Promise.reject(error),
  );
  const expression = new RegExp(`^${agentName(label)}-(\\d+)\\.toml$`);
  const seatFiles = names
    .map((name) => ({ name, seat: expression.exec(name)?.[1] }))
    .filter((entry) => entry.seat !== undefined)
    .sort((a, b) => Number(a.seat) - Number(b.seat));
  if (!seatFiles.length) return null;
  const entries = [];
  for (const file of seatFiles) {
    entries.push(
      await readAgentFile(
        join(directory, file.name),
        `${agentName(label)}-${file.seat}`,
      ),
    );
  }
  return entries;
}

async function retiredAgentFiles(scope, { cwd, codexHome }) {
  const directory = await pathsFor(scope, cwd, codexHome);
  const names = await readdir(directory).catch((error) =>
    error.code === "ENOENT" ? [] : Promise.reject(error),
  );
  return names
    .filter(
      (name) =>
        name.startsWith("pstack-") &&
        name.endsWith(".toml") &&
        !isKnownAgentFile(name),
    )
    .sort()
    .map((file) => ({ scope, file }));
}

async function effectiveRole(label, { cwd, codexHome }) {
  for (const scope of ["project", "user"]) {
    const entries = await roleEntries(scope, label, { cwd, codexHome });
    if (entries) return { entries, scope, overridden: true };
  }
  const defaults = ROLE_DEFAULTS[label];
  const values = Array.isArray(defaults) ? defaults : [defaults];
  return {
    entries: values.map((value) => ({ ...value, budget: null, path: null })),
    scope: "fallback",
    overridden: false,
  };
}

export async function show({
  cwd = process.cwd(),
  codexHome = getCodexHome(),
} = {}) {
  const roles = {};
  const budgets = new Set();
  for (const label of Object.keys(ROLE_DEFAULTS)) {
    const resolved = await effectiveRole(label, { cwd, codexHome });
    roles[label] = {
      scope: resolved.scope,
      choices: resolved.entries.map(
        ({ path: _path, budget: entryBudget, ...entry }) => {
          if (entryBudget) budgets.add(entryBudget);
          return entry;
        },
      ),
    };
  }
  return {
    budget: budgets.size === 1 ? [...budgets][0] : null,
    budgets: [...budgets],
    roles,
    retiredFiles: [
      ...(await retiredAgentFiles("project", { cwd, codexHome })),
      ...(await retiredAgentFiles("user", { cwd, codexHome })),
    ],
  };
}

export async function resolveRole(
  label,
  { cwd = process.cwd(), codexHome = getCodexHome() } = {},
) {
  if (!Object.hasOwn(ROLE_DEFAULTS, label))
    throw new Error(`Unknown pstack role '${label}'.`);
  const resolved = await effectiveRole(label, { cwd, codexHome });
  return {
    role: label,
    scope: resolved.scope,
    panel: PANEL_ROLES.has(label),
    choices: resolved.entries.map(({ path: _path, ...entry }) => entry),
  };
}

function parseWriteInput(raw) {
  let input;
  try {
    input = JSON.parse(raw);
  } catch (error) {
    throw new Error(`Cannot parse --input JSON: ${error.message}`);
  }
  if (
    !input ||
    typeof input !== "object" ||
    Array.isArray(input) ||
    !input.roles ||
    typeof input.roles !== "object" ||
    Array.isArray(input.roles)
  ) {
    throw new Error("--input must be a JSON object with a roles object.");
  }
  if (input.budget !== undefined && !BUDGET_EFFORT.has(input.budget))
    throw new Error(`Unknown budget '${input.budget}'.`);
  for (const label of Object.keys(input.roles)) {
    if (!Object.hasOwn(ROLE_DEFAULTS, label))
      throw new Error(`Unknown pstack role '${label}'.`);
  }
  return input;
}

function addVerifiedPairs(catalog, pairs) {
  if (pairs === undefined) return catalog;
  if (!Array.isArray(pairs))
    throw new Error(
      "verifiedPairs must be an array of model/effort pairs with evidence.",
    );
  const merged = new Map(
    catalog.models.map((item) => [item.model, new Set(item.efforts)]),
  );
  for (const pair of pairs) {
    if (
      typeof pair?.model !== "string" ||
      !EFFORT_ORDER.includes(pair.effort) ||
      typeof pair.evidence !== "string" ||
      !pair.evidence.trim()
    ) {
      throw new Error(
        "Each verifiedPairs entry must include model, supported effort, and non-empty evidence.",
      );
    }
    const efforts = merged.get(pair.model) ?? new Set();
    efforts.add(pair.effort);
    merged.set(pair.model, efforts);
  }
  return {
    ...catalog,
    models: [...merged].map(([model, efforts]) => ({
      model,
      efforts: [...efforts],
    })),
  };
}

function instructionsFor(label, seat, choice) {
  const seatText = seat === undefined ? "" : ` (seat ${seat})`;
  const aliasText = choice.choice
    ? `\nPstack setup choice: ${choice.choice}.`
    : "";
  return `Pstack role: ${label}${seatText}. Follow the role instructions supplied by the invoking workflow.${aliasText}`;
}

function serializeAgent(label, seat, choice, budget) {
  const name = agentName(label, seat);
  const seatText = seat === undefined ? "" : ` seat ${seat}`;
  const data = {
    name,
    description: `Pstack ${label}${seatText} model role.`,
    developer_instructions: instructionsFor(label, seat, choice),
  };
  if (choice.model !== undefined) {
    data.model = choice.model;
    data.model_reasoning_effort = choice.effort;
  }
  return `# pstack budget: ${budget ?? BUDGETS[0]}\n${TOML.stringify(data)}`;
}

async function existingSeats(directory, label) {
  const expression = new RegExp(`^${agentName(label)}-(\\d+)\\.toml$`);
  const names = await readdir(directory).catch((error) =>
    error.code === "ENOENT" ? [] : Promise.reject(error),
  );
  return names
    .map((name) => ({ name, seat: expression.exec(name)?.[1] }))
    .filter((item) => item.seat !== undefined);
}

/** Write only the explicitly requested scope. Null removes that role's own override. */
export async function writeConfiguration({
  scope,
  input,
  cwd = process.cwd(),
  codexHome = getCodexHome(),
  catalog,
} = {}) {
  if (!catalog) catalog = await readModelCatalog({ codexHome });
  const parsed = typeof input === "string" ? parseWriteInput(input) : input;
  if (
    !parsed ||
    !parsed.roles ||
    typeof parsed.roles !== "object" ||
    Array.isArray(parsed.roles)
  ) {
    throw new Error("Input must contain a roles object.");
  }
  if (parsed.budget !== undefined && !BUDGET_EFFORT.has(parsed.budget))
    throw new Error(`Unknown budget '${parsed.budget}'.`);
  catalog = addVerifiedPairs(catalog, parsed.verifiedPairs);
  const directory = await pathsFor(scope, cwd, codexHome);
  if (parsed.retiredFiles !== undefined && !Array.isArray(parsed.retiredFiles))
    throw new Error(
      "retiredFiles must be an array of retired pstack TOML basenames.",
    );
  const retiredFiles = parsed.retiredFiles ?? [];
  for (const file of retiredFiles) {
    if (
      typeof file !== "string" ||
      !/^pstack-[a-z0-9-]+\.toml$/.test(file) ||
      isKnownAgentFile(file)
    ) {
      throw new Error(
        `Refusing to remove non-retired or non-pstack agent file '${file}'.`,
      );
    }
  }
  const actions = [];
  for (const [label, rawValue] of Object.entries(parsed.roles)) {
    if (!Object.hasOwn(ROLE_DEFAULTS, label))
      throw new Error(`Unknown pstack role '${label}'.`);
    if (rawValue === null) {
      actions.push({ label, remove: true });
      continue;
    }
    const panel = PANEL_ROLES.has(label);
    if (panel !== Array.isArray(rawValue)) {
      throw new Error(
        `Role '${label}' must be ${panel ? "an array of panel-seat choices" : "one model choice"}.`,
      );
    }
    const values = panel ? rawValue : [rawValue];
    const choices = values.map((value) =>
      normalizeChoice(value, {
        catalog,
        budget: parsed.budget ?? BUDGETS[0],
      }),
    );
    actions.push({ label, panel, choices });
  }
  const written = [];
  const removed = [];
  for (const action of actions) {
    const { label } = action;
    if (action.remove) {
      const files = PANEL_ROLES.has(label)
        ? (await existingSeats(directory, label)).map((item) => item.name)
        : [`${agentName(label)}.toml`];
      for (const file of files) {
        await rm(join(directory, file), { force: true });
        removed.push(file);
      }
      continue;
    }
    const { panel, choices } = action;
    await mkdir(directory, { recursive: true });
    for (let index = 0; index < choices.length; index += 1) {
      const seat = panel ? index + 1 : undefined;
      const name = `${agentName(label, seat)}.toml`;
      await writeFile(
        join(directory, name),
        serializeAgent(label, seat, choices[index], parsed.budget),
        "utf8",
      );
      written.push(name);
    }
    if (panel) {
      for (const old of await existingSeats(directory, label)) {
        if (Number(old.seat) > choices.length) {
          await rm(join(directory, old.name));
          removed.push(old.name);
        }
      }
    }
  }
  for (const file of retiredFiles) {
    await rm(join(directory, file), { force: true });
    removed.push(file);
  }
  return { scope, directory, written, removed };
}

async function cli(args) {
  const [command, ...rest] = args;
  const cwd = process.cwd();
  const codexHome = getCodexHome();
  if (command === "show") {
    console.log(JSON.stringify(await show({ cwd, codexHome }), null, 2));
    return;
  }
  if (command === "resolve") {
    const label = rest.join(" ");
    console.log(
      JSON.stringify(await resolveRole(label, { cwd, codexHome }), null, 2),
    );
    return;
  }
  if (command === "models") {
    const { metadataModels: _metadataModels, ...catalog } =
      await readModelCatalog({ codexHome });
    console.log(JSON.stringify(catalog, null, 2));
    return;
  }
  if (command === "parent") {
    console.log(JSON.stringify(await readActiveParent({ codexHome }), null, 2));
    return;
  }
  if (command === "write") {
    const options = new Map();
    for (let index = 0; index < rest.length; index += 2)
      options.set(rest[index], rest[index + 1]);
    const scope = options.get("--scope");
    const inputArg = options.get("--input");
    if (!scope || !["project", "user"].includes(scope))
      throw new Error("write requires --scope project|user.");
    if (!inputArg)
      throw new Error(
        "write requires --input JSON (a literal JSON string or a file path). ",
      );
    let input;
    if (inputArg.startsWith("@"))
      input = await readFile(resolve(inputArg.slice(1)), "utf8");
    else input = inputArg;
    console.log(
      JSON.stringify(
        await writeConfiguration({ scope, input, cwd, codexHome }),
        null,
        2,
      ),
    );
    return;
  }
  throw new Error(
    "Usage: bun .agents/scripts/pstack-models.mjs {show|resolve ROLE LABEL|models|parent|write --scope project|user --input JSON}",
  );
}

if (import.meta.main) {
  try {
    await cli(process.argv.slice(2));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
