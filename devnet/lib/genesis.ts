// Anchoring the rollup to L1: the one fact everything downstream reads back.
//
// The rollup starts after a particular L1 block — the one the OP Stack
// contracts landed in, since before it there is no SystemConfig for op-node to
// read — and the L2 genesis timestamp is derived from it, so op-node's
// derivation clock and the engine's genesis agree. Both are
// consensus-relevant, and both are written to devnet/chain-genesis.env, which
// lib/env.ts reads back: that is how the engine, in a container of its own,
// ends up on exactly the genesis the rollup config was generated with.
//
// The anchor is read from l1-addresses.env itself rather than through
// chainParams(), which is the one place in this package where the layering is
// the wrong tool. chain-genesis.env sits *above* l1-addresses.env in it, so
// the merged view of "the anchor" is this file's own last output whenever
// there is one — and a copy left by a previous run names a block on an L1 that
// no longer exists. Read from the deploy's file, the answer is the deploy's.

import { writeFileSync } from "node:fs";
import { paths, readEnvFile } from "./env.ts";
import { die, say } from "./proc.ts";
import { l1Public } from "./wallet.ts";

/** Reads the L1 anchor block and writes devnet/chain-genesis.env. */
export async function anchorRollup(): Promise<void> {
    const deployed = readEnvFile(paths.l1Addresses);
    const recordedHash = deployed.L1_GENESIS_HASH;
    const recordedNumber = deployed.L1_GENESIS_NUMBER;
    if (recordedHash === undefined || recordedNumber === undefined) {
        die(`no L1 deployment recorded in ${paths.l1Addresses} — deploy the contracts first`);
    }

    const anchor = await l1Public().getBlock({ blockNumber: BigInt(recordedNumber) });
    // The hash op-deployer recorded is the one the deployment committed to;
    // this only reads the block again for its timestamp. If the two disagree
    // the L1 moved under the deployment, and anchoring to either would be a
    // guess.
    if (anchor.hash.toLowerCase() !== recordedHash.toLowerCase()) {
        die(
            `L1 block ${recordedNumber} is ${anchor.hash}, but the deployment recorded ` +
                `${recordedHash} — the L1 reorged; redeploy`,
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
