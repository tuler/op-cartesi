#!/usr/bin/env bun

// Sends an L1 deposit that op-node derives into an L2 deposit transaction —
// viem's own OP-stack deposit flow
// (https://viem.sh/op-stack/guides/deposits): depositTransaction against the
// portal the l2Chain definition names, then getL2TransactionHashes to derive
// the L2 deposit hash from the L1 receipt's TransactionDeposited log, and a
// wait for the L2 inclusion.
//
//   bun scripts/deposit.ts <recipient> [wei]
//
// Requires the devnet to be running with the contract suite deployed
// (./devnet/start-devnet.ts, the WITH_CONTRACTS=1 default): the portal is
// the deposit path a real user takes, and the only one — the contract-less
// mode has no deposits.

import { config, usage } from "devnet/env";
import { l1Public, l1Wallet, l2Chain, l2Public } from "devnet/wallet";
import { getAddress, type TransactionReceipt } from "viem";
import { getL2TransactionHashes } from "viem/op-stack";

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg) usage("deposit.ts <recipient address> [wei]");
const to = getAddress(toArg);
const amount = BigInt(amountArg ?? "1000000000000000000");

const l1 = l1Public();
const wallet = l1Wallet(config.depositorKey);

const code = await l1.getCode({ address: config.depositContract });
if (code === undefined || code === "0x") {
    console.error(
        `no portal at ${config.depositContract} — this devnet has no contracts (WITH_CONTRACTS=0?); ` +
            `start it with the WITH_CONTRACTS=1 default`,
    );
    process.exit(1);
}

console.error(`depositing ${amount} wei to ${to} via OptimismPortal at ${config.depositContract}`);
const hash = await wallet.depositTransaction({
    request: { to, value: amount, mint: amount, gas: config.depositGasLimit },
    targetChain: l2Chain,
});
const receipt: TransactionReceipt = await l1.waitForTransactionReceipt({ hash });
console.log(`L1 tx ${hash} in block ${receipt.blockNumber} status ${receipt.status}`);

// Follow the deposit across the bridge: the derived deposit-transaction hash
// comes from the receipt's TransactionDeposited log.
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
    // The L1 deposit is done regardless; derivation just has not caught up
    // within the wait. The hash above is the one to watch.
    console.error("  not derived within 120s — the deposit stands, check the L2 hash later");
}
