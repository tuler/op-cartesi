// Sending an L2 transaction, the way a wallet would: viem fills the nonce
// through eth_getTransactionCount (the accounts drive), the fees through the
// gas methods the shim serves, and signs EIP-1559 — which the routed guest
// verifies. No --legacy, no invented nonce.

import { config } from "devnet/env";
import { l2Public, l2Wallet } from "devnet/wallet";
import type { Address, Hex } from "viem";

export async function sendL2Tx(tx: {
    to: Address;
    data?: Hex;
    value?: bigint;
    key?: string;
}): Promise<Hex> {
    const wallet = l2Wallet(tx.key ?? config.senderKey);
    return wallet.sendTransaction({ to: tx.to, data: tx.data, value: tx.value });
}

/** The address sendL2Tx signs with, for balance checks and messages. */
export function l2Sender(key?: string): Address {
    return l2Wallet(key ?? config.senderKey).account.address;
}

/** waitForTransactionReceipt with the failure mode of this chain spelled
 * out: an input the guest *rejects* (bad nonce, bad signature, balance not
 * covering fee and value) is dropped from the pool and never enters a
 * block, so no receipt ever appears — unlike a revert, which is included
 * with status 0. Waiting forever on that is a silent hang; a bounded wait
 * that names the likely cause is a diagnosis. */
export async function waitL2Receipt(hash: Hex, timeout = 60_000) {
    try {
        return await l2Public().waitForTransactionReceipt({
            hash,
            pollingInterval: 2_000,
            timeout,
        });
    } catch (e) {
        if (e instanceof Error && e.name === "WaitForTransactionReceiptTimeoutError") {
            throw new Error(
                `no receipt for ${hash} after ${timeout / 1000}s — the guest most likely ` +
                    `rejected the transaction (rejected inputs are dropped without a receipt). ` +
                    `Common causes: the sender's L2 balance does not cover value plus fee, or ` +
                    `a nonce mismatch. Check with: bun scripts/balance.ts ${l2Sender()}`,
            );
        }
        throw e;
    }
}
