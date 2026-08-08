#!/usr/bin/env bun
// op-cartesi, the sequencer's execution engine: the authenticated Engine API
// op-node drives, and the public eth_* RPC everything else uses.
//
// It waits for genesis because its chain flags have to match the ones
// rollup.json was generated with — the genesis timestamp among them, which
// procs/genesis.ts writes to chain-genesis.env for exactly this reason.

import { portOf, stack } from "../lib/env.ts";
import { runEngine } from "../lib/opcartesi.ts";
import { addressing } from "../lib/optools.ts";
import { procInit, say, waitForPort, waitReady } from "../lib/proc.ts";

procInit("engine");
await waitReady("genesis", "the rollup config");
await waitForPort(stack.machinePort, "the machine server");

const { engineAddr, httpAddr } = addressing();
say(
    `booting the machine; the engine binds :${portOf(engineAddr)} and :${portOf(httpAddr)} once the chain is open`,
);
await runEngine({
    remote: `http://127.0.0.1:${stack.machinePort}`,
    snapshot: stack.snapshotDir,
    engineAddr,
    httpAddr,
});
