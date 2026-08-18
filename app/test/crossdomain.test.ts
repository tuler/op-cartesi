// The standard messaging pair end to end (DESIGN §6): registration and
// escrow exclusivity, inbound finalizeBridgeERC20/ETH relayed from the L1
// messenger, failed-relay recording and replay, and outbound bridgeERC20
// producing the messenger-shaped withdrawal L1CrossDomainMessenger accepts.

import {
    applyL1ToL2Alias,
    BRIDGE_ADDRESS,
    baseGas,
    bridgeAbi,
    CONFIG_ADDRESS,
    configAbi,
    decodeRelayMessage,
    encodeRelayMessage,
    encodeVersionedNonce,
    L2_BRIDGE_ADDRESS,
    L2_MESSENGER_ADDRESS,
    l2StandardBridgeAbi,
    l2TokenAddress,
    PORTAL_ERC20,
} from "@op-cartesi/evm";
import { type Address, decodeFunctionData, encodeFunctionData, type Hex, size } from "viem";
import { describe, expect, it } from "vitest";
import { block, depositTx, makeRouter, owner, signedTx, user, withdrawals } from "./helpers.ts";

const L1_MESSENGER: Address = "0x00000000000000000000000000000000000f00f7";
const L1_BRIDGE: Address = "0x00000000000000000000000000000000000f0010";
const L1_TOKEN: Address = "0x59b670e9fa9d0a427751af201d676719a970857b";
const RECIPIENT: Address = "0x00000000000000000000000000000000000000bb";

async function setup() {
    const rig = await makeRouter();
    const res = await rig.router.advance(
        block(),
        await signedTx(owner, {
            to: CONFIG_ADDRESS,
            nonce: 0,
            data: encodeFunctionData({
                abi: configAbi,
                functionName: "registerMessenger",
                args: [L1_MESSENGER, L1_BRIDGE],
            }),
        }),
    );
    expect(res.outcome).toBe("accept");
    return rig;
}

/** A finalizeBridgeERC20 deposit as L1CrossDomainMessenger would send it. */
function erc20Relay(nonce: bigint, to: Address, amount: bigint): Uint8Array {
    return depositTx({
        from: applyL1ToL2Alias(L1_MESSENGER),
        to: L2_MESSENGER_ADDRESS,
        data: encodeRelayMessage({
            nonce: encodeVersionedNonce(nonce, 1n),
            sender: L1_BRIDGE,
            target: L2_BRIDGE_ADDRESS,
            value: 0n,
            minGasLimit: 200_000n,
            message: encodeFunctionData({
                abi: l2StandardBridgeAbi,
                functionName: "finalizeBridgeERC20",
                args: [l2TokenAddress(L1_TOKEN), L1_TOKEN, user.address, to, amount, "0x"],
            }),
        }),
    });
}

describe("the standard messaging pair", () => {
    it("relays a bridged ERC-20 deposit into a ledger credit", async () => {
        const { router, ledger } = await setup();
        const res = await router.advance(block(), erc20Relay(0n, user.address, 1000n));
        expect(res.outcome).toBe("accept");
        const token = ledger.tokenByL1Address(L1_TOKEN);
        expect(token).not.toBeNull();
        expect(await ledger.tokenBalance(user.address, token!.id)).toBe(1000n);
    });

    it("records a wrong-pair message as failed instead of crediting", async () => {
        const { router, ledger } = await setup();
        // The message names an l2Token that is not the derived façade, so
        // the L1 escrow could never release a withdrawal of it.
        const bad = depositTx({
            from: applyL1ToL2Alias(L1_MESSENGER),
            to: L2_MESSENGER_ADDRESS,
            data: encodeRelayMessage({
                nonce: encodeVersionedNonce(7n, 1n),
                sender: L1_BRIDGE,
                target: L2_BRIDGE_ADDRESS,
                value: 0n,
                minGasLimit: 200_000n,
                message: encodeFunctionData({
                    abi: l2StandardBridgeAbi,
                    functionName: "finalizeBridgeERC20",
                    args: [RECIPIENT, L1_TOKEN, user.address, user.address, 5n, "0x"],
                }),
            }),
        });
        const res = await router.advance(block(), bad);
        // OP semantics: the relay's failure is recorded, not propagated.
        expect(res.outcome).toBe("accept");
        expect(ledger.tokenByL1Address(L1_TOKEN)).toBeNull();
    });

    it("refuses an unauthenticated relay and a double relay", async () => {
        const { router, ledger } = await setup();
        // Not from the registered messenger: falls into the replay path,
        // and there is no recorded failure to replay.
        const forged = depositTx({
            from: applyL1ToL2Alias(RECIPIENT),
            to: L2_MESSENGER_ADDRESS,
            data: encodeRelayMessage({
                nonce: encodeVersionedNonce(0n, 1n),
                sender: L1_BRIDGE,
                target: L2_BRIDGE_ADDRESS,
                value: 0n,
                minGasLimit: 200_000n,
                message: "0x",
            }),
        });
        expect((await router.advance(block(), forged)).outcome).toBe("revert");

        // A relayed message cannot be relayed again.
        expect((await router.advance(block(), erc20Relay(1n, user.address, 10n))).outcome).toBe(
            "accept",
        );
        expect((await router.advance(block(), erc20Relay(1n, user.address, 10n))).outcome).toBe(
            "revert",
        );
        const token = ledger.tokenByL1Address(L1_TOKEN);
        expect(await ledger.tokenBalance(user.address, token!.id)).toBe(10n);
    });

    it("relays bridged ether to the recipient", async () => {
        const { router, ledger } = await setup();
        const deposit = depositTx({
            from: applyL1ToL2Alias(L1_MESSENGER),
            to: L2_MESSENGER_ADDRESS,
            mint: 500n,
            value: 500n,
            data: encodeRelayMessage({
                nonce: encodeVersionedNonce(2n, 1n),
                sender: L1_BRIDGE,
                target: L2_BRIDGE_ADDRESS,
                value: 500n,
                minGasLimit: 200_000n,
                message: encodeFunctionData({
                    abi: l2StandardBridgeAbi,
                    functionName: "finalizeBridgeETH",
                    args: [user.address, RECIPIENT, 500n, "0x"],
                }),
            }),
        });
        expect((await router.advance(block(), deposit)).outcome).toBe("accept");
        expect((await ledger.account(RECIPIENT)).balance).toBe(500n);
        expect((await ledger.account(L2_MESSENGER_ADDRESS)).balance).toBe(0n);
    });

    it("bridgeERC20 emits the withdrawal L1CrossDomainMessenger accepts", async () => {
        const { router, ledger } = await setup();
        await router.advance(block(), erc20Relay(0n, user.address, 1000n));

        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: L2_BRIDGE_ADDRESS,
                nonce: 0,
                data: encodeFunctionData({
                    abi: l2StandardBridgeAbi,
                    functionName: "bridgeERC20To",
                    args: [l2TokenAddress(L1_TOKEN), L1_TOKEN, RECIPIENT, 300n, 200000, "0x"],
                }),
            }),
        );
        expect(res.outcome).toBe("accept");
        const token = ledger.tokenByL1Address(L1_TOKEN);
        expect(await ledger.tokenBalance(user.address, token!.id)).toBe(700n);

        const [w] = withdrawals(res.emissions);
        expect(w).toBeDefined();
        // The withdrawal's sender is the L2 messenger — what the portal
        // exposes as l2Sender(), and what L1CrossDomainMessenger requires.
        expect(w!.sender.toLowerCase()).toBe(L2_MESSENGER_ADDRESS.toLowerCase());
        expect(w!.target.toLowerCase()).toBe(L1_MESSENGER.toLowerCase());
        expect(w!.value).toBe(0n);

        const relay = decodeRelayMessage(w!.data);
        expect(relay).toBeDefined();
        // First outbound message: messenger nonce 0, version 1.
        expect(relay!.nonce).toBe(encodeVersionedNonce(0n, 1n));
        expect(relay!.sender.toLowerCase()).toBe(L2_BRIDGE_ADDRESS.toLowerCase());
        expect(relay!.target.toLowerCase()).toBe(L1_BRIDGE.toLowerCase());
        expect(w!.gasLimit).toBe(baseGas(size(relay!.message), 200_000n));

        // The inner message is the L1 bridge's finalizeBridgeERC20, with the
        // pair in L1 perspective — the exact call that releases the escrow.
        const inner = decodeFunctionData({ abi: l2StandardBridgeAbi, data: relay!.message });
        expect(inner.functionName).toBe("finalizeBridgeERC20");
        const [localToken, remoteToken, from, to, amount] = inner.args as readonly [
            Address,
            Address,
            Address,
            Address,
            bigint,
            Hex,
        ];
        expect(localToken.toLowerCase()).toBe(L1_TOKEN.toLowerCase());
        expect(remoteToken.toLowerCase()).toBe(l2TokenAddress(L1_TOKEN).toLowerCase());
        expect(from.toLowerCase()).toBe(user.address.toLowerCase());
        expect(to.toLowerCase()).toBe(RECIPIENT.toLowerCase());
        expect(amount).toBe(300n);
    });

    it("messenger nonces are dense and survive a revert unspent", async () => {
        const { router, ledger } = await setup();
        await router.advance(block(), erc20Relay(0n, user.address, 1000n));
        const bridgeTx = (nonce: number, amount: bigint) =>
            signedTx(user, {
                to: L2_BRIDGE_ADDRESS,
                nonce,
                data: encodeFunctionData({
                    abi: l2StandardBridgeAbi,
                    functionName: "bridgeERC20",
                    args: [l2TokenAddress(L1_TOKEN), L1_TOKEN, amount, 100000, "0x"],
                }),
            });
        expect((await router.advance(block(), await bridgeTx(0, 100n))).outcome).toBe("accept");
        // A reverting bridge call must not burn a messenger nonce.
        expect((await router.advance(block(), await bridgeTx(1, 10_000n))).outcome).toBe("revert");
        const res = await router.advance(block(), await bridgeTx(2, 100n));
        expect(res.outcome).toBe("accept");
        const relay = decodeRelayMessage(withdrawals(res.emissions)[0]!.data);
        expect(relay!.nonce).toBe(encodeVersionedNonce(1n, 1n));
        expect(ledger.peekMessageNonce()).toBe(2n);
    });

    it("keeps the escrow paths exclusive, both ways", async () => {
        // Messenger first, then an ERC-20 portal: refused.
        const a = await setup();
        const registerPortal = await signedTx(owner, {
            to: CONFIG_ADDRESS,
            nonce: 1,
            data: encodeFunctionData({
                abi: configAbi,
                functionName: "registerPortal",
                args: [PORTAL_ERC20, RECIPIENT],
            }),
        });
        expect((await a.router.advance(block(), registerPortal)).outcome).toBe("revert");

        // Portal first, then the messenger: refused.
        const b = await makeRouter();
        const portalFirst = await signedTx(owner, {
            to: CONFIG_ADDRESS,
            nonce: 0,
            data: encodeFunctionData({
                abi: configAbi,
                functionName: "registerPortal",
                args: [PORTAL_ERC20, RECIPIENT],
            }),
        });
        expect((await b.router.advance(block(), portalFirst)).outcome).toBe("accept");
        const messengerSecond = await signedTx(owner, {
            to: CONFIG_ADDRESS,
            nonce: 1,
            data: encodeFunctionData({
                abi: configAbi,
                functionName: "registerMessenger",
                args: [L1_MESSENGER, L1_BRIDGE],
            }),
        });
        expect((await b.router.advance(block(), messengerSecond)).outcome).toBe("revert");
    });

    it("withdrawERC20 on the 0xC751 bridge defers to the standard bridge", async () => {
        const { router } = await setup();
        await router.advance(block(), erc20Relay(0n, user.address, 1000n));
        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: BRIDGE_ADDRESS,
                nonce: 0,
                data: encodeFunctionData({
                    abi: bridgeAbi,
                    functionName: "withdrawERC20",
                    args: [l2TokenAddress(L1_TOKEN), RECIPIENT, 1n],
                }),
            }),
        );
        expect(res.outcome).toBe("revert");
    });
});
