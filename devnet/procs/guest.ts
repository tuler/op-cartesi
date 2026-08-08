#!/usr/bin/env bun
// The guest program's account of the chain: the reports it emits per
// transaction, read back over cartesi_getTransactionEmissions.
//
// The machine's console output — Linux booting, guest stdout — is the
// `machine` pane. This is the other half: what the guest says about each
// input, including why it rejected one.

import { join } from "node:path";
import { paths, portOf } from "../lib/env.ts";
import { addressing } from "../lib/optools.ts";
import { exec, procInit, waitForPort } from "../lib/proc.ts";

procInit("guest");
const { httpAddr } = addressing();
await waitForPort(portOf(httpAddr), "the engine's eth RPC");

// The engine may be bound to 0.0.0.0 (that is how a container reaches it), but
// this is on the host, so it always dials loopback.
await exec([
    process.execPath,
    join(paths.devnet, "guest-log.ts"),
    `http://127.0.0.1:${portOf(httpAddr)}`,
]);
