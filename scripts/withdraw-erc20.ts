#!/usr/bin/env bun

// Runs an ERC-20 withdrawal end to end, through the standard OP bridge.
//
//   bun scripts/withdraw-erc20.ts <recipient> <amount> [l1-token]
//
// This targets the routed guest of `app/` with the standard messenger
// registered (DESIGN §7g): the withdrawal is a call to the L2StandardBridge
// predeploy's bridgeERC20To, naming the token by its derived L2 façade —
// so the sender (SENDER_KEY) must hold the tokens on L2; deposit through
// L1StandardBridge first. The guest debits the ledger and sends
// finalizeBridgeERC20 to L1StandardBridge through the L2 messenger; the
// message rides an OP Withdrawal whose sender is the messenger predeploy,
// which is exactly what L1CrossDomainMessenger requires of the portal's
// l2Sender. Proving and finalizing (scripts/lib/portal.ts) then makes the
// L1 bridge release its own escrow — no voucher, no executor, no custom
// contract anywhere on the path.

import {
    decodeWithdrawalOutput,
    L2_BRIDGE_ADDRESS,
    l2StandardBridgeAbi,
    l2TokenAddress,
} from "@op-cartesi/evm";
import { config, usage } from "devnet/env";
import { l1Public, l2Public } from "devnet/wallet";
import { encodeFunctionData, getAddress, parseAbi } from "viem";
import { l2Sender, sendL2Tx, waitL2Receipt } from "./lib/l2.ts";
import { proveAndFinalizeWithdrawal } from "./lib/portal.ts";

const balanceOfAbi = parseAbi(["function balanceOf(address owner) view returns (uint256)"]);

const [toArg, amountArg, tokenArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw-erc20.ts <recipient> <amount> [l1-token]");
const to = getAddress(toArg);
const amount = BigInt(amountArg);
const token = tokenArg ? getAddress(tokenArg) : config.testToken;
if (!token) {
    console.error(
        "no token given and no TEST_TOKEN_ADDRESS; run bun scripts/deposit-erc20.ts first",
    );
    process.exit(1);
}

const l1 = l1Public();
const l2 = l2Public();

// The bridge call would *revert* on a short token balance (visible in the
// receipt), but a sender with no ether at all cannot pay the fee and gets
// *rejected* — dropped without a receipt — so name the sender up front.
const sender = l2Sender();

console.error(`asking the guest to bridge ${amount} of ${token} to ${to} (sender ${sender})`);
const txHash = await sendL2Tx({
    to: L2_BRIDGE_ADDRESS,
    data: encodeFunctionData({
        abi: l2StandardBridgeAbi,
        functionName: "bridgeERC20To",
        args: [l2TokenAddress(token), token, to, amount, 400_000, "0x"],
    }),
});
console.error(`  L2 tx ${txHash}`);

const receipt = await waitL2Receipt(txHash);
const emissions = await l2.getTransactionEmissions({ hash: txHash });
const message = emissions.outputs
    .map((o) => decodeWithdrawalOutput(o.data))
    .find((w) => w !== undefined);
if (!message) throw new Error("the transaction emitted no withdrawal message");

const balance = () =>
    l1.readContract({ address: token, abi: balanceOfAbi, functionName: "balanceOf", args: [to] });
const before = await balance();
await proveAndFinalizeWithdrawal(message, receipt.blockNumber);
const after = await balance();

console.error("");
console.error("withdrawal finalized through L1StandardBridge");
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);
