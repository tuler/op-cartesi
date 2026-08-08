#!/usr/bin/env bun
// Stops whatever is left of a devnet.
//
// Quitting mprocs already stops every process it started, so this is for the
// other endings: mprocs killed rather than quit, a terminal closed, a pane
// started by hand. It works from what the processes recorded about themselves
// under devnet/.state, never from process names — matching by name was both
// too narrow and too wide: `pkill -x cartesi-jsonrpc` only matches on Linux,
// where the process name is truncated to 15 characters, so on macOS the
// machine servers survived and the next run inherited a server that already
// held a machine, while on Linux it happily killed emulators and anvils
// belonging to whatever else the developer was running.

import { forgetRecordedProcs, removeContainers, say, stopRecordedProcs } from "./lib/proc.ts";

removeContainers();
await stopRecordedProcs("SIGTERM");
// Whatever is still recorded after that round ignored SIGTERM, or is a machine
// server the emulator forked and left behind.
await stopRecordedProcs("SIGKILL");
forgetRecordedProcs();

say("devnet stopped");
