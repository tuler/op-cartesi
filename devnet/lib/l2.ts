// Sending an L2 transaction, the way a wallet would: viem fills the nonce
// through eth_getTransactionCount (the accounts drive), the fees through the
// gas methods the shim serves, and signs EIP-1559 — which the routed guest
// verifies. No --legacy, no invented nonce.

import type { Address, Hex } from "viem";
import { config, l2Wallet } from "./env.ts";

export async function sendL2Tx(tx: {
    to: Address;
    data?: Hex;
    value?: bigint;
    key?: string;
}): Promise<Hex> {
    const wallet = l2Wallet(tx.key ?? config.senderKey);
    return wallet.sendTransaction({ to: tx.to, data: tx.data, value: tx.value });
}
