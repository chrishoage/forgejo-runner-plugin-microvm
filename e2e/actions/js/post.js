// SPDX-License-Identifier: GPL-3.0-or-later
//
// The post step of the JavaScript action in action.yml. It exits non-zero
// unless it sees the state index.js saved, which the runner read back out of
// the VM.
"use strict";

if (process.env.STATE_ran !== "yes") {
  console.log(`::error::state ran is ${JSON.stringify(process.env.STATE_ran)}, want "yes"`);
  process.exit(1);
}
console.log("js action post: the state came back");
