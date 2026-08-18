// The L1 half of every OP-path withdrawal, whatever the message says: wait
// for a proposal covering the withdrawal's block, prove the sentMessages
// slot against it with eth_getProof — the exact storage proof viem's
// buildProveWithdrawal would fetch — then outlive the delays and finalize.
//
// The delay and game plumbing is devnet-specific: anvil time is advanced
// past the proof maturity delay and the permissioned game's clock, and the
// game is resolved by hand because nothing else on the devnet will.

import {
    MESSAGE_PASSER_ADDRESS,
    type WithdrawalMessage,
    withdrawalHash,
    withdrawalSlot,
} from "@op-cartesi/evm";
import { config } from "devnet/env";
import { l1Public, l1Wallet, l2Public } from "devnet/wallet";
import { type Address, type Hex, parseAbi } from "viem";

const factoryAbi = parseAbi([
    "function gameCount() view returns (uint256)",
    "function gameAtIndex(uint256 index) view returns (uint32, uint64, address)",
]);

const gameAbi = parseAbi([
    "function l2BlockNumber() view returns (uint256)",
    "function status() view returns (uint8)",
    "function resolve() returns (uint8)",
    "function resolveClaim(uint256 claimIndex, uint256 numToResolve)",
    "function maxClockDuration() view returns (uint64)",
]);

const portalAbi = parseAbi([
    "function proveWithdrawalTransaction((uint256 nonce, address sender, address target, uint256 value, uint256 gasLimit, bytes data) tx, uint256 disputeGameIndex, (bytes32 version, bytes32 stateRoot, bytes32 messagePasserStorageRoot, bytes32 latestBlockhash) outputRootProof, bytes[] withdrawalProof)",
    "function finalizeWithdrawalTransaction((uint256 nonce, address sender, address target, uint256 value, uint256 gasLimit, bytes data) tx)",
    "function proofMaturityDelaySeconds() view returns (uint256)",
]);

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Proves and finalizes one withdrawal message through OptimismPortal.
 * `emittedIn` is the L2 block that carried it. */
export async function proveAndFinalizeWithdrawal(
    message: WithdrawalMessage,
    emittedIn: bigint,
): Promise<void> {
    const factory = config.disputeGameFactory;
    if (!factory) throw new Error("run the devnet with contracts deployed first");
    const l1 = l1Public();
    const l2 = l2Public();
    const caller = l1Wallet(config.callerKey);
    const portal = config.depositContract;
    const wHash = withdrawalHash(message);
    console.error(`  withdrawal ${wHash}, in L2 block ${emittedIn}`);

    // 1. Wait for a proposal whose L2 block is at or past the withdrawal's.
    console.error(`  waiting for a proposal covering L2 block ${emittedIn}`);
    let gameIndex = -1n;
    let game: Address | undefined;
    let proposedBlock = 0n;
    for (;;) {
        const count = await l1.readContract({
            address: factory,
            abi: factoryAbi,
            functionName: "gameCount",
        });
        for (let i = count - 1n; i >= 0n; i--) {
            const [, , proxy] = await l1.readContract({
                address: factory,
                abi: factoryAbi,
                functionName: "gameAtIndex",
                args: [i],
            });
            const block = await l1.readContract({
                address: proxy,
                abi: gameAbi,
                functionName: "l2BlockNumber",
            });
            if (block >= emittedIn) {
                gameIndex = i;
                game = proxy;
                proposedBlock = block;
                break;
            }
        }
        if (game) break;
        await sleep(4_000);
    }
    console.error(`  game ${gameIndex} proposes L2 block ${proposedBlock}`);

    // 2. The storage proof against the proposed block.
    const slot = withdrawalSlot(wHash);
    const proof = await l2.getProof({
        address: MESSAGE_PASSER_ADDRESS,
        storageKeys: [slot],
        blockNumber: proposedBlock,
    });
    const block = await l2.getBlock({ blockNumber: proposedBlock });
    if (!block.withdrawalsRoot) throw new Error(`L2 block ${proposedBlock} has no withdrawalsRoot`);
    if (proof.storageHash !== block.withdrawalsRoot) {
        throw new Error(
            `storageHash ${proof.storageHash} != withdrawalsRoot ${block.withdrawalsRoot}`,
        );
    }

    // 3. Prove against the portal.
    const zero: Hex = "0x0000000000000000000000000000000000000000000000000000000000000000";
    await waitL1(
        l1,
        caller.writeContract({
            address: portal,
            abi: portalAbi,
            functionName: "proveWithdrawalTransaction",
            args: [
                message,
                gameIndex,
                {
                    version: zero,
                    stateRoot: block.stateRoot,
                    messagePasserStorageRoot: block.withdrawalsRoot,
                    latestBlockhash: block.hash,
                },
                proof.storageProof[0]!.proof,
            ],
        }),
    );
    console.error(`  proven against OptimismPortal at ${portal}`);

    // 4. Outlive the delays and resolve the game — devnet plumbing.
    const maturity = await l1.readContract({
        address: portal,
        abi: portalAbi,
        functionName: "proofMaturityDelaySeconds",
    });
    const clock = await l1.readContract({
        address: game!,
        abi: gameAbi,
        functionName: "maxClockDuration",
    });
    const skip = (maturity > clock ? maturity : clock) + 60n;
    console.error(
        `  advancing anvil time by ${skip}s (proof maturity ${maturity}s, game clock ${clock}s)`,
    );
    await l1.request({ method: "evm_increaseTime" as never, params: [Number(skip)] as never });
    await l1.request({ method: "evm_mine" as never, params: [] as never });

    const status = await l1.readContract({ address: game!, abi: gameAbi, functionName: "status" });
    if (status === 0) {
        console.error("  resolving the dispute game");
        await waitL1(
            l1,
            caller.writeContract({
                address: game!,
                abi: gameAbi,
                functionName: "resolveClaim",
                args: [0n, 0n],
            }),
        );
        await waitL1(
            l1,
            caller.writeContract({ address: game!, abi: gameAbi, functionName: "resolve" }),
        );
    }

    // 5. Finalize: the portal executes the withdrawal's call.
    await waitL1(
        l1,
        caller.writeContract({
            address: portal,
            abi: portalAbi,
            functionName: "finalizeWithdrawalTransaction",
            args: [message],
        }),
    );
    console.error("  finalized through OptimismPortal");
}

async function waitL1(l1: ReturnType<typeof l1Public>, tx: Promise<Hex>): Promise<void> {
    const receipt = await l1.waitForTransactionReceipt({ hash: await tx });
    if (receipt.status !== "success")
        throw new Error(`L1 transaction ${receipt.transactionHash} reverted`);
}
