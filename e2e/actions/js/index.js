// SPDX-License-Identifier: GPL-3.0-or-later
//
// The main step of the JavaScript action in action.yml. It exits non-zero
// unless its input is right and its own script, bin/tool.sh, runs, which
// needs the executable bit the runner copied in.
"use strict";

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

function fail(message) {
  console.log(`::error::${message}`);
  process.exit(1);
}

const who = process.env.INPUT_WHO;
if (who !== "microvm") {
  fail(`input who is ${JSON.stringify(who)}, want "microvm"`);
}
const tool = execFileSync(path.join(__dirname, "bin", "tool.sh"), { encoding: "utf8" }).trim();
if (tool !== "tool ran") {
  fail(`bin/tool.sh printed ${JSON.stringify(tool)}, want "tool ran"`);
}
// The runner reads these files back out of the VM after the step.
fs.appendFileSync(process.env.GITHUB_OUTPUT, `greeting=hello, ${who}\n`);
fs.appendFileSync(process.env.GITHUB_ENV, "FROM_JS=yes\n");
fs.appendFileSync(process.env.GITHUB_STATE, "ran=yes\n");
console.log(`js action: hello, ${who}, from node ${process.version}`);
