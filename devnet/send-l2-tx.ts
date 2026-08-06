#!/usr/bin/env bun
// Sends one L2 transaction — the general form of what the old send-l2-tx.sh
// did for one hardcoded recipient.
//
//   bun devnet/send-l2-tx.ts <to> [calldata] [value-wei]
//
// The sender defaults to SENDER_KEY (anvil account 3, the guest owner).
// Prints the transaction hash on stdout.

import { getAddress, isHex } from "viem";
import { usage } from "./lib/env.ts";
import { sendL2Tx } from "./lib/l2.ts";

const [toArg, dataArg, valueArg] = process.argv.slice(2);
if (!toArg) usage("send-l2-tx.ts <to> [calldata] [value-wei]");
const to = getAddress(toArg);
const data = dataArg === undefined || dataArg === "" ? undefined : dataArg;
if (data !== undefined && !isHex(data)) usage("calldata must be 0x-prefixed hex");
const value = valueArg === undefined ? undefined : BigInt(valueArg);

console.log(await sendL2Tx({ to, data, value }));
