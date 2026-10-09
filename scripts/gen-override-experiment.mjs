#!/usr/bin/env node
// Emit chat bodies that isolate whether the override sentence is what fails on
// a tool-less request.
//
// WHAT THIS ACTUALLY FOUND (read before trusting a single run):
//
// The tool-less case is NON-DETERMINISTIC. The same body, sent repeatedly to
// the same exit, alternates between an answer and a title:
//
//   'Simple math question: 2+2'   <- titled
//   '2+2 arithmetic question'     <- titled
//   '2 + 2 = 4'                   <- answered
//   '4'                           <- answered
//
// Roughly a third of eight samples titled. So a one-shot comparison here proves
// nothing, and the hypothesis this script was written to test — that the
// override's trailing "using only the tools the client advertises" clause is at
// fault — was NOT confirmed: the first sample of the tools-free rewrite looked
// worse, but that was noise.
//
// What is solid: with `tools` present the answer is consistently correct
// (measured '4', 'Paris', 'PONG' every time). Only the tool-less path — which
// must carry the far more insistent title prompt — is unreliable.
//
// Use this to gather a sample, not to draw a conclusion from one request.
//
// The title template is only accepted when the request carries no tools, and it
// says "you output ONLY a thread title" and "NEVER respond to questions". The
// override is meant to neutralise that.
//
//   none.json     chunk only            -> baseline: the model titles
//   current.json  chunk + shipped text  -> the behaviour under test
//   notools.json  chunk + tools-free    -> the clause hypothesis, unconfirmed
//
// Usage: node scripts/gen-override-experiment.mjs <outdir>

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const read = (f) => fs.readFileSync(path.join(here, "..", "internal", "forward", f), "utf8");
const goString = (name, source) => {
  const m = source.match(new RegExp(`(?:const|var) ${name} = ("(?:[^"\\\\]|\\\\.)*")`));
  if (!m) throw new Error(`could not find ${name}`);
  return JSON.parse(m[1]);
};

const TITLE = goString("gateTitleChunk", read("gateprompts.go"));
const OVERRIDE = goString("gateOverride", read("freegate.go"));

// The same sentence with the tools clause removed, and the user request named
// plainly instead.
const NO_TOOLS = OVERRIDE.replace(
  /,?\s*using only the tools the client advertises\.?$/,
  ". The client advertises no tools for this request, so answer from your own knowledge.",
);

if (NO_TOOLS === OVERRIDE) throw new Error("the tools clause was not found in gateOverride");

const [outdir] = process.argv.slice(2);
if (!outdir) {
  console.error("usage: gen-override-experiment.mjs <outdir>");
  process.exit(2);
}

const question = "What is 2+2?";
const make = (system) => ({
  model: "big-pickle",
  stream: true,
  messages: [
    { role: "system", content: system },
    { role: "user", content: question },
  ],
});

fs.mkdirSync(outdir, { recursive: true });
for (const [name, system] of [
  ["none", TITLE],
  ["current", TITLE + OVERRIDE],
  ["notools", TITLE + NO_TOOLS],
]) {
  const p = path.join(outdir, `${name}.json`);
  fs.writeFileSync(p, JSON.stringify(make(system)));
  fs.chmodSync(p, 0o600);
  console.log(p);
}
console.log(`\nquestion: ${question}`);
console.log(`current  override tail: ...${OVERRIDE.slice(-60)}`);
console.log(`notools  override tail: ...${NO_TOOLS.slice(-60)}`);
