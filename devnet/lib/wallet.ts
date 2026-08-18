// The chains and the clients every transactional script talks through.
//
// Split from lib/env.ts, which answers "what is configured": this answers
// "what do I call". The two halves were one file and read as one, but only
// this one pulls in viem and the cartesi_ extension, and only this one needs
// the chain definitions — so a proc that wants a port no longer imports a
// wallet.
//
// The clients come pre-extended with viem's op-stack surface on both sides —
// L1 actions on the L1 pair (deposits, waitToProve, proveWithdrawal,
// finalizeWithdrawal), L2 actions on the L2 pair (initiateWithdrawal,
// buildProveWithdrawal) — plus the cartesi_ namespace on the L2 public
// client (lib/cartesi.ts). The stock withdrawal guide
// (viem.sh/op-stack/guides/withdrawals) runs against these clients as-is.

import {
    type Chain,
    createPublicClient,
    createWalletClient,
    defineChain,
    type Hex,
    http,
    rpcSchema,
} from "viem";
import { privateKeyToAccount } from "viem/accounts";
import { publicActionsL1, publicActionsL2, walletActionsL1, walletActionsL2 } from "viem/op-stack";
import { type CartesiRpcSchema, cartesiActions } from "./cartesi.ts";
import { config } from "./env.ts";
import { MULTICALL3_ADDRESS } from "./multicall3.ts";

/** The guard earns viem's template-literal type by inspection, the same
 * doctrine as the router's tx.ts: no casts, checked at runtime. */
function isPrivateKey(v: string): v is Hex {
    return /^0x[0-9a-f]{64}$/.test(v);
}

export function asKey(key: string): Hex {
    const k = key.toLowerCase();
    if (!isPrivateKey(k)) throw new Error(`not a 32-byte hex private key: ${key}`);
    return k;
}

export const l1Chain: Chain = defineChain({
    id: config.l1ChainId,
    name: "devnet-l1",
    nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
    rpcUrls: { default: { http: [config.l1Rpc] } },
    contracts: {
        // Placed by deploy-l1.ts (anvil_setCode); viem's multicall — which
        // getGames runs under waitToProve — resolves it from here.
        multicall3: { address: MULTICALL3_ADDRESS },
    },
});

// The L2 chain carries the OP-stack shape viem's op-stack actions resolve
// against: sourceId names the L1, and contracts are keyed by it — so
// `targetChain: l2Chain` finds the portal without every script naming an
// address. Deliberately not annotated `: Chain`: the widened type would
// erase the contracts entry the targetChain parameter's type requires.
export const l2Chain = defineChain({
    id: config.l2ChainId,
    name: "op-cartesi-devnet",
    nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
    rpcUrls: { default: { http: [config.l2Rpc] } },
    sourceId: config.l1ChainId,
    contracts: {
        portal: { [config.l1ChainId]: { address: config.depositContract } },
        // Unconditional because viem's targetChain type requires the pair;
        // before the contracts are deployed there is no factory to name, and
        // every script that would reach for it refuses first.
        disputeGameFactory: {
            [config.l1ChainId]: {
                address: config.disputeGameFactory ?? "0x0000000000000000000000000000000000000000",
            },
        },
    },
});

export function l1Public(rpc?: string) {
    return createPublicClient({
        chain: l1Chain,
        transport: http(rpc ?? config.l1Rpc),
    }).extend(publicActionsL1());
}

export function l1Wallet(key: string, rpc?: string) {
    return createWalletClient({
        chain: l1Chain,
        transport: http(rpc ?? config.l1Rpc),
        account: privateKeyToAccount(asKey(key)),
    }).extend(walletActionsL1());
}

export function l2Public(rpc?: string) {
    return createPublicClient({
        chain: l2Chain,
        transport: http(rpc ?? config.l2Rpc),
        rpcSchema: rpcSchema<CartesiRpcSchema>(),
    })
        .extend(cartesiActions())
        .extend(publicActionsL2());
}

export function l2Wallet(key: string) {
    return createWalletClient({
        chain: l2Chain,
        transport: http(),
        account: privateKeyToAccount(asKey(key)),
    }).extend(walletActionsL2());
}
