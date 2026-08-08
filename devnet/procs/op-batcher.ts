#!/usr/bin/env bun
// op-batcher posts the sequenced blocks to L1 as calldata, which is what
// advances the safe head — and what the verifier rebuilds the chain from.
//
// Calldata rather than blobs: blobs would need a beacon endpoint, and this
// devnet deliberately runs without one.

import { addresses, portOf, stack } from "../lib/env.ts";
import { addressing, runOp, txmgrArgs } from "../lib/optools.ts";
import { procInit, waitForPort } from "../lib/proc.ts";

procInit("op-batcher");
const a = addressing();
await waitForPort(portOf(a.httpAddr), "the engine's eth RPC");
await waitForPort(stack.opNodeRpcPort, "op-node");

await runOp("op-batcher", "op-batcher", stack.batcherRpcPort, [
    `--l1-eth-rpc=http://${a.hostAddr}:${stack.l1Port}`,
    `--l2-eth-rpc=http://${a.hostAddr}:${portOf(a.httpAddr)}`,
    `--rollup-rpc=${a.sequencerRpc}`,
    `--private-key=${addresses().batcherKey}`,
    "--data-availability-type=calldata",
    "--sub-safety-margin=4",
    "--poll-interval=2s",
    "--max-channel-duration=2",
    ...txmgrArgs(),
    `--rpc.addr=${a.bindAddr}`,
    `--rpc.port=${stack.batcherRpcPort}`,
]);
