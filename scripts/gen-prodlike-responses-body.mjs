#!/usr/bin/env node
// Emit the body the proxy would send upstream for a given client request on the
// Responses surface — byte for byte what prepareResponses builds.
//
// This exists because unit tests prove the envelope matches its own intent, not
// that the upstream accepts it. The gate reads verbatim text and a specific tool
// shape, so the only honest way to check "does production still pass the gate"
// is to send the production body and watch it get past the gate to whatever the
// model says next.
//
// Usage:
//   node scripts/gen-prodlike-responses-body.mjs <out.json> [model]

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (f) => fs.readFileSync(path.join(here, "..", "internal", "forward", f), "utf8");
// The chunks live in the generated file; the override lives with the logic.
const prompts = read("gateprompts.go");
const logic = read("freegate.go");

const goString = (name, source) => {
  const m = source.match(new RegExp(`(?:const|var) ${name} = ("(?:[^"\\\\]|\\\\.)*")`));
  if (!m) throw new Error(`could not find ${name}`);
  return JSON.parse(m[1]);
};

const BUILD = goString("gateBuildChunk", prompts);
const OVERRIDE = goString("gateOverride", logic);
const toolNames = [
  ...prompts.match(/var gateToolNames = \[\]string\{([^}]*)\}/)[1].matchAll(/"([^"]+)"/g),
].map((x) => x[1]);

const [out, model = "muse-spark-1.3-contributor-free"] = process.argv.slice(2);
if (!out) {
  console.error("usage: gen-prodlike-responses-body.mjs <out.json> [model]");
  process.exit(2);
}

// A Responses client names its tools at the top level, so the top-up entries the
// proxy writes are flat too — this is the shape under test.
const tools = toolNames.slice(0, 6).map((name) => ({
  type: "function",
  name,
  description: "probe",
  parameters: { type: "object", properties: {} },
}));

const body = {
  model,
  stream: true,
  // What prepareResponses assembles: the chunk first, then the override, then
  // whatever instructions the client sent.
  instructions: `${BUILD}${OVERRIDE}\n\nYou are a pirate.`,
  input: "hi",
  tools,
  // An unrelated field, to confirm the rewrite does not drop what it does not model.
  temperature: 0.7,
};

fs.writeFileSync(out, JSON.stringify(body));
fs.chmodSync(out, 0o600);
console.log(`${out} (${JSON.stringify(body).length} bytes, ${tools.length} flat tools)`);
