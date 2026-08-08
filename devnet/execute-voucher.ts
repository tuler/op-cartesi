#!/usr/bin/env bun
// Takes a voucher the guest emitted and runs it on L1.
//
//   bun devnet/execute-voucher.ts <L2 tx hash>
//
// Prints the executed output's chain-wide index on stdout. The mechanics —
// find the output, wait for a proposal, open its root claim, prove and
// execute — live in lib/voucher.ts, shared with the withdraw scripts.

import type { Hex } from "viem";
import { usage } from "./lib/env.ts";
import { executeVoucher } from "./lib/voucher.ts";

function isTxHash(v: string): v is Hex {
    return /^0x[0-9a-f]{64}$/i.test(v);
}

const [txArg] = process.argv.slice(2);
if (!txArg || !isTxHash(txArg)) usage("execute-voucher.ts <L2 tx hash>");

console.log((await executeVoucher(txArg)).toString());
