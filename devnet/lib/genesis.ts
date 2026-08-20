// Anchoring the rollup to L1: the one fact everything downstream reads back.
//
// The rollup starts after a particular L1 block, and the L2 genesis timestamp
// is derived from it, so op-node's derivation clock and the engine's genesis
// agree. Both are consensus-relevant, and both are written to
// devnet/chain-genesis.env, which lib/env.ts reads back — that is how a
// process started separately ends up on the same genesis as the rollup config.
//
// This lives here rather than in procs/genesis.ts because it is a property of
// the chain, not of the way it was started: the mprocs pane and the compose
// one-shot (compose/genesis.ts) both anchor exactly like this, and differ only
// in what they wait for.

import { writeFileSync } from "node:fs";
import { chainParams, paths, stack } from "./env.ts";
import { die, forget, say } from "./proc.ts";
import { l1Public } from "./wallet.ts";

/** Reads the L1 anchor block and writes devnet/chain-genesis.env. */
export async function anchorRollup(): Promise<void> {
    const l1 = l1Public();
    // The file this step writes is one lib/env.ts reads back, and it sits
    // *above* l1-addresses.env in that layering — so a copy left by a previous
    // run shadows the anchor the deploy just recorded, and the block it names
    // belongs to an L1 that no longer exists. What that looks like is this
    // step reporting a reorg against a deployment it never saw. The step is
    // the file's only writer, so the fix is not to read its own last output:
    // start-devnet.ts already clears it before a run, and now the anchor does
    // too, which is what the compose bring-up needs since compose has no
    // moment before the run to clear anything in.
    forget(paths.chainGenesis);
    // With contracts, the anchor is the block they landed in, which the deploy
    // recorded; without them there is nothing to anchor to but L1 genesis.
    const recorded = stack.withContracts ? chainParams() : undefined;
    const anchorNumber = recorded ? BigInt(recorded.l1GenesisNumber) : 0n;
    const anchor = await l1.getBlock({ blockNumber: anchorNumber });
    // The hash op-deployer recorded is the one the deployment committed to;
    // this only reads the block again for its timestamp. If the two disagree
    // the L1 moved under the deployment, and anchoring to either would be a
    // guess.
    if (recorded && recorded.l1GenesisHash !== anchor.hash) {
        die(
            `L1 block ${anchorNumber} is ${anchor.hash}, but the deployment recorded ` +
                `${recorded.l1GenesisHash} — the L1 reorged; redeploy`,
        );
    }

    writeFileSync(
        paths.chainGenesis,
        [
            "# Written by devnet/lib/genesis.ts. Read back by devnet/lib/env.ts.",
            "# The L1 anchor of this rollup, and the L2 genesis timestamp derived",
            "# from it. Both are consensus-relevant: op-node and the engine must",
            "# agree on them or the engine serves a genesis op-node rejects.",
            `L1_GENESIS_HASH=${anchor.hash}`,
            `L1_GENESIS_NUMBER=${anchor.number}`,
            `GENESIS_TIMESTAMP=${anchor.timestamp}`,
            "",
        ].join("\n"),
    );
    say(
        `anchored at L1 block ${anchor.number} (${anchor.hash}), genesis timestamp ${anchor.timestamp}`,
    );
}
