#!/usr/bin/env bun
// Starts op-cartesi: the authenticated Engine API for op-node, and the public
// eth_* RPC for everything else.
//
//   bun devnet/start-shim.ts
//
// This is op-cartesi on its own, not a devnet: no L1, no contracts, no
// op-node. The devnet is `docker compose up`, where the engine is a service
// with a machine server and a rollup config behind it. Here it is one process
// on whatever MACHINE_REMOTE and ENGINE_ADDR/HTTP_ADDR say — with no machine
// server, the in-memory mock — which is enough to drive the Engine API by hand
// and explore the RPC surface.

import { stack } from "./lib/env.ts";
import { ensureJwt, runEngine } from "./lib/opcartesi.ts";

ensureJwt();
await runEngine({
    remote: process.env.MACHINE_REMOTE,
    snapshot: process.env.MACHINE_SNAPSHOT ?? stack.snapshotDir,
    engineAddr: stack.engineAddr,
    httpAddr: stack.httpAddr,
});
