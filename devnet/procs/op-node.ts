#!/usr/bin/env bun
// op-node in sequencer mode: it drives block production, and every L2 block
// carries the L1-attributes deposit it injects.

import { portOf, stack } from "../lib/env.ts";
import { addressing, runOp } from "../lib/optools.ts";
import { procInit, waitForPort, waitReady } from "../lib/proc.ts";

procInit("op-node");
await waitReady("genesis", "the rollup config");
const a = addressing();
// op-cartesi binds its listeners only after the chain is open and the machine
// has booted, so a port that answers is an engine that can serve.
await waitForPort(portOf(a.engineAddr), "the engine");

await runOp("op-node", "op-node", stack.opNodeRpcPort, [
    `--l1=http://${a.hostAddr}:${stack.l1Port}`,
    "--l1.rpckind=basic",
    "--l1.trustrpc",
    `--rollup.l1-chain-config=${a.l1ChainConfigArg}`,
    `--l2=http://${a.hostAddr}:${portOf(a.engineAddr)}`,
    `--l2.jwt-secret=${a.jwtArg}`,
    `--rollup.config=${a.rollupConfigArg}`,
    "--sequencer.enabled",
    "--sequencer.l1-confs=0",
    "--verifier.l1-confs=0",
    "--p2p.disable",
    "--l1.beacon.ignore",
    `--rpc.addr=${a.bindAddr}`,
    `--rpc.port=${stack.opNodeRpcPort}`,
]);
