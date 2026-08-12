#!/usr/bin/env bun

// Reads a balance out of the guest's ledger.
//
//   bun devnet/balance.ts <address> [l1-token]
//
// Ether comes back through eth_getBalance, which the shim answers with one
// machine.read_memory against the accounts drive — no execution. A token
// balance is a plain readContract balanceOf against the token's derived L2
// façade (EVM-COMPAT §6, §9): the shim's eth_call wraps the call in the
// EvmCall envelope and unwraps the guest's tagged reports, so viem sees an
// ordinary ERC-20 — reverts included, surfaced as execution-reverted errors
// with the reason.

import { l2TokenAddress } from "@op-cartesi/evm";
import { erc20Abi, getAddress } from "viem";
import { usage } from "./lib/env.ts";
import { l2Public } from "./lib/wallet.ts";

const [whoArg, tokenArg] = process.argv.slice(2);
if (!whoArg) usage("balance.ts <address> [l1-token]");
const who = getAddress(whoArg);
const l2 = l2Public();

if (!tokenArg) {
    console.log((await l2.getBalance({ address: who })).toString());
} else {
    const balance = await l2.readContract({
        address: l2TokenAddress(getAddress(tokenArg)),
        abi: erc20Abi,
        functionName: "balanceOf",
        args: [who],
    });
    console.log(balance.toString());
}
