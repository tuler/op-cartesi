#!/usr/bin/env bun

// Runs an ether withdrawal end to end: ask the guest for one, then prove and
// execute the voucher it emits.
//
//   bun devnet/withdraw.ts <recipient> <wei>
//
// This targets the routed guest (ledger-app): the withdrawal is a call to
// the bridge predeploy's withdrawEther(to), carrying the wei as msg.value —
// so the sender (SENDER_KEY) must hold that balance on L2; deposit first.
// The bridge burns the value and emits the voucher (EVM-COMPAT §9), and
// everything downstream is the outputs tree doing its job: the tree root
// goes into the block's withdrawalsRoot, op-proposer's claim commits to it,
// and Cartesi's own proof libraries verify the voucher against it on L1.

import { BRIDGE_ADDRESS, bridgeAbi } from "@cartesi/evm-compat";
import { encodeFunctionData, getAddress } from "viem";
import { l1Public, usage } from "./lib/env.ts";
import { sendL2Tx } from "./lib/l2.ts";
import { executeVoucher } from "./lib/voucher.ts";

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw.ts <recipient> <wei>");
const to = getAddress(toArg);
const amount = BigInt(amountArg);

console.error(`asking the guest to withdraw ${amount} wei to ${to}`);
const txHash = await sendL2Tx({
    to: BRIDGE_ADDRESS,
    data: encodeFunctionData({ abi: bridgeAbi, functionName: "withdrawEther", args: [to] }),
    value: amount,
});
console.error(`  L2 tx ${txHash}`);

const l1 = l1Public();
const before = await l1.getBalance({ address: to });
await executeVoucher(txHash);
const after = await l1.getBalance({ address: to });

console.error("");
console.error("withdrawal executed on L1");
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);
