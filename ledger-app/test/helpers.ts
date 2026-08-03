// Test rig: the router over an in-memory drive, plus transaction builders.
// Keys are the anvil dev accounts the devnet already uses (env.sh: the guest
// owner defaults to anvil #3).

import { keccak256, toBytes, toHex, type Address, type Hex, type TransactionSerializable } from "viem";
import { privateKeyToAccount } from "viem/accounts";
import { serializeTransaction as serializeOpStack } from "viem/op-stack";
import { Ledger } from "../src/ledger.ts";
import { Router } from "../src/router.ts";
import type { BlockContext, Emission } from "../src/types.ts";

export const CHAIN_ID = 901n;

export const OWNER_KEY: Hex = "0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6";
export const owner = privateKeyToAccount(OWNER_KEY); // 0x90F79bf6EB2c4f870365E785982E1f101E93b906

export const USER_KEY: Hex = "0x47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a";
export const user = privateKeyToAccount(USER_KEY); // 0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65

export const APP_CONTRACT: Address = "0xab7528bb862fb57e8a2bcd567a2e929a0be56a5e";

export async function makeRouter(): Promise<{ router: Router; ledger: Ledger }> {
    const ledger = await Ledger.inMemory();
    const router = new Router(ledger, { chainId: CHAIN_ID, owner: owner.address });
    return { router, ledger };
}

let blockCounter = 0n;

export function block(overrides: Partial<BlockContext> = {}): BlockContext {
    blockCounter += 1n;
    return {
        chainId: CHAIN_ID,
        appContract: APP_CONTRACT,
        blockNumber: blockCounter,
        timestamp: 1720000000n + blockCounter,
        prevRandao: 1n,
        inputIndex: blockCounter,
        ...overrides,
    };
}

/** A signed transaction as input bytes. Defaults to EIP-1559 — what wallets
 * actually send on a chain whose headers carry a base fee. */
export async function signedTx(
    account: ReturnType<typeof privateKeyToAccount>,
    tx: Partial<TransactionSerializable> & { to: Address; nonce: number },
): Promise<Uint8Array> {
    const serialized = await account.signTransaction({
        type: "eip1559",
        chainId: Number(CHAIN_ID),
        gas: 1_000_000n,
        maxFeePerGas: 1n,
        maxPriorityFeePerGas: 0n,
        value: 0n,
        ...tx,
    } as TransactionSerializable);
    return toBytes(serialized);
}

export async function signedLegacyTx(
    account: ReturnType<typeof privateKeyToAccount>,
    tx: Partial<TransactionSerializable> & { to: Address; nonce: number },
    withChainId = true,
): Promise<Uint8Array> {
    const serialized = await account.signTransaction({
        type: "legacy",
        ...(withChainId ? { chainId: Number(CHAIN_ID) } : {}),
        gas: 1_000_000n,
        gasPrice: 1n,
        value: 0n,
        ...tx,
    } as TransactionSerializable);
    return toBytes(serialized);
}

let sourceCounter = 0;

/** An OP deposit transaction (type 0x7e) as input bytes. */
export function depositTx(d: {
    from: Address;
    to?: Address;
    mint?: bigint;
    value?: bigint;
    data?: Hex;
}): Uint8Array {
    sourceCounter++;
    return toBytes(
        serializeOpStack({
            type: "deposit",
            sourceHash: keccak256(toHex(`source-${sourceCounter}`)),
            from: d.from,
            to: d.to,
            mint: d.mint ?? 0n,
            value: d.value ?? 0n,
            gas: 1_000_000n,
            isSystemTx: false,
            data: d.data,
        }),
    );
}

export function notices(emissions: Emission[]): Extract<Emission, { kind: "notice" }>[] {
    return emissions.filter((e) => e.kind === "notice");
}

export function vouchers(emissions: Emission[]): Extract<Emission, { kind: "voucher" }>[] {
    return emissions.filter((e) => e.kind === "voucher");
}

export function reports(emissions: Emission[]): Extract<Emission, { kind: "report" }>[] {
    return emissions.filter((e) => e.kind === "report");
}

/** The drive image, for byte-identity assertions. */
export function driveBytes(ledger: Ledger): Uint8Array {
    return (ledger.backing as { bytes: Uint8Array }).bytes.slice();
}
