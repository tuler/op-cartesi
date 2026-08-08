#!/usr/bin/env bun
// The verifier's op-node: same rollup config as the sequencer's, without
// --sequencer.enabled, so everything it knows comes from L1.

import { portOf, stack } from "../lib/env.ts";
import { addressing, runOp } from "../lib/optools.ts";
import { procInit, waitForPort, waitReady } from "../lib/proc.ts";

procInit("verifier-node");
await waitReady("genesis", "the rollup config");
const a = addressing();
await waitForPort(portOf(a.verifierEngineAddr), "the verifier's engine");

await runOp("op-node", "verifier-node", stack.verifierOpNodeRpcPort, [
    `--l1=http://${a.hostAddr}:${stack.l1Port}`,
    "--l1.rpckind=basic",
    "--l1.trustrpc",
    `--rollup.l1-chain-config=${a.l1ChainConfigArg}`,
    `--l2=http://${a.hostAddr}:${portOf(a.verifierEngineAddr)}`,
    `--l2.jwt-secret=${a.jwtArg}`,
    `--rollup.config=${a.rollupConfigArg}`,
    "--verifier.l1-confs=0",
    "--p2p.disable",
    "--l1.beacon.ignore",
    `--rpc.addr=${a.bindAddr}`,
    `--rpc.port=${stack.verifierOpNodeRpcPort}`,
]);
