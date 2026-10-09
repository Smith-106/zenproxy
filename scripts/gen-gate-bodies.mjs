#!/usr/bin/env node
// Emit ready-to-send enveloped request bodies for the free-tier gate.
//
// The chunks come from internal/forward/gateprompts.go so the probe sends the
// same bytes the proxy does; a retyped character would cost the envelope and
// the measurement would be about the typo, not the model.
//
// Usage:
//   node scripts/gen-gate-bodies.mjs <outdir> <model>
//
// Writes <outdir>/chat.json and <outdir>/resp.json. Nothing is printed but the
// two paths, because these bodies are large and are pasted into a remote shell.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, "..", "internal", "forward", "gateprompts.go"), "utf8");

const lit = (name) => {
  const m = src.match(new RegExp(`const ${name} = ("(?:[^"\\\\]|\\\\.)*")`));
  if (!m) throw new Error(`could not find ${name} in gateprompts.go`);
  return JSON.parse(m[1]);
};
const toolList = (() => {
  const m = src.match(/var gateToolNames = \[\]string\{([^}]*)\}/);
  if (!m) throw new Error("could not find gateToolNames");
  return [...m[1].matchAll(/"([^"]+)"/g)].map((x) => x[1]);
})();

const [outdir, model] = process.argv.slice(2);
if (!outdir || !model) {
  console.error("usage: gen-gate-bodies.mjs <outdir> <model>");
  process.exit(2);
}

const BUILD = lit("gateBuildChunk");
const tools = toolList.slice(0, 6).map((name) => ({
  type: "function",
  function: { name, description: "probe", parameters: { type: "object", properties: {} } },
}));

// With tools present the gate wants the coding-agent prompt; without tools it
// wants the title prompt. Every body here carries tools, so every body uses
// BUILD — the pairing is the part that silently costs the envelope.
const chat = {
  model,
  stream: true,
  messages: [
    { role: "system", content: BUILD },
    { role: "user", content: "hi" },
  ],
  tools,
};
// The Responses shape: `instructions` is where Responses carries what chat
// carries as a system message, and `input` replaces `messages`.
const resp = { model, stream: true, instructions: BUILD, input: "hi", tools };

fs.mkdirSync(outdir, { recursive: true });
for (const [name, body] of [["chat", chat], ["resp", resp]]) {
  const p = path.join(outdir, `${name}.json`);
  fs.writeFileSync(p, JSON.stringify(body));
  fs.chmodSync(p, 0o600);
  console.log(p);
}
