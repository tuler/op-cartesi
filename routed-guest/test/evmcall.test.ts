// The inspect surface: EvmCall return/revert framing, EOA semantics,
// EvmSimulate's no-mutation guarantee, and the adopted L1Block predeploy.

import { describe, expect, it } from "vitest";
import { concat, encodeFunctionData, toBytes, toFunctionSelector, toHex, type Hex } from "viem";
import { L1BLOCK_ADDRESS, l1BlockAbi } from "@cartesi/evm-compat";
import { encodeEvmCall } from "@cartesi/evm-compat";
import { block, CHAIN_ID, depositTx, driveBytes, makeRouter, user } from "./helpers.ts";

const EOA = "0x00000000000000000000000000000000000000aa";

// op-node's canonical L1-attributes depositor.
const L1_INFO_DEPOSITOR = "0xdeaddeaddeaddeaddeaddeaddeaddeaddead0001";

function call(to: `0x${string}`, data: Uint8Array, simulate = false, value = 0n): Uint8Array {
    return toBytes(encodeEvmCall({ chainId: CHAIN_ID, from: user.address, to, value, data, simulate }));
}

describe("EvmCall", () => {
    it("returns empty for an EOA, like eth_call", async () => {
        const { router } = await makeRouter();
        const res = await router.inspect(call(EOA, new Uint8Array(0)));
        expect(res.accept).toBe(true);
        expect(res.reports).toEqual(["0x01"]); // tag only, empty return
    });

    it("rejects a non-EvmCall query with revert framing", async () => {
        const { router } = await makeRouter();
        const res = await router.inspect(new Uint8Array([1, 2, 3]));
        expect(res.accept).toBe(false);
        expect(res.reports[0]!.slice(0, 4)).toBe("0x02");
    });

    it("EvmSimulate reports success without mutating the drive", async () => {
        const { router, ledger } = await makeRouter();
        await router.advance(block(), depositTx({ from: user.address, to: user.address, mint: 100n, value: 100n }));
        const before = driveBytes(ledger);
        const res = await router.inspect(call(EOA, new Uint8Array(0), true, 40n));
        expect(res.accept).toBe(true);
        // Buffer.compare, not toEqual: deep-diffing a 1 MiB array is what
        // vitest timeouts are made of.
        expect(Buffer.compare(Buffer.from(driveBytes(ledger)), Buffer.from(before))).toBe(0);
        expect((await ledger.account(EOA)).balance).toBe(0n);
    });

    it("EvmSimulate surfaces a would-be failure as revert", async () => {
        const { router } = await makeRouter();
        // No funding: the simulated value transfer cannot cover.
        const res = await router.inspect(call(EOA, new Uint8Array(0), true, 40n));
        expect(res.accept).toBe(false);
        expect(res.reports[0]!.slice(0, 4)).toBe("0x02");
    });
});

describe("L1Block", () => {
    /** The Ecotone packed attributes: what op-node deposits every block. */
    function attributesCalldata(): Hex {
        const body = new Uint8Array(160);
        const view = new DataView(body.buffer);
        view.setUint32(0, 1368); // baseFeeScalar
        view.setUint32(4, 810949); // blobBaseFeeScalar
        view.setBigUint64(8, 4n); // sequenceNumber
        view.setBigUint64(16, 1720009999n); // timestamp
        view.setBigUint64(24, 123456n); // number
        body[63] = 7; // basefee = 7
        body[95] = 1; // blobBaseFee = 1
        body.fill(0xaa, 96, 128); // hash
        body.fill(0xbb, 128, 160); // batcherHash
        return concat([toFunctionSelector("setL1BlockValuesEcotone()"), toHex(body)]);
    }

    it("stores the attributes deposit and serves the standard views", async () => {
        const { router } = await makeRouter();
        const res = await router.advance(
            block(),
            depositTx({ from: L1_INFO_DEPOSITOR, to: L1BLOCK_ADDRESS, data: attributesCalldata() }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");

        const num = await router.inspect(
            call(L1BLOCK_ADDRESS, toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "number" }))),
        );
        expect(num.accept).toBe(true);
        expect(BigInt(`0x${num.reports[0]!.slice(4)}`)).toBe(123456n);

        const ts = await router.inspect(
            call(L1BLOCK_ADDRESS, toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "timestamp" }))),
        );
        expect(BigInt(`0x${ts.reports[0]!.slice(4)}`)).toBe(1720009999n);

        const basefee = await router.inspect(
            call(L1BLOCK_ADDRESS, toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "basefee" }))),
        );
        expect(BigInt(`0x${basefee.reports[0]!.slice(4)}`)).toBe(7n);
    });

    it("reverts views before the first attributes deposit", async () => {
        const { router } = await makeRouter();
        const res = await router.inspect(
            call(L1BLOCK_ADDRESS, toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "number" }))),
        );
        expect(res.accept).toBe(false);
        expect(res.reports[0]!.slice(0, 4)).toBe("0x02");
    });
});
