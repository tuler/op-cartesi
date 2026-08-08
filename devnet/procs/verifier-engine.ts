#!/usr/bin/env bun
// The verifying node's engine: a second op-cartesi that sequences nothing and
// learns the chain only from what the batcher posted to L1. That it agrees
// block for block is the property that makes this a rollup rather than a
// database with an RPC.

import { portOf, stack } from "../lib/env.ts";
import { runEngine } from "../lib/opcartesi.ts";
import { addressing } from "../lib/optools.ts";
import { procInit, say, waitForPort, waitReady } from "../lib/proc.ts";

procInit("verifier-engine");
await waitReady("genesis", "the rollup config");
await waitForPort(stack.verifierMachinePort, "the verifier's machine server");

const { verifierEngineAddr, verifierHttpAddr } = addressing();
say(`verifier engine on :${portOf(verifierEngineAddr)} (eth RPC :${portOf(verifierHttpAddr)})`);
await runEngine({
    remote: `http://127.0.0.1:${stack.verifierMachinePort}`,
    snapshot: stack.snapshotDir,
    engineAddr: verifierEngineAddr,
    httpAddr: verifierHttpAddr,
    // Its own store: a pebble database is held by one process, and pointing
    // two nodes at one directory fails with a message about resources rather
    // than about sharing.
    dataDir: stack.dataDir ? `${stack.dataDir}-verifier` : "",
});
