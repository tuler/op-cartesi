#!/usr/bin/env bun
// Generates the rollup.json op-node needs to derive this chain.
//
//   bun devnet/generate-config.ts
//
// The chain flags must match those the engine runs with, or op-node rejects
// the engine for serving a different genesis block — which is why both come
// from the chain configuration document lib/opcartesi.ts writes rather than
// from two lists.
//
// The devnet does this in the `genesis` service (compose/genesis.ts), which
// also anchors the rollup to L1 and records the flags for the engines. This is
// the generator alone, for op-cartesi driven by hand: it needs MACHINE_REMOTE
// pointed at a machine server, and takes the L1 anchor from whatever
// devnet/*.env and the environment say.

import { stack } from "./lib/env.ts";
import { generateRollupConfig } from "./lib/opcartesi.ts";

await generateRollupConfig({
    remote: process.env.MACHINE_REMOTE,
    snapshot: process.env.MACHINE_SNAPSHOT ?? stack.snapshotDir,
});
