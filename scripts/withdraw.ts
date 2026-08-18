#!/usr/bin/env bun

// Runs an ether withdrawal end to end, exactly as viem's withdrawal guide
// (viem.sh/op-stack/guides/withdrawals) writes it:
//
//   bun scripts/withdraw.ts <recipient> <wei>
//
// buildInitiateWithdrawal + initiateWithdrawal send the L2 transaction to
// the L2ToL1MessagePasser predeploy, which the routed guest of `app/`
// serves: it burns the value and emits the OP Withdrawal message whose hash
// enters the block's withdrawal trie — the header's withdrawalsRoot, the
// messagePasserStorageRoot of the output root op-proposer submits. The
// sender (SENDER_KEY) must hold the wei on L2; deposit first. From the
// receipt onward it is the stock flow every OP chain runs
// (scripts/lib/portal.ts), and the ETH comes out of the same lockbox the
// deposits funded.

import { config, usage } from "devnet/env";
import { l1Public, l2Public, l2Wallet } from "devnet/wallet";
import { getAddress } from "viem";
import { l2Sender, waitL2Receipt } from "./lib/l2.ts";
import { proveAndFinalizeWithdrawal } from "./lib/portal.ts";

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw.ts <recipient> <wei>");
const to = getAddress(toArg);
const amount = BigInt(amountArg);

const l1 = l1Public();
const l2 = l2Public();
const wallet = l2Wallet(config.senderKey);

// A sender whose balance cannot cover the value would be *rejected* by the
// guest — dropped without a receipt — so check up front and say so.
const sender = l2Sender();
const senderBalance = await l2.getBalance({ address: sender });
if (senderBalance < amount) {
    console.error(
        `sender ${sender} holds ${senderBalance} wei on L2, less than the ${amount} to withdraw — ` +
            `deposit first: bun scripts/deposit.ts ${sender} ${amount}`,
    );
    process.exit(1);
}

// Step 1 of the guide: build the initiation (the gas estimate on L1 becomes
// the withdrawal's L1 gas limit) and send it to the message passer.
console.error(`initiating a withdrawal of ${amount} wei to ${to} (sender ${sender})`);
const args = await l1.buildInitiateWithdrawal({ to, value: amount });
const txHash = await wallet.initiateWithdrawal(args);
console.error(`  L2 tx ${txHash}`);
const receipt = await waitL2Receipt(txHash);

const before = await l1.getBalance({ address: to });
await proveAndFinalizeWithdrawal(receipt);
const after = await l1.getBalance({ address: to });

console.error("");
console.error("withdrawal finalized through OptimismPortal");
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);
