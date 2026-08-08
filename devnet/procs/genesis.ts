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
//   devnet/rollup.json        what op-node derives the chain from.
//
// Generating rollup.json loads the snapshot, so this needs a machine server of
// its own; it is started and stopped here rather than shared with the node's,
// because a server holds exactly one machine.

import { writeFileSync } from "node:fs";
import { chainParams, l1Public, paths, stack } from "../lib/env.ts";
import { generateRollupConfig, startMachineServer } from "../lib/opcartesi.ts";
import { clearReady, die, markReady, procInit, say, waitForPort, waitReady } from "../lib/proc.ts";

procInit("genesis");
clearReady("genesis");
await waitForPort(stack.l1Port, "anvil");

if (stack.withContracts) {
    await waitReady("l1-contracts", "the L1 contract deployment");
}

const l1 = l1Public();
// With contracts, the anchor is the block they landed in, which the deploy
// recorded; without them there is nothing to anchor to but L1 genesis.
const recorded = stack.withContracts ? chainParams() : undefined;
const anchorNumber = recorded ? BigInt(recorded.l1GenesisNumber) : 0n;
const anchor = await l1.getBlock({ blockNumber: anchorNumber });
// The hash op-deployer recorded is the one the deployment committed to; this
// only reads the block again for its timestamp. If the two disagree the L1
// moved under the deployment, and anchoring to either would be a guess.
if (recorded && recorded.l1GenesisHash !== anchor.hash) {
    die(
        `L1 block ${anchorNumber} is ${anchor.hash}, but the deployment recorded ` +
            `${recorded.l1GenesisHash} — the L1 reorged; redeploy`,
    );
}

writeFileSync(
    paths.chainGenesis,
    [
        "# Written by devnet/procs/genesis.ts. Read back by devnet/lib/env.ts.",
        "# The L1 anchor of this rollup, and the L2 genesis timestamp derived",
        "# from it. Both are consensus-relevant: op-node and the engine must",
        "# agree on them or the engine serves a genesis op-node rejects.",
        `L1_GENESIS_HASH=${anchor.hash}`,
        `L1_GENESIS_NUMBER=${anchor.number}`,
        // The L2 genesis timestamp is anchored to the L1 block the rollup
        // starts after, so op-node's derivation clock and the engine's genesis
        // agree.
        `GENESIS_TIMESTAMP=${anchor.timestamp}`,
        "",
    ].join("\n"),
);
say(
    `anchored at L1 block ${anchor.number} (${anchor.hash}), genesis timestamp ${anchor.timestamp}`,
);

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
