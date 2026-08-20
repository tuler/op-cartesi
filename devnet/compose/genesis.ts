#!/usr/bin/env bun
// The chain's genesis, as docker compose runs it: a container that does its
// work and exits, which everything downstream waits for with
// `depends_on: {condition: service_completed_successfully}`.
//
// Same outputs as the mprocs pane (procs/genesis.ts) — the JWT secret,
// devnet/chain-genesis.env, devnet/rollup.json — minus the waiting, which
// compose does, and plus one file the pane has no use for:
//
//   devnet/chain-flags   the chain flags rollup.json was generated with.
//
// Those flags determine the L2 genesis block hash, so the engine has to run
// with exactly the ones the config was generated with or op-node rejects it
// for serving a different genesis. Under mprocs both sides call chainFlags()
// in one process and the question does not arise; here they are two
// containers, and this file is how the engine (compose/engine.sh) is told what
// this step committed to.
//
// The machine server is a container of its own too (`genesis-machine`), since
// a server holds exactly one machine and the sequencer's is already holding
// the chain's.

import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { paths, readEnvFile } from "../lib/env.ts";
import { anchorRollup } from "../lib/genesis.ts";
import { chainFlags, ensureJwt, generateRollupConfig } from "../lib/opcartesi.ts";
import { die, forget, say } from "../lib/proc.ts";
import { alreadyDone, anchorStillOnL1 } from "./live.ts";

const remote = process.env.MACHINE_REMOTE;
if (!remote) die("MACHINE_REMOTE is unset — this step needs a machine server of its own");
const snapshot = process.env.MACHINE_SNAPSHOT ?? "/snapshot";
const flagsFile = process.env.CHAIN_FLAGS_FILE ?? join(paths.devnet, "chain-flags");

// Already anchored, to the deployment that is on L1 now? Then this chain is
// the running one and re-anchoring would only move it out from under the
// engine serving it. The two files are compared rather than read through
// lib/env.ts, whose layering would merge them and hide exactly the
// disagreement being looked for.
const deployed = readEnvFile(paths.l1Addresses);
const anchored = readEnvFile(paths.chainGenesis);
if (
    existsSync(paths.rollupConfig) &&
    existsSync(flagsFile) &&
    anchored.L1_GENESIS_HASH !== undefined &&
    anchored.L1_GENESIS_HASH === deployed.L1_GENESIS_HASH &&
    (await anchorStillOnL1(anchored))
) {
    alreadyDone(`the rollup is already anchored at L1 block ${anchored.L1_GENESIS_NUMBER}`);
}

// Otherwise this is a new chain, and a run starts from nothing.
// start-devnet.ts clears these before it starts anything, which is a moment
// compose does not have: its containers are the run. So it happens here, at
// the step where the chain is born — every file below describes the chain
// being replaced, and left in place it would describe it to the scripts of the
// one replacing it. The L1 addresses are not in the list: the deploy this step
// waited for has just written them.
for (const file of [paths.outputsAddresses, paths.tokenEnv, paths.rollupConfig]) {
    forget(file);
}

// op-node needs the secret before the engine would otherwise create it, and it
// is a bind mount into the op-node container, which docker would silently
// create as a directory if the file were missing.
ensureJwt();

await anchorRollup();
await generateRollupConfig({ remote, snapshot });

// Empty machine options on purpose: what the engine gets from this file is the
// consensus-relevant flags only. Which machine it drives is the engine's own
// business — and differs between the sequencer and the verifier.
writeFileSync(flagsFile, `${chainFlags({ remote: "", snapshot: "" }).join(" ")}\n`);
say(`wrote ${flagsFile} — the flags the engines must run with`);

// The generator is done with the machine, and a booted one is not cheap to
// leave sitting around: procs/genesis.ts kills the server it spawned, and this
// is the same act one container over. The server exits, its container exits
// with it, and nothing downstream depends on it.
try {
    await fetch(remote, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "shutdown", params: [] }),
    });
    say(`shut down the machine server at ${remote}`);
} catch {
    // It went away before it could answer, which is the wanted state anyway.
}
