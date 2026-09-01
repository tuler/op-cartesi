// The application API: ABI-driven dispatch to registered callbacks, the
// EVM's exception-is-revert rule, the Revert/Fail choice an application makes
// over its own un-journaled RAM, the namespace guards, and the ABI drive as
// the discovery record.

import { KIND_APP, KIND_SYSTEM } from "@op-cartesi/abis";
import { CONFIG_ADDRESS, encodeEvmCall, errorRevert, L1BLOCK_ADDRESS } from "@op-cartesi/evm";
import {
    type Address,
    decodeFunctionResult,
    encodeFunctionData,
    type Hex,
    parseAbi,
    toBytes,
} from "viem";
import { describe, expect, it } from "vitest";
import { AccountsDriveError, Application, Fail, Revert } from "../src/index.ts";
import { block, CHAIN_ID, depositTx, owner, signedTx, user } from "./helpers.ts";

const COUNTER: Address = "0xc0de000000000000000000000000000000000001";
/** Somewhere for a callback's ledger effect to land, so a test can see
 * whether the journal kept it or rolled it back. */
const PAYEE: Address = "0x00000000000000000000000000000000000000e1";

const counterAbi = parseAbi([
    "function increment()",
    "function reset(uint64 to)",
    "function boom()",
    "function refuse()",
    "function halfDone()",
    "function driveRefusal()",
    "function count() view returns (uint256)",
]);

async function makeCounter() {
    const app = await Application.inMemory({ chainId: CHAIN_ID, owner: owner.address });

    // Application state is a variable. It is machine state — covered by the
    // state root, deterministic — but the journal does not reach it, which is
    // what the Revert/Fail choice below is about.
    let count = 0n;

    await app.contract({
        address: COUNTER,
        abi: counterAbi,
        transactions: {
            increment: () => {
                count += 1n;
            },
            reset: (to) => {
                count = to;
            },
            boom: async ({ ledger }) => {
                // The hazard, deliberately: writes RAM and the ledger, then
                // dies. The ledger half rolls back and the RAM half does not.
                count = 999n;
                await ledger.creditEther(PAYEE, 7n);
                throw new Error("kaboom");
            },
            refuse: async ({ ledger }) => {
                // Nothing of its own written yet, so a revert is honest.
                await ledger.creditEther(PAYEE, 7n);
                throw new Revert("politely declined");
            },
            halfDone: async ({ ledger }) => {
                // The same shape as boom, declared properly: Fail keeps both
                // halves rather than tearing them apart.
                count = 500n;
                await ledger.creditEther(PAYEE, 7n);
                throw new Fail("half done");
            },
            driveRefusal: () => {
                throw new AccountsDriveError("tableFull", "table at load limit");
            },
        },
        views: {
            count: () => count,
        },
    });
    return { app, read: () => count };
}

type NoArgFn = "increment" | "boom" | "refuse" | "halfDone" | "driveRefusal";

function calldata(functionName: NoArgFn): Hex {
    return encodeFunctionData({ abi: counterAbi, functionName });
}

/** The first report's tag byte and payload, as the shim would read them. */
function reportOf(emissions: { kind: string; payload?: Hex }[]): { tag: string; payload: Hex } {
    const r = emissions.find((e) => e.kind === "report");
    const hex = r!.payload!;
    return { tag: hex.slice(0, 4), payload: `0x${hex.slice(4)}` };
}

async function countOf(app: Application): Promise<bigint> {
    const res = await app.router.inspect(
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

async function fund(app: Application, who: Address): Promise<void> {
    await app.router.advance(block(), depositTx({ from: who, to: who, mint: 1000n, value: 1000n }));
}

describe("application contracts", () => {
    it("dispatches transactions and views by ABI, and state survives", async () => {
        const { app } = await makeCounter();
        await fund(app, user.address);

        const res = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("increment") }),
        );
        expect(res.outcome).toBe("accept");
        expect(await countOf(app)).toBe(1n);

        // Typed args flow through: reset(7).
        const reset = encodeFunctionData({ abi: counterAbi, functionName: "reset", args: [7n] });
        await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 1, data: reset }),
        );
        expect(await countOf(app)).toBe(7n);
    });

    it("an exception is a revert: the ledger rolls back, application RAM does not", async () => {
        const { app, read } = await makeCounter();
        await fund(app, user.address);

        const res = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("boom") }),
        );
        // The input is accepted — nonce and fee consumed, no free retry.
        expect(res.outcome).toBe("revert");
        // The journal's half is undone...
        expect((await app.ledger.account(PAYEE)).balance).toBe(0n);
        // ...and the application's own half is not. This is the hazard the
        // model makes explicit rather than papering over: a callback that
        // writes RAM before it can still throw must use Fail, not Revert.
        expect(read()).toBe(999n);
        expect((await app.ledger.account(user.address)).nonce).toBe(1n);
        // The revert data carries the exception message, Error(string)-coded.
        expect(reportOf(res.emissions)).toEqual({ tag: "0x02", payload: errorRevert("kaboom") });
    });

    it("a revert before the callback writes anything undoes the ledger cleanly", async () => {
        const { app, read } = await makeCounter();
        await fund(app, user.address);

        const res = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("refuse") }),
        );
        expect(res.outcome).toBe("revert");
        expect((await app.ledger.account(PAYEE)).balance).toBe(0n);
        expect(read()).toBe(0n);
        expect(reportOf(res.emissions)).toEqual({
            tag: "0x02",
            payload: errorRevert("politely declined"),
        });
    });

    it("Fail keeps both halves and still charges", async () => {
        const { app, read } = await makeCounter();
        await fund(app, user.address);

        const res = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("halfDone") }),
        );
        expect(res.outcome).toBe("fail");
        // Both halves stand — the point of Fail.
        expect((await app.ledger.account(PAYEE)).balance).toBe(7n);
        expect(read()).toBe(500n);
        expect((await app.ledger.account(user.address)).nonce).toBe(1n);
        // Tagged apart from a revert, because it means the opposite to a
        // caller reading the receipt.
        expect(reportOf(res.emissions)).toEqual({ tag: "0x03", payload: errorRevert("half done") });
    });

    it("a drive refusal inside an application reverts and charges, never rejects", async () => {
        const { app } = await makeCounter();
        await fund(app, user.address);

        const res = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("driveRefusal") }),
        );
        // Resource exhaustion used to escalate to reject, which left the
        // transaction free and infinitely replayable. Now the sender pays.
        expect(res.outcome).toBe("revert");
        expect(res.accept).toBe(true);
        expect((await app.ledger.account(user.address)).nonce).toBe(1n);
    });

    it("Revert carries chosen data; unknown functions and wrong sides revert", async () => {
        const { app } = await makeCounter();
        await fund(app, user.address);

        const refused = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 0, data: calldata("refuse") }),
        );
        expect(refused.outcome).toBe("revert");

        const unknown = await app.router.advance(
            block(),
            await signedTx(user, { to: COUNTER, nonce: 1, data: "0xdeadbeef" }),
        );
        expect(unknown.outcome).toBe("revert");

        // increment via eth_call: not a view.
        const wrongSide = await app.router.inspect(
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
        const { app } = await makeCounter();
        const entries = await app.contracts();
        const kinds = new Map(entries.map((e) => [e.address.toLowerCase(), e.kind]));
        expect(kinds.get(COUNTER.toLowerCase())).toBe(KIND_APP);
        expect(kinds.get(CONFIG_ADDRESS.toLowerCase())).toBe(KIND_SYSTEM);
        expect(kinds.get(L1BLOCK_ADDRESS.toLowerCase())).toBe(KIND_SYSTEM);
        const counter = entries.find((e) => e.address.toLowerCase() === COUNTER.toLowerCase());
        expect(JSON.parse(counter!.abi)).toEqual(counterAbi);
    });

    it("refuses reserved and already-routed addresses", async () => {
        const { app } = await makeCounter();
        const spec = { abi: counterAbi, transactions: {}, views: {} };
        await expect(app.contract({ ...spec, address: CONFIG_ADDRESS })).rejects.toThrowError(
            /reserved/,
        );
        await expect(app.contract({ ...spec, address: L1BLOCK_ADDRESS })).rejects.toThrowError(
            /reserved/,
        );
        await expect(app.contract({ ...spec, address: COUNTER })).rejects.toThrowError(
            /already routed/,
        );
    });
});
