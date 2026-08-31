#!/usr/bin/env bun
// The chain's genesis, as docker compose runs it: a container that does its
// work and exits, which everything downstream waits for with
// `depends_on: {condition: service_completed_successfully}`.
//
// Four outputs: the JWT secret, devnet/chain-genesis.env (the L1 anchor),
// devnet/rollup.json (what op-node derives the chain from), and
//
//   devnet/chain-config.json   the consensus parameters it was generated from.
//
// Those parameters determine the L2 genesis block hash, so an engine has to
// serve exactly the chain the config was generated for or op-node rejects it
// for serving a different genesis — and the ones below genesis (the input
// envelope, the cycle budget) op-node cannot check at all, so a disagreement
// there surfaces as a state root divergence instead. Generating and running
// are two containers, and this file is how the second is told what the first
// committed to (compose/engine.sh).
//
// The machine server is a container of its own too (`genesis-machine`), since
// a server holds exactly one machine and the sequencer's is already holding
// the chain's.

import { existsSync } from "node:fs";
import { paths, readEnvFile } from "../lib/env.ts";
import { anchorRollup } from "../lib/genesis.ts";
import { ensureJwt, generateRollupConfig, writeChainConfig } from "../lib/opcartesi.ts";
import { die, forget, say } from "../lib/proc.ts";
import { alreadyDone, anchorStillOnL1 } from "./live.ts";

const remote = process.env.MACHINE_REMOTE;
if (!remote) die("MACHINE_REMOTE is unset — this step needs a machine server of its own");
const snapshot = process.env.MACHINE_SNAPSHOT ?? "/snapshot";
const configFile = paths.chainConfig;

// Already anchored, to the deployment that is on L1 now? Then this chain is
// the running one and re-anchoring would only move it out from under the
// engine serving it. The two files are compared rather than read through
// lib/env.ts, whose layering would merge them and hide exactly the
// disagreement being looked for.
const deployed = readEnvFile(paths.l1Addresses);
const anchored = readEnvFile(paths.chainGenesis);
if (
    existsSync(paths.rollupConfig) &&
    existsSync(configFile) &&
    anchored.L1_GENESIS_HASH !== undefined &&
    anchored.L1_GENESIS_HASH === deployed.L1_GENESIS_HASH &&
    (await anchorStillOnL1(anchored))
) {
    alreadyDone(`the rollup is already anchored at L1 block ${anchored.L1_GENESIS_NUMBER}`);
}

// Otherwise this is a new chain, and a new chain starts from nothing. There is
// no moment before the run to clear anything in — the containers are the run —
// so it happens here, at the step where the chain is born: every file below
// describes the chain being replaced, and left in place it would describe it
// to the scripts of the one replacing it. The L1 addresses are not in the
// list: the deploy this step waited for has just written them.
for (const file of [paths.outputsAddresses, paths.tokenEnv, paths.rollupConfig]) {
    forget(file);
}

// op-node needs the secret before the engine would otherwise create it, and it
// is a bind mount into the op-node container, which docker would silently
// create as a directory if the file were missing.
ensureJwt();

await anchorRollup();
await generateRollupConfig({ remote, snapshot });

// One file, one source, both engines. It carries the consensus parameters and
// nothing else: which machine an engine drives is its own business, and
// differs between the sequencer and the verifier.
await writeChainConfig();
say(`wrote ${configFile} — the chain every engine here must serve`);

// The generator is done with the machine, and a booted one is not cheap to
// leave sitting around. Shutting the server down is how a step cleans up the
// thing it needed: the server exits, its container exits with it, and nothing
// downstream depends on it.
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
