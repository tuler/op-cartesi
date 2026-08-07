// The application API: ABI-driven dispatch to registered callbacks, the
// EVM's exception-is-revert rule, revert-safe app state through the shared
// journal, the namespace guards, and the ABI drive as the discovery record.

import { KIND_APP, KIND_SYSTEM } from "@cartesi/abis";
import {
    CONFIG_ADDRESS,
    L1BLOCK_ADDRESS,
    encodeEvmCall,
    errorRevert,
} from "@cartesi/evm-compat";
import { decodeFunctionResult, encodeFunctionData, parseAbi, toBytes, type Address, type Hex } from "viem";
import { describe, expect, it } from "vitest";
import { Guest, Revert } from "../src/index.ts";
import { CHAIN_ID, block, depositTx, owner, signedTx, user } from "./helpers.ts";

const COUNTER: Address = "0xc0de000000000000000000000000000000000001";

const counterAbi = parseAbi([
    "function increment()",
    "function reset(uint64 to)",
    "function boom()",
    "function refuse()",
    "function count() view returns (uint256)",
]);

async function makeCounter() {
    const guest = await Guest.inMemory({ chainId: CHAIN_ID, owner: owner.address });
    const state = guest.store(8);

    const read = async () => {
        const b = await state.readAt(0, 8);
        return new DataView(b.buffer, b.byteOffset, b.byteLength).getBigUint64(0, true);
    };
    const write = async (v: bigint) => {
        const b = new Uint8Array(8);
        new DataView(b.buffer).setBigUint64(0, v, true);
        await state.writeAt(0, b);
    };

    await guest.contract({
        address: COUNTER,
        abi: counterAbi,
        transactions: {
            increment: async () => {
                await write((await read()) + 1n);
            },
            reset: async ([to]) => {
                await write(to);
            },
            boom: async () => {
                // Writes, then dies: the write must roll back with the revert.
                await write(999n);
                throw new Error("kaboom");
            },
            refuse: () => {
                throw new Revert("politely declined");
            },
        },
        views: {
            count: async () => read(),
        },
    });
    return { guest, read };
}

function calldata(functionName: "increment" | "boom" | "refuse"): Hex {
    return encodeFunctionData({ abi: counterAbi, functionName });
}

async function countOf(guest: Guest): Promise<bigint> {
    const res = await guest.router.inspect(
        toBytes(
            encodeEvmCall({
                chainId: CHAIN_ID,
                from: user.address,
                to: COUNTER,
                value: 0n,
                data: toBytes(encodeFunctionData({ abi: counterAbi, functionName: "count" })),
            }),
        ),
    );
    expect(res.accept).toBe(true);
    const returned: Hex = `0x${res.reports[0]!.slice(4)}`;
    return decodeFunctionResult({ abi: counterAbi, functionName: "count", data: returned });
}

async function fund(guest: Guest, who: Address): Promise<void> {
    await guest.router.advance(block(), depositTx({ from: who, to: who, mint: 1000n, value: 1000n }));
}

describe("application contracts", () => {
    it("dispatches transactions and views by ABI, and state survives", async () => {
        const { guest } = await makeCounter();
        await fund(guest, user.address);

        const res = await guest.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("increment") }),
        );
        expect(res.outcome).toBe("accept");
        expect(await countOf(guest)).toBe(1n);

        // Typed args flow through: reset(7).
        const reset = encodeFunctionData({ abi: counterAbi, functionName: "reset", args: [7n] });
        await guest.router.advance(block(), await signedTx(user, { to: COUNTER, nonce: 1, data: reset }));
        expect(await countOf(guest)).toBe(7n);
    });

    it("an exception is a revert, and journaled app state rolls back", async () => {
        const { guest, read } = await makeCounter();
        await fund(guest, user.address);
        await guest.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("increment") }),
        );

        const res = await guest.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 1, data: calldata("boom") }),
        );
        // The input is accepted (fee and nonce consumed), the effect undone.
        expect(res.outcome).toBe("revert");
        expect(await read()).toBe(1n);
        // The revert data carries the exception message, Error(string)-coded.
        const revertReport = res.emissions.find((e) => e.kind === "report");
        expect(revertReport?.payload).toBe(`0x02${errorRevert("kaboom").slice(2)}`);
    });

    it("Revert carries chosen data; unknown functions and wrong sides revert", async () => {
        const { guest } = await makeCounter();
        await fund(guest, user.address);

        const refused = await guest.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("refuse") }),
        );
        expect(refused.outcome).toBe("revert");

        const unknown = await guest.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 1, data: "0xdeadbeef" }),
        );
        expect(unknown.outcome).toBe("revert");

        // increment via eth_call: not a view.
        const wrongSide = await guest.router.inspect(
            toBytes(
                encodeEvmCall({
                    chainId: CHAIN_ID,
                    from: user.address,
                    to: COUNTER,
                    value: 0n,
                    data: toBytes(calldata("increment")),
                }),
            ),
        );
        expect(wrongSide.accept).toBe(false);
    });

    it("records the manifest in the ABI drive: built-ins as system, apps as apps", async () => {
        const { guest } = await makeCounter();
        const entries = await guest.contracts();
        const kinds = new Map(entries.map((e) => [e.address.toLowerCase(), e.kind]));
        expect(kinds.get(COUNTER.toLowerCase())).toBe(KIND_APP);
        expect(kinds.get(CONFIG_ADDRESS.toLowerCase())).toBe(KIND_SYSTEM);
        expect(kinds.get(L1BLOCK_ADDRESS.toLowerCase())).toBe(KIND_SYSTEM);
        const counter = entries.find((e) => e.address.toLowerCase() === COUNTER.toLowerCase());
        expect(JSON.parse(counter!.abi)).toEqual(counterAbi);
    });

    it("refuses reserved and already-routed addresses", async () => {
        const { guest } = await makeCounter();
        const spec = { abi: counterAbi, transactions: {}, views: {} };
        await expect(guest.contract({ ...spec, address: CONFIG_ADDRESS })).rejects.toThrowError(/reserved/);
        await expect(guest.contract({ ...spec, address: L1BLOCK_ADDRESS })).rejects.toThrowError(/reserved/);
        await expect(guest.contract({ ...spec, address: COUNTER })).rejects.toThrowError(/already routed/);
    });
});
