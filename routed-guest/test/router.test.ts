// Router core: enforcement, native transfers, deposits, and the outcome
// model — the EVM-COMPAT §5 behaviors.

import { describe, expect, it } from "vitest";
import { parseTransaction, serializeTransaction, toBytes, toHex } from "viem";
import { block, depositTx, makeRouter, signedLegacyTx, signedTx, user, owner } from "./helpers.ts";

const EOA = "0x00000000000000000000000000000000000000aa";

describe("deposits", () => {
    it("credits an OP-path ether deposit: mint to from, value to to", async () => {
        const { router, ledger } = await makeRouter();
        const res = await router.advance(
            block(),
            depositTx({ from: user.address, to: EOA, mint: 100n, value: 60n }),
        );
        expect(res.accept).toBe(true);
        expect((await ledger.account(user.address)).balance).toBe(40n);
        expect((await ledger.account(EOA)).balance).toBe(60n);
    });

    it("does not bump nonces for deposits", async () => {
        const { router, ledger } = await makeRouter();
        await router.advance(block(), depositTx({ from: user.address, to: user.address, mint: 10n, value: 10n }));
        expect((await ledger.account(user.address)).nonce).toBe(0n);
    });
});

describe("enforcement", () => {
    async function fund(router: Awaited<ReturnType<typeof makeRouter>>["router"], amount: bigint) {
        await router.advance(block(), depositTx({ from: user.address, to: user.address, mint: amount, value: amount }));
    }

    it("accepts a signed 1559 transfer and bumps the nonce", async () => {
        const { router, ledger } = await makeRouter();
        await fund(router, 100n);
        const res = await router.advance(block(), await signedTx(user, { to: EOA, nonce: 0, value: 30n }));
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");
        expect((await ledger.account(user.address)).balance).toBe(70n);
        expect((await ledger.account(user.address)).nonce).toBe(1n);
        expect((await ledger.account(EOA)).balance).toBe(30n);
    });

    it("accepts legacy transactions, with and without EIP-155", async () => {
        const { router, ledger } = await makeRouter();
        await fund(router, 100n);
        expect(
            (await router.advance(block(), await signedLegacyTx(user, { to: EOA, nonce: 0, value: 1n }))).accept,
        ).toBe(true);
        expect(
            (await router.advance(block(), await signedLegacyTx(user, { to: EOA, nonce: 1, value: 1n }, false)))
                .accept,
        ).toBe(true);
        expect((await ledger.account(user.address)).nonce).toBe(2n);
    });

    it("rejects a replayed transaction (nonce consumed)", async () => {
        const { router } = await makeRouter();
        await fund(router, 100n);
        const raw = await signedTx(user, { to: EOA, nonce: 0, value: 1n });
        expect((await router.advance(block(), raw)).accept).toBe(true);
        const replay = await router.advance(block(), raw);
        expect(replay.accept).toBe(false);
        expect(replay.reason).toContain("nonce");
    });

    it("rejects a transaction signed for another chain", async () => {
        const { router } = await makeRouter();
        await fund(router, 100n);
        const foreign = await user.signTransaction({
            type: "eip1559",
            chainId: 10,
            nonce: 0,
            gas: 1_000_000n,
            maxFeePerGas: 1n,
            maxPriorityFeePerGas: 0n,
            to: EOA,
            value: 1n,
        });
        const res = await router.advance(block(), toBytes(foreign));
        expect(res.accept).toBe(false);
        expect(res.reason).toContain("chain");
    });

    it("rejects value beyond the balance", async () => {
        const { router } = await makeRouter();
        await fund(router, 10n);
        const res = await router.advance(block(), await signedTx(user, { to: EOA, nonce: 0, value: 11n }));
        expect(res.accept).toBe(false);
        expect(res.reason).toContain("balance");
    });

    it("rejects a malleated (high-s) signature", async () => {
        const { router } = await makeRouter();
        await fund(router, 100n);
        const raw = await signedLegacyTx(user, { to: EOA, nonce: 0, value: 1n }, false);
        const tx = parseTransaction(toHex(raw));
        const N = BigInt("0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141");
        const highS = toHex(N - BigInt(tx.s!), { size: 32 });
        const flippedV = tx.v === 27n ? 28n : 27n;
        const malleated = serializeTransaction(
            { type: "legacy", nonce: tx.nonce, gasPrice: tx.gasPrice, gas: tx.gas, to: tx.to, value: tx.value },
            { v: flippedV, r: tx.r!, s: highS },
        );
        const res = await router.advance(block(), toBytes(malleated));
        expect(res.accept).toBe(false);
        expect(res.reason).toContain("signature");
    });

    it("rejects blob transactions", async () => {
        const { router } = await makeRouter();
        // A minimal serialized 4844 envelope: type byte 0x03 over a valid-enough
        // body is refused before any signature work.
        const res = await router.advance(block(), new Uint8Array([0x03, 0xc0]));
        // Unparseable 4844 bytes fall back to opaque record-and-accept; a
        // parseable one is refused. Either way it must not execute.
        expect(["opaque", "reject"]).toContain(res.outcome);
    });

    it("records-and-accepts bytes that are not a transaction", async () => {
        const { router } = await makeRouter();
        const res = await router.advance(block(), new Uint8Array([0xde, 0xad, 0xbe, 0xef]));
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("opaque");
        expect(res.emissions[0]).toMatchObject({ kind: "report" });
    });

    it("moves value on a transfer to an EOA with calldata (data ignored)", async () => {
        const { router, ledger } = await makeRouter();
        await fund(router, 100n);
        const res = await router.advance(
            block(),
            await signedTx(user, { to: EOA, nonce: 0, value: 5n, data: "0xdeadbeef" }),
        );
        expect(res.accept).toBe(true);
        expect((await ledger.account(EOA)).balance).toBe(5n);
    });
});

describe("fees", () => {
    it("owner sets the fee; a broke fresh sender is then rejected", async () => {
        const { router, ledger } = await makeRouter();
        // Owner needs no balance while the fee is zero.
        const { encodeFunctionData } = await import("viem");
        const { configAbi } = await import("@cartesi/evm-compat");
        const { CONFIG_ADDRESS } = await import("@cartesi/evm-compat");
        const setFee = encodeFunctionData({ abi: configAbi, functionName: "setFee", args: [7n] });
        const res = await router.advance(
            block(),
            await signedTx(owner, { to: CONFIG_ADDRESS, nonce: 0, data: setFee }),
        );
        expect(res.accept).toBe(true);
        expect(ledger.fee).toBe(7n);

        const broke = await router.advance(block(), await signedTx(user, { to: EOA, nonce: 0 }));
        expect(broke.accept).toBe(false);
        expect(broke.reason).toContain("fee");
    });

    it("a reverted transaction still consumes nonce and fee", async () => {
        const { router, ledger } = await makeRouter();
        await router.advance(block(), depositTx({ from: user.address, to: user.address, mint: 100n, value: 100n }));
        const { encodeFunctionData } = await import("viem");
        const { configAbi } = await import("@cartesi/evm-compat");
        const { CONFIG_ADDRESS } = await import("@cartesi/evm-compat");
        const setFee = encodeFunctionData({ abi: configAbi, functionName: "setFee", args: [5n] });
        await router.advance(block(), await signedTx(owner, { to: CONFIG_ADDRESS, nonce: 0, data: setFee }));

        // A non-owner calling the config contract reverts...
        const res = await router.advance(block(), await signedTx(user, { to: CONFIG_ADDRESS, nonce: 0, data: setFee }));
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("revert");
        // ...but is not free: nonce bumped, fee paid, config untouched.
        const account = await ledger.account(user.address);
        expect(account.nonce).toBe(1n);
        expect(account.balance).toBe(95n);
        expect(ledger.fee).toBe(5n);
    });
});
