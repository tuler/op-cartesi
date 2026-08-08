#!/usr/bin/env bun
// Generates the rollup.json op-node needs to derive this chain.
//
//   bun devnet/generate-config.ts
//
// The chain flags must match those the engine runs with, or op-node rejects
// the engine for serving a different genesis block — which is why both come
// from chainFlags() in lib/opcartesi.ts rather than from two lists.
//
// procs/genesis.ts is what normally runs this, with a machine server of its
// own; on its own it needs MACHINE_REMOTE pointed at one.

import { stack } from "./lib/env.ts";
import { generateRollupConfig } from "./lib/opcartesi.ts";

await generateRollupConfig({
    remote: process.env.MACHINE_REMOTE,
    snapshot: process.env.MACHINE_SNAPSHOT ?? stack.snapshotDir,
});
