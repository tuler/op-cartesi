// The routed L2ToL1MessagePasser: what viem's initiateWithdrawal sends, run
// against the guest. The passer needs no registration — a withdrawal is a
// burn, and the L1 side (OptimismPortal) authenticates by storage proof, not
// by anything the guest could configure.

import {
    MESSAGE_PASSER_ADDRESS,
    messagePasserAbi,
    WITHDRAWAL_DEFAULT_GAS_LIMIT,
} from "@op-cartesi/evm";
import { type Address, encodeFunctionData } from "viem";
import { describe, expect, it } from "vitest";
import { block, depositTx, makeRouter, signedTx, user, vouchers, withdrawals } from "./helpers.ts";

const RECIPIENT: Address = "0x00000000000000000000000000000000000a11ce";

async function fundedRouter() {
    const { router, ledger } = await makeRouter();
    await router.advance(
        block(),
        depositTx({ from: user.address, to: user.address, mint: 1000n, value: 1000n }),
    );
    return { router, ledger };
}

describe("L2ToL1MessagePasser", () => {
    it("initiateWithdrawal burns the value and emits the OP withdrawal", async () => {
        const { router, ledger } = await fundedRouter();
        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: MESSAGE_PASSER_ADDRESS,
                nonce: 0,
                value: 200n,
                data: encodeFunctionData({
                    abi: messagePasserAbi,
                    functionName: "initiateWithdrawal",
                    args: [RECIPIENT, 21_000n, "0xdead"],
                }),
            }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");
        const [w] = withdrawals(res.emissions);
        // The caller's exact surface: sender is the EOA, target, gas limit
        // and calldata are theirs verbatim.
        expect(w!.sender.toLowerCase()).toBe(user.address.toLowerCase());
        expect(w!.target.toLowerCase()).toBe(RECIPIENT.toLowerCase());
        expect(w!.value).toBe(200n);
        expect(w!.gasLimit).toBe(21_000n);
        expect(w!.data).toBe("0xdead");
        expect(w!.nonce >> 240n).toBe(1n); // OP message version 1
        expect(vouchers(res.emissions)).toHaveLength(0); // no second executor
        expect((await ledger.account(user.address)).balance).toBe(800n);
        // Burned, not held: the passer keeps nothing.
        expect((await ledger.account(MESSAGE_PASSER_ADDRESS)).balance).toBe(0n);
    });

    it("a plain transfer is receive(): withdraws back to the sender", async () => {
        const { router, ledger } = await fundedRouter();
        const res = await router.advance(
            block(),
            await signedTx(user, { to: MESSAGE_PASSER_ADDRESS, nonce: 0, value: 300n }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");
        const [w] = withdrawals(res.emissions);
        expect(w!.sender.toLowerCase()).toBe(user.address.toLowerCase());
        expect(w!.target.toLowerCase()).toBe(user.address.toLowerCase());
        expect(w!.value).toBe(300n);
        expect(w!.gasLimit).toBe(WITHDRAWAL_DEFAULT_GAS_LIMIT);
        expect((await ledger.account(user.address)).balance).toBe(700n);
        expect((await ledger.account(MESSAGE_PASSER_ADDRESS)).balance).toBe(0n);
    });

    it("unknown calldata reverts and emits nothing", async () => {
        const { router, ledger } = await fundedRouter();
        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: MESSAGE_PASSER_ADDRESS,
                nonce: 0,
                value: 100n,
                data: "0x12345678",
            }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("revert");
        expect(withdrawals(res.emissions)).toHaveLength(0);
        // The value came back with the revert; only the (zero) fee moved.
        expect((await ledger.account(user.address)).balance).toBe(1000n);
    });
});
