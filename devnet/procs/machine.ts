#!/usr/bin/env bun
// The Cartesi Machine server the sequencer's engine drives.
//
// This pane is the guest's console: the emulator writes the machine's own
// output here — Linux booting, then whatever the guest program (demo)
// prints — including from the servers op-cartesi forks per block, which
// inherit these file descriptors. The guest's *structured* account of each
// transaction, the reports it emits, is the `guest` pane instead.

import { stack } from "../lib/env.ts";
import { exec, procInit, say } from "../lib/proc.ts";

procInit("machine");
say(`machine server on :${stack.machinePort} (snapshot ${stack.snapshotDir})`);
await exec(["cartesi-jsonrpc-machine", `--server-address=127.0.0.1:${stack.machinePort}`]);
