#!/usr/bin/env bun
// Sends an L1 deposit that op-node derives into an L2 deposit transaction.
//
//   bun devnet/deposit.ts <recipient> [wei]
//
// With the contract suite deployed this is viem's own OP-stack deposit flow
// (https://viem.sh/op-stack/guides/deposits): walletActionsL1's
// depositTransaction against the portal the l2Chain definition names. On a
// contract-less devnet it installs a minimal emitter at the rollup config's
// deposit contract address instead — op-node's derivation reads the
// TransactionDeposited log, not the contract that produced it, so an emitter
// that logs the canonical event is indistinguishable from the portal as far
// as the chain is concerned. viem's action cannot drive the emitter (it
// speaks the portal ABI; the emitter reads its own raw calldata layout), but
// because the emitted event is canonical, both branches share the good part:
// getL2TransactionHashes derives the L2 deposit hash from the L1 receipt,
// and the script waits for the L2 inclusion instead of saying "check back
// in a few blocks".
//
// Requires the devnet to be running (./devnet/start-devnet.sh).

import {
    concat,
    encodeAbiParameters,
    encodePacked,
    getAddress,
    padHex,
    type Hex,
    type TransactionReceipt,
} from "viem";
import { getL2TransactionHashes, walletActionsL1 } from "viem/op-stack";
import { config, l1Public, l1Test, l1Wallet, l2Chain, l2Public, usage } from "./lib/env.ts";

// Runtime bytecode for the emitter. It logs TransactionDeposited with
// from = msg.sender, to = calldata word 0, version = 0, and data = the rest
// of the calldata, which is the caller's already-ABI-encoded opaqueData.
// Keeping the encoding on this side is what keeps the bytecode small enough
// to audit by eye:
//
//   CALLDATASIZE PUSH1 20 SWAP1 SUB PUSH1 20 PUSH1 00 CALLDATACOPY
//       -- memory[0:] = calldata[32:]
//   PUSH1 00                     -- topic4: version 0
//   PUSH1 00 CALLDATALOAD        -- topic3: to
//   CALLER                       -- topic2: from
//   PUSH32 <TransactionDeposited>-- topic1: event signature
//   CALLDATASIZE PUSH1 20 SWAP1 SUB PUSH1 00 LOG4 STOP
const TOPIC0 = "b3813568d9991fc951961fcb4c784893574240a28925604d09fc577c55bb7c32";
const EMITTER_CODE: Hex = `0x366020900360206000376000600035337f${TOPIC0}36602090036000a400`;

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg) usage("deposit.ts <recipient address> [wei]");
const to = getAddress(toArg);
const amount = BigInt(amountArg ?? "1000000000000000000");

const l1 = l1Public();
const wallet = l1Wallet(config.depositorKey);

/** Reports the L1 inclusion, then follows the deposit to L2: the derived
 * deposit-transaction hash comes from the receipt's canonical
 * TransactionDeposited log, which both branches emit. */
async function report(hash: Hex): Promise<void> {
    const receipt: TransactionReceipt = await l1.waitForTransactionReceipt({ hash });
    console.log(`L1 tx ${hash} in block ${receipt.blockNumber} status ${receipt.status}`);
    const [l2Hash] = getL2TransactionHashes(receipt);
    if (!l2Hash) {
        console.error("no TransactionDeposited log in the receipt");
        process.exit(1);
    }
    console.error(`  L2 deposit tx ${l2Hash}; waiting for derivation`);
    try {
        const l2Receipt = await l2Public().waitForTransactionReceipt({
            hash: l2Hash,
            pollingInterval: 2_000,
            timeout: 120_000,
        });
        console.log(`L2 tx ${l2Hash} in block ${l2Receipt.blockNumber} status ${l2Receipt.status}`);
    } catch {
        // The L1 deposit is done regardless; derivation just has not caught
        // up within the wait. The hash above is the one to watch.
        console.error("  not derived within 120s — the deposit stands, check the L2 hash later");
    }
}

const code = await l1.getCode({ address: config.depositContract });
if (code !== undefined && code !== "0x") {
    console.error(`depositing ${amount} wei to ${to} via OptimismPortal at ${config.depositContract}`);
    await report(
        await wallet.extend(walletActionsL1()).depositTransaction({
            request: { to, value: amount, mint: amount, gas: config.depositGasLimit },
            targetChain: l2Chain,
        }),
    );
    process.exit(0);
}

console.error(`no contract at ${config.depositContract}; installing the minimal emitter`);
await l1Test().setCode({ address: config.depositContract, bytecode: EMITTER_CODE });

// opaqueData for version 0, as OptimismPortal packs it:
//   mint (uint256) ‖ value (uint256) ‖ gasLimit (uint64) ‖ isCreation (bool) ‖ data
const opaque = encodePacked(
    ["uint256", "uint256", "uint64", "bool"],
    [amount, amount, config.depositGasLimit, false],
);
// The log's data field is the ABI encoding of that bytes value, since
// opaqueData is a non-indexed event parameter.
const calldata = concat([padHex(to, { size: 32 }), encodeAbiParameters([{ type: "bytes" }], [opaque])]);

console.error(`depositing ${amount} wei to ${to}`);
await report(await wallet.sendTransaction({ to: config.depositContract, data: calldata }));
