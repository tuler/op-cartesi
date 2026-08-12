#!/usr/bin/env bun

// Runs an ERC-20 withdrawal end to end.
//
//   bun devnet/withdraw-erc20.ts <recipient> <amount> [l1-token]
//
// This targets the routed guest of `app/`: the withdrawal is a call to
// the bridge predeploy's withdrawERC20, naming the token by its derived L2
// façade address and debiting the sender's own holding (EVM-COMPAT §9) — so
// the sender (SENDER_KEY) must hold the tokens on L2; deposit first. The
// only difference from the ether case is what the voucher says: there the
// call is "send the recipient this much ether", here "token, transfer this
// much to the recipient" — one call from the application contract, which has
// held the tokens since the deposit. Everything downstream is identical,
// because to the outputs tree a voucher is a voucher.
//
// This is the direction OP's standard bridge cannot serve on this chain: its
// release path runs through OptimismPortal.proveWithdrawalTransaction, which
// wants an MPT proof against an L2 state root that here is a Cartesi hash
// tree. See DESIGN §7d.

import { BRIDGE_ADDRESS, bridgeAbi, l2TokenAddress } from "@op-cartesi/evm";
import { encodeFunctionData, getAddress, parseAbi } from "viem";
import { config, usage } from "./lib/env.ts";
import { sendL2Tx } from "./lib/l2.ts";
import { executeVoucher } from "./lib/voucher.ts";
import { l1Public } from "./lib/wallet.ts";

const balanceOfAbi = parseAbi(["function balanceOf(address owner) view returns (uint256)"]);

const [toArg, amountArg, tokenArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw-erc20.ts <recipient> <amount> [l1-token]");
const to = getAddress(toArg);
const amount = BigInt(amountArg);
const token = tokenArg ? getAddress(tokenArg) : config.testToken;
if (!token) {
    console.error(
        "no token given and no TEST_TOKEN_ADDRESS; run bun devnet/deposit-erc20.ts first",
    );
    process.exit(1);
}

console.error(`asking the guest to withdraw ${amount} of ${token} to ${to}`);
const txHash = await sendL2Tx({
    to: BRIDGE_ADDRESS,
    data: encodeFunctionData({
        abi: bridgeAbi,
        functionName: "withdrawERC20",
        args: [l2TokenAddress(token), to, amount],
    }),
});
console.error(`  L2 tx ${txHash}`);

const l1 = l1Public();
const balance = () =>
    l1.readContract({ address: token, abi: balanceOfAbi, functionName: "balanceOf", args: [to] });
const before = await balance();
await executeVoucher(txHash);
const after = await balance();

console.error("");
console.error("withdrawal executed on L1");
console.error(`  token     ${token}`);
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);
