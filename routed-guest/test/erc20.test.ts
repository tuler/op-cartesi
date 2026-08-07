// The token path end to end: portal registration, ERC-20 deposit with
// first-seen registration, the façade over EvmCall, transfers with Transfer
// events, the byte-identical revert journal, and withdrawals as vouchers.

import { describe, expect, it } from "vitest";
import {
    decodeAbiParameters,
    decodeFunctionResult,
    encodeFunctionData,
    encodePacked,
    toBytes,
    type Address,
    type Hex,
} from "viem";
import { applyL1ToL2Alias, BRIDGE_ADDRESS, bridgeAbi, CONFIG_ADDRESS, configAbi, erc20FacadeAbi as erc20Abi, l2TokenAddress, PORTAL_ERC20, PORTAL_ETHER, REGISTRY_ADDRESS, registryAbi } from "@cartesi/evm-compat";
import { EVM_LOG_SELECTOR, TRANSFER_TOPIC } from "@cartesi/evm-compat";
import { encodeEvmCall } from "@cartesi/evm-compat";
import {
    APP_CONTRACT,
    block,
    CHAIN_ID,
    depositTx,
    driveBytes,
    makeRouter,
    notices,
    owner,
    signedTx,
    user,
    vouchers,
} from "./helpers.ts";

const L1_TOKEN: Address = "0x59b670e9fa9d0a427751af201d676719a970857b";
const ERC20_PORTAL: Address = "0x36c02da8a0983159322a80ffe9f24b1acff8b570";
const RECIPIENT: Address = "0x00000000000000000000000000000000000000bb";
const ZERO32 = "0x0000000000000000000000000000000000000000000000000000000000000000";

function decodeEvmLog(payload: Hex) {
    expect(payload.slice(0, 10)).toBe(EVM_LOG_SELECTOR);
    const [emitter, topics, data] = decodeAbiParameters(
        [{ type: "address" }, { type: "bytes32[]" }, { type: "bytes" }],
        `0x${payload.slice(10)}`,
    );
    return { emitter, topics, data };
}

async function setup() {
    const rig = await makeRouter();
    // The owner registers the ERC-20 portal, as a signed L2 transaction —
    // the L1-deposit path of the Lua guest still works, this one is just
    // cheaper to exercise.
    const register = encodeFunctionData({
        abi: configAbi,
        functionName: "registerPortal",
        args: [PORTAL_ERC20, ERC20_PORTAL],
    });
    const res = await rig.router.advance(
        block(),
        await signedTx(owner, { to: CONFIG_ADDRESS, nonce: 0, data: register }),
    );
    expect(res.accept).toBe(true);
    return rig;
}

/** InputEncoding's packed ERC-20 deposit, as OPERC20Portal sends it. */
function erc20Deposit(amount: bigint): Uint8Array {
    return depositTx({
        from: applyL1ToL2Alias(ERC20_PORTAL),
        to: APP_CONTRACT,
        data: encodePacked(["address", "address", "uint256"], [L1_TOKEN, user.address, amount]),
    });
}

async function callView(router: Awaited<ReturnType<typeof makeRouter>>["router"], to: Address, data: Hex) {
    const res = await router.inspect(
        toBytes(encodeEvmCall({ chainId: CHAIN_ID, from: user.address, to, value: 0n, data: toBytes(data) })),
    );
    return res;
}

describe("the token path", () => {
    it("registers first-seen on deposit, credits, and emits a mint Transfer", async () => {
        const { router, ledger } = await setup();
        const res = await router.advance(block(), erc20Deposit(1000n));
        expect(res.accept).toBe(true);

        const token = ledger.tokenByL1Address(L1_TOKEN);
        expect(token).not.toBeNull();
        expect(await ledger.tokenBalance(user.address, token!.id)).toBe(1000n);

        const [transferNotice] = notices(res.emissions);
        const log = decodeEvmLog(transferNotice!.payload);
        expect(log.emitter.toLowerCase()).toBe(l2TokenAddress(L1_TOKEN).toLowerCase());
        expect(log.topics[0]).toBe(TRANSFER_TOPIC);
        expect(log.topics[1]).toBe(ZERO32); // mint: from the zero address
    });

    it("serves balanceOf and l2TokenOf through EvmCall", async () => {
        const { router } = await setup();
        await router.advance(block(), erc20Deposit(1000n));
        const facade = l2TokenAddress(L1_TOKEN);

        const bal = await callView(
            router,
            facade,
            encodeFunctionData({ abi: erc20Abi, functionName: "balanceOf", args: [user.address] }),
        );
        expect(bal.accept).toBe(true);
        // Reports carry the one-byte framing tag; 0x01 is return data.
        expect(bal.reports[0]!.slice(0, 4)).toBe("0x01");
        const returned: Hex = `0x${bal.reports[0]!.slice(4)}`;
        expect(decodeFunctionResult({ abi: erc20Abi, functionName: "balanceOf", data: returned })).toBe(1000n);

        const derived = await callView(
            router,
            REGISTRY_ADDRESS,
            encodeFunctionData({ abi: registryAbi, functionName: "l2TokenOf", args: [L1_TOKEN] }),
        );
        const derivedAddr = decodeFunctionResult({
            abi: registryAbi,
            functionName: "l2TokenOf",
            data: `0x${derived.reports[0]!.slice(4)}`,
        });
        expect(derivedAddr.toLowerCase()).toBe(facade.toLowerCase());
    });

    it("transfer() moves balances and emits Transfer", async () => {
        const { router, ledger } = await setup();
        await router.advance(block(), erc20Deposit(1000n));
        const facade = l2TokenAddress(L1_TOKEN);

        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: facade,
                nonce: 0,
                data: encodeFunctionData({ abi: erc20Abi, functionName: "transfer", args: [RECIPIENT, 300n] }),
            }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");
        const id = ledger.tokenByL1Address(L1_TOKEN)!.id;
        expect(await ledger.tokenBalance(user.address, id)).toBe(700n);
        expect(await ledger.tokenBalance(RECIPIENT, id)).toBe(300n);
        expect(ledger.totalSupply(id)).toBe(1000n);
        const log = decodeEvmLog(notices(res.emissions)[0]!.payload);
        expect(log.topics[0]).toBe(TRANSFER_TOPIC);
    });

    it("a failed transfer reverts byte-identically except nonce, and emits no outputs", async () => {
        const { router, ledger } = await setup();
        await router.advance(block(), erc20Deposit(100n));
        const facade = l2TokenAddress(L1_TOKEN);
        // Materialize the sender's account record first: otherwise the
        // nonce bump's insert (address + liveCount) dominates the diff.
        await router.advance(block(), depositTx({ from: user.address, to: user.address, mint: 1n, value: 1n }));

        const before = driveBytes(ledger);
        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: facade,
                nonce: 0,
                data: encodeFunctionData({ abi: erc20Abi, functionName: "transfer", args: [RECIPIENT, 101n] }),
            }),
        );
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("revert");
        expect(notices(res.emissions)).toHaveLength(0);
        expect(vouchers(res.emissions)).toHaveLength(0);
        // The revert report carries tag 0x02 and Error("ERC20: transfer
        // amount exceeds balance").
        const revertReport = res.emissions.find((e) => e.kind === "report")!;
        expect(revertReport.payload.slice(0, 4)).toBe("0x02");

        // Only the sender's account record (its nonce) may differ.
        const after = driveBytes(ledger);
        expect((await ledger.account(user.address)).nonce).toBe(1n);
        let diffs = 0;
        for (let i = 0; i < before.length; i++) if (before[i] !== after[i]) diffs++;
        expect(diffs).toBeGreaterThan(0); // the nonce byte
        expect(diffs).toBeLessThanOrEqual(8); // and nothing else
    });

    it("withdrawERC20 burns the sender's balance and emits the L1 transfer voucher", async () => {
        const { router, ledger } = await setup();
        await router.advance(block(), erc20Deposit(1000n));
        const facade = l2TokenAddress(L1_TOKEN);

        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: BRIDGE_ADDRESS,
                nonce: 0,
                data: encodeFunctionData({
                    abi: bridgeAbi,
                    functionName: "withdrawERC20",
                    args: [facade, RECIPIENT, 400n],
                }),
            }),
        );
        expect(res.accept).toBe(true);
        const [voucher] = vouchers(res.emissions);
        expect(voucher!.destination.toLowerCase()).toBe(L1_TOKEN.toLowerCase());
        expect(voucher!.value).toBe(0n);
        // transfer(RECIPIENT, 400)
        expect(voucher!.payload.startsWith("0xa9059cbb")).toBe(true);
        const id = ledger.tokenByL1Address(L1_TOKEN)!.id;
        expect(await ledger.tokenBalance(user.address, id)).toBe(600n);
        expect(ledger.totalSupply(id)).toBe(600n);
    });

    it("withdrawEther burns the value and pays the voucher", async () => {
        const { router, ledger } = await setup();
        // Fund through an ether-portal deposit so the credit is voucher-backed.
        const ETHER_PORTAL: Address = "0x0165878a594ca255338adfa4d48449f69242eb8f";
        const register = encodeFunctionData({
            abi: configAbi,
            functionName: "registerPortal",
            args: [PORTAL_ETHER, ETHER_PORTAL],
        });
        await router.advance(block(), await signedTx(owner, { to: CONFIG_ADDRESS, nonce: 1, data: register }));
        const deposit = depositTx({
            from: applyL1ToL2Alias(ETHER_PORTAL),
            to: APP_CONTRACT,
            mint: 500n,
            value: 500n,
            data: encodePacked(["address", "uint256"], [user.address, 500n]),
        });
        expect((await router.advance(block(), deposit)).accept).toBe(true);
        expect((await ledger.account(user.address)).balance).toBe(500n);

        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: BRIDGE_ADDRESS,
                nonce: 0,
                value: 200n,
                data: encodeFunctionData({ abi: bridgeAbi, functionName: "withdrawEther", args: [RECIPIENT] }),
            }),
        );
        expect(res.accept).toBe(true);
        const [voucher] = vouchers(res.emissions);
        expect(voucher!.destination.toLowerCase()).toBe(RECIPIENT.toLowerCase());
        expect(voucher!.value).toBe(200n);
        expect((await ledger.account(user.address)).balance).toBe(300n);
        expect((await ledger.account(BRIDGE_ADDRESS)).balance).toBe(0n); // burned, not held
    });

    it("routes a portal deposit by its registered sender, whatever the to", async () => {
        // The devnet cannot know the application contract address at
        // genesis (the portals and the app contract are deployed after the
        // chain starts), so a deposit from a registered portal routes to
        // the receiver even when `to` is not the envelope's appContract.
        const { router, ledger } = await setup();
        const res = await router.advance(
            block(),
            depositTx({
                from: applyL1ToL2Alias(ERC20_PORTAL),
                to: RECIPIENT, // deliberately not APP_CONTRACT
                data: encodePacked(["address", "address", "uint256"], [L1_TOKEN, user.address, 500n]),
            }),
        );
        expect(res.accept).toBe(true);
        const token = ledger.tokenByL1Address(L1_TOKEN);
        expect(token).not.toBeNull();
        expect(await ledger.tokenBalance(user.address, token!.id)).toBe(500n);
    });

    it("refuses a deposit from an unregistered portal", async () => {
        const { router, ledger } = await makeRouter(); // no portal registered
        const res = await router.advance(block(), erc20Deposit(1000n));
        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("revert"); // mint (zero here) survives; credit does not
        expect(ledger.tokenByL1Address(L1_TOKEN)).toBeNull();
    });
});
