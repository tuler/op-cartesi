#!/usr/bin/env bun
// Starts op-cartesi: the authenticated Engine API for op-node, and the public
// eth_* RPC for everything else.
//
//   bun devnet/start-shim.ts
//
// procs/engine.ts is what normally runs this — as a function call rather than
// a subprocess — after waiting for the rollup config and the machine server.
// This is the same engine started on its own, on whatever MACHINE_REMOTE and
// ENGINE_ADDR/HTTP_ADDR say.

import { stack } from "./lib/env.ts";
import { ensureJwt, runEngine } from "./lib/opcartesi.ts";

ensureJwt();
await runEngine({
    remote: process.env.MACHINE_REMOTE,
    snapshot: process.env.MACHINE_SNAPSHOT ?? stack.snapshotDir,
    engineAddr: stack.engineAddr,
    httpAddr: stack.httpAddr,
});
