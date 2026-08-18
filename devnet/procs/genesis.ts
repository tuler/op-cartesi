#!/usr/bin/env bun
// Anchors the rollup to L1 and writes the config every other piece reads.
//
// Two outputs, both consensus-relevant:
//
//   devnet/chain-genesis.env  the L1 block the rollup starts after and the L2
//                             genesis timestamp derived from it. lib/env.ts
//                             reads it back, which is how the engine — a
//                             different process, started from a different
//                             pane — ends up on the same genesis as op-node.
//                             Written by lib/genesis.ts, which the compose
//                             bring-up shares.
//   devnet/rollup.json        what op-node derives the chain from.
//
// Generating rollup.json loads the snapshot, so this needs a machine server of
// its own; it is started and stopped here rather than shared with the node's,
// because a server holds exactly one machine.

import { stack } from "../lib/env.ts";
import { anchorRollup } from "../lib/genesis.ts";
import { generateRollupConfig, startMachineServer } from "../lib/opcartesi.ts";
import { clearReady, markReady, procInit, say, waitForPort, waitReady } from "../lib/proc.ts";

procInit("genesis");
clearReady("genesis");
await waitForPort(stack.l1Port, "anvil");

if (stack.withContracts) {
    await waitReady("l1-contracts", "the L1 contract deployment");
}

await anchorRollup();

say("booting a machine to read the genesis state root off the snapshot");
const server = startMachineServer(stack.genesisMachinePort);
try {
    await waitForPort(stack.genesisMachinePort, "machine server");
    await generateRollupConfig({
        remote: `http://127.0.0.1:${stack.genesisMachinePort}`,
        snapshot: stack.snapshotDir,
    });
} finally {
    // The generator is done with it, and a booted machine is not cheap to
    // leave sitting around.
    server.kill();
    await server.exited;
}

markReady("genesis");
say("the rollup is anchored and the config is written; the pane stays down from here");
