// The per-block tick: the hook that runs whether or not the block carries
// transactions.
//
// The chain deposits op-node's L1-attributes transaction at the head of every
// block (EVM-COMPAT §2), so that input is the application's heartbeat. These
// tests pin what rides on it: that it fires exactly once per block and only
// off the chain's own deposit, that a tick's failure cannot reach the input,
// the block's L1 attributes, or another contract's tick, and that outputs and
// withdrawal nonces come out of it whole.

import {
    encodeEvmCall,
    L1_INFO_DEPOSITOR,
    L1BLOCK_ADDRESS,
    l1BlockAbi,
    TAG_FAIL,
    TAG_REVERT,
} from "@op-cartesi/evm";
import {
    type Address,
    concat,
    encodeFunctionData,
    type Hex,
    parseAbi,
    toBytes,
    toFunctionSelector,
    toHex,
} from "viem";
import { describe, expect, it } from "vitest";
import type { BlockEnv } from "../src/contract.ts";
import { contractHandler, Fail, Revert } from "../src/contract.ts";
import type { Router } from "../src/router.ts";
import { block, CHAIN_ID, depositTx, makeRouter, signedTx, user, withdrawals } from "./helpers.ts";

const TICKER: Address = "0xc0de000000000000000000000000000000000009";
const OTHER: Address = "0xc0de00000000000000000000000000000000000a";

const tickerAbi = parseAbi(["function ticks() view returns (uint256)"]);

/** The Ecotone packed attributes op-node deposits every block. */
function attributesCalldata(l1Number = 123456n): Hex {
    const body = new Uint8Array(160);
    const view = new DataView(body.buffer);
    view.setUint32(0, 1368); // baseFeeScalar
    view.setUint32(4, 810949); // blobBaseFeeScalar
    view.setBigUint64(8, 4n); // sequenceNumber
    view.setBigUint64(16, 1720009999n); // timestamp
    view.setBigUint64(24, l1Number); // number
    body[63] = 7; // basefee
    body[95] = 1; // blobBaseFee
    body.fill(0xaa, 96, 128);
    body.fill(0xbb, 128, 160);
    return concat([toFunctionSelector("setL1BlockValuesEcotone()"), toHex(body)]);
}

/** The chain's own attributes deposit, as input bytes. */
function attributesDeposit(l1Number?: bigint): Uint8Array {
    return depositTx({
        from: L1_INFO_DEPOSITOR,
        to: L1BLOCK_ADDRESS,
        data: attributesCalldata(l1Number),
    });
}

/** Registers a contract whose only content is a tick. */
function registerTick(
    router: Router,
    address: Address,
    onBlock: (env: BlockEnv) => Promise<void> | void,
): void {
    router.register(address, contractHandler({ address, abi: tickerAbi, onBlock }));
}

describe("the per-block tick", () => {
    it("runs on a block with no transactions in it", async () => {
        const { router } = await makeRouter();
        const seen: bigint[] = [];
        registerTick(router, TICKER, ({ block: b }) => {
            seen.push(b.blockNumber);
        });

        const first = block();
        const second = block();
        await router.advance(first, attributesDeposit());
        await router.advance(second, attributesDeposit());

        expect(seen).toEqual([first.blockNumber, second.blockNumber]);
    });

    it("receives the L1 attributes the same input just delivered", async () => {
        const { router } = await makeRouter();
        let l1Number = 0n;
        let l1Timestamp = 0n;
        registerTick(router, TICKER, ({ l1 }) => {
            l1Number = l1.number;
            l1Timestamp = l1.timestamp;
        });

        await router.advance(block(), attributesDeposit(999n));

        expect(l1Number).toBe(999n);
        expect(l1Timestamp).toBe(1720009999n);
    });

    it("does not fire on an ordinary transaction", async () => {
        const { router, ledger } = await makeRouter();
        let ticks = 0;
        registerTick(router, TICKER, () => {
            ticks += 1;
        });
        await ledger.creditEther(user.address, 10n ** 18n);

        await router.advance(
            block(),
            await signedTx(user, { to: "0x00000000000000000000000000000000000000aa", nonce: 0 }),
        );

        expect(ticks).toBe(0);
    });

    it("does not fire on an ordinary deposit", async () => {
        const { router } = await makeRouter();
        let ticks = 0;
        registerTick(router, TICKER, () => {
            ticks += 1;
        });

        await router.advance(
            block(),
            depositTx({ from: user.address, to: user.address, mint: 100n, value: 100n }),
        );

        expect(ticks).toBe(0);
    });

    it("cannot be minted by a user transaction to L1Block", async () => {
        // The sender is the authority, not the address: attributes-shaped
        // calldata from a funded account buys no extra tick.
        const { router, ledger } = await makeRouter();
        let ticks = 0;
        registerTick(router, TICKER, () => {
            ticks += 1;
        });
        await ledger.creditEther(user.address, 10n ** 18n);

        const res = await router.advance(
            block(),
            await signedTx(user, {
                to: L1BLOCK_ADDRESS,
                nonce: 0,
                data: attributesCalldata(4242n),
            }),
        );

        expect(res.accept).toBe(true);
        expect(ticks).toBe(0);
    });

    it("cannot be minted by a deposit from anyone but the canonical depositor", async () => {
        const { router } = await makeRouter();
        let ticks = 0;
        registerTick(router, TICKER, () => {
            ticks += 1;
        });

        await router.advance(
            block(),
            depositTx({ from: user.address, to: L1BLOCK_ADDRESS, data: attributesCalldata() }),
        );

        expect(ticks).toBe(0);
    });

    it("does not fire when the attributes input is malformed", async () => {
        const { router } = await makeRouter();
        let ticks = 0;
        registerTick(router, TICKER, () => {
            ticks += 1;
        });

        const res = await router.advance(
            block(),
            depositTx({ from: L1_INFO_DEPOSITOR, to: L1BLOCK_ADDRESS, data: "0xdeadbeef" }),
        );

        expect(res.outcome).toBe("revert");
        expect(ticks).toBe(0);
    });

    it("commits ledger writes and emits its outputs on the input", async () => {
        const { router, ledger } = await makeRouter();
        registerTick(router, TICKER, async ({ ledger: l, out }) => {
            await l.creditEther(TICKER, 5n);
            out.notice("0xc0ffee");
        });

        const res = await router.advance(block(), attributesDeposit());

        expect(res.accept).toBe(true);
        expect((await ledger.account(TICKER)).balance).toBe(5n);
        expect(res.emissions).toContainEqual({ kind: "notice", payload: "0xc0ffee" });
    });
});

describe("a tick that fails", () => {
    it("reverts its own ledger writes and drops its outputs, keeping the input", async () => {
        const { router, ledger } = await makeRouter();
        registerTick(router, TICKER, async ({ ledger: l, out }) => {
            await l.creditEther(TICKER, 5n);
            out.notice("0xc0ffee");
            out.report("0xd1a6");
            throw new Revert("not today");
        });

        const res = await router.advance(block(), attributesDeposit());

        expect(res.accept).toBe(true);
        expect(res.outcome).toBe("accept");
        expect((await ledger.account(TICKER)).balance).toBe(0n);
        expect(res.emissions.some((e) => e.kind === "notice")).toBe(false);
        // Diagnostic reports survive a revert, and the revert data follows.
        const reports = res.emissions.filter((e) => e.kind === "report");
        expect(reports.at(-1)?.payload.slice(0, 4)).toBe(toHex(new Uint8Array([TAG_REVERT])));
    });

    it("leaves the block's L1 attributes stored", async () => {
        // The tick rides the input that carries the chain's own L1 context;
        // an application bug must not be able to shadow it.
        const { router } = await makeRouter();
        registerTick(router, TICKER, () => {
            throw new Error("boom");
        });

        await router.advance(block(), attributesDeposit(777n));

        const res = await router.inspect(
            toBytes(
                encodeEvmCall({
                    chainId: CHAIN_ID,
                    from: user.address,
                    to: L1BLOCK_ADDRESS,
                    value: 0n,
                    data: toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "number" })),
                    simulate: false,
                }),
            ),
        );
        expect(res.accept).toBe(true);
        expect(BigInt(`0x${res.reports[0]!.slice(4)}`)).toBe(777n);
    });

    it("keeps its writes and outputs when it throws Fail", async () => {
        const { router, ledger } = await makeRouter();
        registerTick(router, TICKER, async ({ ledger: l, out }) => {
            await l.creditEther(TICKER, 5n);
            out.notice("0xc0ffee");
            throw new Fail("already moved");
        });

        const res = await router.advance(block(), attributesDeposit());

        expect(res.accept).toBe(true);
        expect((await ledger.account(TICKER)).balance).toBe(5n);
        expect(res.emissions).toContainEqual({ kind: "notice", payload: "0xc0ffee" });
        const reports = res.emissions.filter((e) => e.kind === "report");
        expect(reports.at(-1)?.payload.slice(0, 4)).toBe(toHex(new Uint8Array([TAG_FAIL])));
    });

    it("does not disturb another contract's tick", async () => {
        const { router, ledger } = await makeRouter();
        registerTick(router, TICKER, () => {
            throw new Error("boom");
        });
        registerTick(router, OTHER, async ({ ledger: l }) => {
            await l.creditEther(OTHER, 9n);
        });

        await router.advance(block(), attributesDeposit());

        expect((await ledger.account(OTHER)).balance).toBe(9n);
    });

    it("keeps an earlier tick's writes when a later one reverts", async () => {
        const { router, ledger } = await makeRouter();
        registerTick(router, OTHER, async ({ ledger: l }) => {
            await l.creditEther(OTHER, 9n);
        });
        registerTick(router, TICKER, async ({ ledger: l }) => {
            await l.creditEther(TICKER, 5n);
            throw new Revert("no");
        });

        await router.advance(block(), attributesDeposit());

        expect((await ledger.account(OTHER)).balance).toBe(9n);
        expect((await ledger.account(TICKER)).balance).toBe(0n);
    });
});

describe("tick ordering and outputs", () => {
    it("runs ticks in registration order", async () => {
        const { router } = await makeRouter();
        const order: string[] = [];
        registerTick(router, TICKER, () => {
            order.push("first");
        });
        registerTick(router, OTHER, () => {
            order.push("second");
        });

        await router.advance(block(), attributesDeposit());

        expect(order).toEqual(["first", "second"]);
    });

    it("gives every withdrawal of the block a distinct nonce", async () => {
        const { router } = await makeRouter();
        registerTick(router, TICKER, ({ out }) => {
            out.withdrawal({ sender: TICKER, target: user.address, value: 1n });
        });
        registerTick(router, OTHER, ({ out }) => {
            out.withdrawal({ sender: OTHER, target: user.address, value: 2n });
        });

        const res = await router.advance(block(), attributesDeposit());

        const nonces = withdrawals(res.emissions).map((w) => w.nonce);
        expect(nonces).toHaveLength(2);
        expect(new Set(nonces).size).toBe(2);
    });
});

describe("the tick's context", () => {
    it("hands out copies, so a tick cannot rewrite the chain's L1 context", async () => {
        const { router } = await makeRouter();
        registerTick(router, TICKER, ({ block: b, l1 }) => {
            l1.number = 1n;
            b.timestamp = 1n;
        });

        // What Application.run does: the parked machine's last-seen context
        // is the very object handed to advance, so a tick that could write
        // through it would be rewriting what eth_call answers from.
        const b = block();
        const timestamp = b.timestamp;
        router.lastBlock = b;
        await router.advance(b, attributesDeposit(555n));

        const res = await router.inspect(
            toBytes(
                encodeEvmCall({
                    chainId: CHAIN_ID,
                    from: user.address,
                    to: L1BLOCK_ADDRESS,
                    value: 0n,
                    data: toBytes(encodeFunctionData({ abi: l1BlockAbi, functionName: "number" })),
                    simulate: false,
                }),
            ),
        );
        expect(BigInt(`0x${res.reports[0]!.slice(4)}`)).toBe(555n);
        expect(router.lastBlock.timestamp).toBe(timestamp);
    });
});
