#!/usr/bin/env bun
// Lists the guest's interface surface: what the chain speaks, straight from
// the machine's drives via cartesi_getContracts.
//
//   bun devnet/contracts.ts
//
// Recorded contracts print their ABI as human-readable signatures; token
// façades print the L1 token they serve (their shared ABI is the standard's
// erc20FacadeAbi).

import type { AbiFunction } from "viem";
import { usage } from "./lib/env.ts";
import { l2Public } from "./lib/wallet.ts";

if (process.argv[2] === "-h" || process.argv[2] === "--help") usage("contracts.ts");

const l2 = l2Public();
const { blockNumber, contracts } = await l2.getContracts();

console.log(`contracts as of block ${Number(blockNumber)}:`);
for (const c of contracts) {
    console.log(`\n  ${c.address}  (${c.kind})`);
    if (c.kind === "token" && c.l1Token) {
        console.log(`    façade of L1 token ${c.l1Token} — standard ERC-20 surface`);
        continue;
    }
    for (const item of c.abi ?? []) {
        if (item.type !== "function") continue;
        const fn: AbiFunction = item;
        const args = fn.inputs.map((i) => `${i.type}${i.name ? ` ${i.name}` : ""}`).join(", ");
        const rets = fn.outputs.map((o) => o.type).join(", ");
        const mut = fn.stateMutability === "nonpayable" ? "" : ` ${fn.stateMutability}`;
        console.log(`    function ${fn.name}(${args})${mut}${rets ? ` returns (${rets})` : ""}`);
    }
}
