#!/usr/bin/env bun
import { currentInspection, pausedOutcome, sessionEvidence } from './live.mjs';

const inputs = process.argv.slice(2);
if (inputs.length !== 1) throw new Error('Usage: replay.mjs PATH_TO_STDOUT_JSONL');
const events = (await Bun.file(inputs[0]).text()).split('\n').filter(line => line.trim()).map(line => JSON.parse(line));
const entries = events.filter(event => event.type === 'message_end').map((event, index) => ({ type: 'message', id: String(index), message: event.message }));
const evidence = sessionEvidence(entries.map(entry => JSON.stringify(entry)).join('\n'));
const checked = [];
for (const result of evidence.results) {
 if (result.isError || !['litter_inspect', 'litter_stop'].includes(result.name)) continue;
 const call = evidence.calls.find(call => call.id === result.id && call.name === result.name);
 if (!call) throw new Error(`No matching call for ${result.id}.`);
 const prefix = { ...evidence, results: evidence.results.filter(candidate => candidate.position <= result.position) };
 const verified = result.name === 'litter_inspect' ? currentInspection(prefix, call.arguments.id) : pausedOutcome(prefix, call.arguments.id);
 checked.push({ operation: result.name, id: call.arguments.id, state: verified.result.value.state });
}
for (const operation of ['litter_inspect', 'litter_stop']) {
 if (!checked.some(result => result.operation === operation)) throw new Error(`No successful ${operation} result was verified.`);
}
console.log(JSON.stringify({ ok: true, input: inputs[0], checked }));
