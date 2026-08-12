#!/usr/bin/env bun
// The front page: what this devnet is serving and how to poke at it.
//
// It prints and exits — the pane keeps the text, which is the point. mprocs
// marks it DOWN, as it does every step that finishes rather than runs
// (genesis, l1-contracts, outputs).

import { chainParams, paths, portOf, stack } from "../lib/env.ts";
import { addressing } from "../lib/optools.ts";
import { procInit } from "../lib/proc.ts";

procInit("info");
const a = addressing();
const p = chainParams();
const l1Rpc = `http://127.0.0.1:${stack.l1Port}`;
const l2Rpc = `http://127.0.0.1:${portOf(a.httpAddr)}`;
const verifierRpc = `http://127.0.0.1:${portOf(a.verifierHttpAddr)}`;
const out: string[] = [];
const line = (s = "") => out.push(s);

line("op-cartesi devnet");
line("=================");
line();
line(`  L1 (anvil)        ${l1Rpc}        chain ${p.l1ChainId}`);
line(`  L2 (op-cartesi)   ${l2Rpc}        chain ${p.l2ChainId}`);
line(`  op-node           http://127.0.0.1:${stack.opNodeRpcPort}`);
if (stack.withBatcher) line(`  op-batcher        http://127.0.0.1:${stack.batcherRpcPort}`);
if (stack.withProposer && stack.withContracts) {
    line(`  op-proposer       http://127.0.0.1:${stack.proposerRpcPort}`);
}
if (stack.withVerifier) {
    line(`  verifier L2       ${verifierRpc}`);
    line(`  verifier op-node  http://127.0.0.1:${stack.verifierOpNodeRpcPort}`);
}
line();
line("panes");
line("-----");
line("  machine   the emulator's console: Linux booting, then guest stdout");
line("  guest     what the guest says about each transaction (its reports)");
line("  genesis   runs once and stays down; so do l1-contracts and outputs");
line("  outputs   does not autostart — press `s` to deploy the outputs suite");
line();
line("  up/down pick a pane, `s` start, `x` stop, `r` restart, `q` quit.");
line(`  Logs are also written to ${paths.logs}/<pane>.log.`);
line();
line("from another terminal");
line("---------------------");
line(`  cast block-number --rpc-url ${l2Rpc}`);
line(`  cast rpc cartesi_getOutputsRoot latest --rpc-url ${l2Rpc}`);
line(`  cast rpc optimism_syncStatus --rpc-url http://127.0.0.1:${stack.opNodeRpcPort}`);
if (stack.withVerifier) {
    line();
    line("  # the verifier sequences nothing, so it must agree block for block");
    line(`  cast block 10 --rpc-url ${l2Rpc} | grep -E 'hash|stateRoot'`);
    line(`  cast block 10 --rpc-url ${verifierRpc} | grep -E 'hash|stateRoot'`);
}
if (stack.withContracts) {
    line();
    line("  bun scripts/deposit.ts <address> <wei>   # deposit through OptimismPortal");
    line("  bun scripts/balance.ts <address>         # what the guest's ledger says");
}
console.log(out.join("\n"));
