#!/usr/bin/env bun

// Runs an ether withdrawal end to end, through the standard OP Stack path:
// ask the guest for one, then prove and finalize it against OptimismPortal.
//
//   bun scripts/withdraw.ts <recipient> <wei>
//
// This targets the routed guest of `app/`: the withdrawal is a call to the
// bridge predeploy's withdrawEther(to), carrying the wei as msg.value — so
// the sender (SENDER_KEY) must hold that balance on L2; deposit first. The
// bridge burns the value and emits an OP Withdrawal message; the hash's
// sentMessages slot enters the block's withdrawal trie, whose root is the
// header's withdrawalsRoot — the messagePasserStorageRoot of the output root
// op-proposer submits. From there it is the stock flow every OP chain runs
// (scripts/lib/portal.ts), and the ETH comes out of the same lockbox the
// deposits funded.

import { BRIDGE_ADDRESS, bridgeAbi, decodeWithdrawalOutput } from "@op-cartesi/evm";
import { usage } from "devnet/env";
import { l1Public, l2Public } from "devnet/wallet";
import { encodeFunctionData, getAddress } from "viem";
import { sendL2Tx } from "./lib/l2.ts";
import { proveAndFinalizeWithdrawal } from "./lib/portal.ts";

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw.ts <recipient> <wei>");
const to = getAddress(toArg);
const amount = BigInt(amountArg);

const l1 = l1Public();
const l2 = l2Public();

console.error(`asking the guest to withdraw ${amount} wei to ${to}`);
const txHash = await sendL2Tx({
    to: BRIDGE_ADDRESS,
    data: encodeFunctionData({ abi: bridgeAbi, functionName: "withdrawEther", args: [to] }),
    value: amount,
});
console.error(`  L2 tx ${txHash}`);

const receipt = await l2.waitForTransactionReceipt({ hash: txHash, pollingInterval: 2_000 });
const emissions = await l2.getTransactionEmissions({ hash: txHash });
const message = emissions.outputs
    .map((o) => decodeWithdrawalOutput(o.data))
    .find((w) => w !== undefined);
if (!message) throw new Error("the transaction emitted no withdrawal message");

const before = await l1.getBalance({ address: to });
await proveAndFinalizeWithdrawal(message, receipt.blockNumber);
const after = await l1.getBalance({ address: to });

console.error("");
console.error("withdrawal finalized through OptimismPortal");
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);
