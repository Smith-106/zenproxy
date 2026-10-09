#!/usr/bin/env node
// Probe the upstream's /responses surface against the free-tier gate.
//
// Why: the free-tier envelope is applied only to paths ending
// /chat/completions, so a client that prefers the native Responses API gets a
// bare 403 FreeTierError. That 403 is the *client-identity* gate, which means
// the request was never enveloped — it says nothing about whether the model is
// usable on that surface.
//
// This script answers the two questions the fix depends on:
//   1. Does the gate protect /responses at all? (i.e. is a bare request refused)
//   2. If the official identity is present, does /responses answer?
//
// It sends through the running zenproxy reverse ingress, so the account key is
// injected by zenproxy and never handled here.
//
// Usage:
//   node scripts/probe-responses-gate.mjs <base-url> [model ...]
//
// <base-url> looks like http://127.0.0.1:19000/zen-01

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, "..", "internal", "forward", "gateprompts.go"), "utf8");

// The chunks are Go string literals produced by JSON.stringify, so a JSON parse
// of the quoted span recovers them byte for byte.
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

const TITLE = lit("gateTitleChunk");
const BUILD = lit("gateBuildChunk");

const [base, ...models] = process.argv.slice(2);
if (!base) {
  console.error("usage: probe-responses-gate.mjs <base-url> [model ...]");
  process.exit(2);
}
const targets = models.length ? models : ["big-pickle", "muse-spark-1.3-contributor-free"];

const BASE62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
const hex = (n) => Array.from({ length: n }, () => "0123456789abcdef"[Math.floor(Math.random() * 16)]).join("");
const b62 = (n) => Array.from({ length: n }, () => BASE62[Math.floor(Math.random() * BASE62.length)]).join("");
const clientID = (prefix) => `${prefix}${hex(12)}${b62(14)}`;

const officialHeaders = () => ({
  "content-type": "application/json",
  "user-agent": "opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14",
  "x-opencode-client": "cli",
  "x-opencode-project": "global",
  "x-opencode-request": clientID("msg_"),
  "x-opencode-session": clientID("ses_"),
  "x-opencode-session-id": clientID("ses_"),
});

const tools = (n) =>
  toolList.slice(0, n).map((name) => ({
    type: "function",
    function: { name, description: "probe", parameters: { type: "object", properties: {} } },
  }));

// The chat-completions shapes.
const chatBare = (model) => ({
  model,
  stream: true,
  messages: [{ role: "user", content: "hi" }],
});
const chatEnveloped = (model) => ({
  model,
  stream: true,
  messages: [
    { role: "system", content: BUILD },
    { role: "user", content: "hi" },
  ],
  tools: tools(6),
});

// The Responses API shapes. `instructions` is where Responses carries what
// chat carries as a system message; `input` replaces `messages`.
const respBare = (model) => ({ model, stream: true, input: "hi" });
const respInstr = (model, chunk) => ({
  model,
  stream: true,
  instructions: chunk,
  input: "hi",
});
const respInstrTools = (model, chunk, n) => ({
  model,
  stream: true,
  instructions: chunk,
  input: "hi",
  tools: tools(n),
});

const CASES = [
  // Controls on the surface the envelope was built for.
  ["chat  bare        ", "/chat/completions", {}, chatBare],
  ["chat  official    ", "/chat/completions", officialHeaders(), chatBare],
  ["chat  official+env", "/chat/completions", officialHeaders(), chatEnveloped],
  // The surface in question.
  ["resp  bare        ", "/responses", {}, respBare],
  ["resp  official    ", "/responses", officialHeaders(), respBare],
  ["resp  off+build   ", "/responses", officialHeaders(), (m) => respInstr(m, BUILD)],
  ["resp  off+build+T ", "/responses", officialHeaders(), (m) => respInstrTools(m, BUILD, 6)],
  ["resp  off+title   ", "/responses", officialHeaders(), (m) => respInstr(m, TITLE)],
];

const excerpt = (s, n = 260) => s.replace(/\s+/g, " ").trim().slice(0, n);

async function run(model) {
  console.log(`\n${"=".repeat(78)}\nmodel: ${model}\n${"=".repeat(78)}`);
  for (const [label, suffix, headers, body] of CASES) {
    const payload = JSON.stringify(body(model));
    let out;
    try {
      const res = await fetch(base + suffix, {
        method: "POST",
        headers,
        body: payload,
        signal: AbortSignal.timeout(45000),
      });
      const text = await res.text();
      let marker = "";
      try {
        const j = JSON.parse(text);
        const err = j.error ?? j;
        marker = err?.type || err?.code || "";
      } catch {
        marker = text.startsWith("data:") ? "SSE" : "";
      }
      out = `${String(res.status).padEnd(3)} ${(marker || "-").padEnd(28)} ${excerpt(text)}`;
    } catch (e) {
      out = `ERR ${e.name}: ${e.message}`;
    }
    console.log(`${label} ${out}`);
  }
}

for (const m of targets) await run(m);
