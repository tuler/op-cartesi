#!/usr/bin/env bun
// The outputs suite: the validator that opens an OP proposal, the executor
// that pays vouchers against it, and the Cartesi-style portals — plus the
// portal registration the guest needs before it will credit deposits.
//
// This one does not autostart. It is the step you take when you want to move
// assets rather than just watch blocks, and it needs a proposal to exist
// first, so it is a pane you start yourself (`s` in mprocs) once the proposer
// has had a chance to propose. Everything it writes lands in
// devnet/outputs-addresses.env, which lib/env.ts reads back.

import { deployOutputs } from "../deploy-outputs.ts";
import { portOf } from "../lib/env.ts";
import { addressing } from "../lib/optools.ts";
import { procInit, say, waitForPort, waitReady } from "../lib/proc.ts";

procInit("outputs");
await waitReady("l1-contracts", "the L1 contract deployment");
await waitForPort(portOf(addressing().httpAddr), "the engine's eth RPC");

await deployOutputs();
say("the outputs suite is deployed; deposits and withdrawals will find it");
