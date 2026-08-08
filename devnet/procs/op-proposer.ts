#!/usr/bin/env bun
// op-proposer reads optimism_outputAtBlock from op-node and creates a game per
// proposal. Nothing about it is op-cartesi-specific: the output root it
// submits already commits to the Cartesi outputs tree, because op-node builds
// it from the header's withdrawalsRoot.
//
// --allow-non-finalized because anvil has no beacon chain to finalize
// anything, so a proposer that waited for finality would never propose.
//
// The txmgr settings below stop it rebroadcasting proposals anvil has already
// mined, and waiting ten blocks to call them confirmed. See txmgrArgs.

import { addresses, stack } from "../lib/env.ts";
import { addressing, runOp, txmgrArgs } from "../lib/optools.ts";
import { die, procInit, waitForPort, waitReady } from "../lib/proc.ts";

procInit("op-proposer");
await waitReady("l1-contracts", "the DisputeGameFactory");
await waitForPort(stack.opNodeRpcPort, "op-node");

// Read after the wait: the deployment that produced this address happened
// while this process was already running.
const a = addresses();
if (!a.disputeGameFactory) die("no DISPUTE_GAME_FACTORY_ADDRESS in devnet/l1-addresses.env");

const net = addressing();
await runOp("op-proposer", "op-proposer", stack.proposerRpcPort, [
    `--l1-eth-rpc=http://${net.hostAddr}:${stack.l1Port}`,
    `--rollup-rpc=${net.sequencerRpc}`,
    `--game-factory-address=${a.disputeGameFactory}`,
    `--game-type=${a.disputeGameType}`,
    `--private-key=${a.proposerKey}`,
    "--proposal-interval=20s",
    "--poll-interval=4s",
    "--allow-non-finalized",
    "--wait-node-sync",
    ...txmgrArgs(),
    `--rpc.addr=${net.bindAddr}`,
    `--rpc.port=${stack.proposerRpcPort}`,
]);
