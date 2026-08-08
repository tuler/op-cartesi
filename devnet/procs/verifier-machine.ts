#!/usr/bin/env bun
// The verifier's Cartesi Machine server — its own, because a server holds
// exactly one machine. Guest console output for the verifying node lands here.

import { stack } from "../lib/env.ts";
import { exec, procInit, say } from "../lib/proc.ts";

procInit("verifier-machine");
say(`verifier machine server on :${stack.verifierMachinePort}`);
await exec(["cartesi-jsonrpc-machine", `--server-address=127.0.0.1:${stack.verifierMachinePort}`]);
