#!/usr/bin/env bun
// Reads a balance out of the guest's ledger.
//
//   bun devnet/balance.ts <address> [l1-token]
//
// Ether comes back through eth_getBalance, which the shim answers with one
// machine.read_memory against the accounts drive — no execution. A token
// balance is an ERC-20 balanceOf against the token's derived L2 façade
// (EVM-COMPAT §6, §9), carried over eth_call. The shim does not build the
// EvmCall envelope yet (EVM-COMPAT §11's shim half), so this script encodes
// it client-side and passes it as raw call data, which today's shim hands to
// the guest's inspect unchanged; when the shim half lands, this collapses to
// a plain readContract.

import { decodeErrorResult, decodeFunctionResult, encodeFunctionData, getAddress, parseAbi, toBytes, type Hex } from "viem";
import { l2TokenAddress } from "ledger-app/addresses";
import { encodeEvmCall } from "ledger-app/evmcall";
import { erc20Abi } from "ledger-app/handlers/erc20";
import { config, l2Public, usage } from "./lib/env.ts";

const [whoArg, tokenArg] = process.argv.slice(2);
if (!whoArg) usage("balance.ts <address> [l1-token]");
const who = getAddress(whoArg);
const l2 = l2Public();

if (!tokenArg) {
    console.log((await l2.getBalance({ address: who })).toString());
    process.exit(0);
}

const facade = l2TokenAddress(getAddress(tokenArg));
const call = encodeEvmCall({
    chainId: BigInt(config.l2ChainId),
    from: who,
    to: facade,
    value: 0n,
    data: toBytes(encodeFunctionData({ abi: erc20Abi, functionName: "balanceOf", args: [who] })),
});
const { data } = await l2.call({ to: facade, data: call });
if (!data || data.length < 4) throw new Error(`inspect returned no report: ${data}`);

// One-byte report framing (EVM-COMPAT §7): 0x01 return data, 0x02 revert.
const tag = data.slice(0, 4);
const body: Hex = `0x${data.slice(4)}`;
if (tag === "0x01") {
    console.log(decodeFunctionResult({ abi: erc20Abi, functionName: "balanceOf", data: body }).toString());
} else if (tag === "0x02") {
    const err = decodeErrorResult({ abi: parseAbi(["error Error(string)"]), data: body });
    throw new Error(`reverted: ${err.args[0]}`);
} else {
    throw new Error(`unexpected report framing: ${data}`);
}
