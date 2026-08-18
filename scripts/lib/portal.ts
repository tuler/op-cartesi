// The L1 half of every OP-path withdrawal, on viem's op-stack actions — the
// same calls the withdrawal guide (viem.sh/op-stack/guides/withdrawals) runs
// against any OP chain: waitToProve finds the dispute game covering the
// withdrawal's block, buildProveWithdrawal fetches the message passer's
// storage proof with eth_getProof, and proveWithdrawal / finalizeWithdrawal
// drive OptimismPortal. The withdrawal itself comes off the L2 receipt's
// MessagePassed log, so callers hand over a receipt and nothing else.
//
// One deliberate departure from the guide: no waitToFinalize. It measures
// the proof's age against the caller's wall clock, and this devnet advances
// *anvil's* clock past the delays instead of living through a week of them —
// the two clocks disagree by exactly the amount skipped, so waitToFinalize
// would wait out the real seven days. The devnet plumbing between prove and
// finalize (advance time, resolve the permissioned game by hand, advance
// past the air gap) stands in for it.

import { config } from "devnet/env";
import { l1Public, l1Wallet, l2Chain, l2Public } from "devnet/wallet";
import { getAddress, parseAbi, type TransactionReceipt, toFunctionSelector } from "viem";

const gameAbi = parseAbi([
    "function status() view returns (uint8)",
    "function resolve() returns (uint8)",
    "function resolveClaim(uint256 claimIndex, uint256 numToResolve)",
    "function maxClockDuration() view returns (uint64)",
]);

const portalDelaysAbi = parseAbi([
    "function proofMaturityDelaySeconds() view returns (uint256)",
    "function disputeGameFinalityDelaySeconds() view returns (uint256)",
]);

// The portal's custom errors, by selector — viem's own portal ABI carries
// none of them, so without this a revert prints four bytes and nothing else.
const portalErrorNames = new Map(
    [
        "OptimismPortal_InvalidRootClaim",
        "OptimismPortal_ProofNotOldEnough",
        "OptimismPortal_Unproven",
        "OptimismPortal_InvalidProofTimestamp",
        "OptimismPortal_ImproperDisputeGame",
        "OptimismPortal_InvalidDisputeGame",
        "OptimismPortal_AlreadyFinalized",
        "OptimismPortal_InvalidMerkleProof",
        "OptimismPortal_InvalidOutputRootProof",
        "OptimismPortal_BadTarget",
        "OptimismPortal_CallPaused",
        "OptimismPortal_GasEstimation",
    ].map((name) => [toFunctionSelector(`${name}()`).toLowerCase(), name]),
);

/** Rethrows a portal revert with its error named, when the selector in the
 * message is one of the portal's. */
function explainPortalRevert(e: unknown): never {
    const match = e instanceof Error ? e.message.match(/0x[0-9a-fA-F]{8}/) : null;
    const name = match && portalErrorNames.get(match[0].toLowerCase());
    if (e instanceof Error && name) {
        e.message = `${name} — ${e.message}`;
    }
    throw e;
}

/** Proves and finalizes the withdrawal carried by an L2 transaction receipt
 * through OptimismPortal. */
export async function proveAndFinalizeWithdrawal(receipt: TransactionReceipt): Promise<void> {
    if (!config.disputeGameFactory) throw new Error("run the devnet with contracts deployed first");
    const l1 = l1Public();
    const l2 = l2Public();
    const caller = l1Wallet(config.callerKey);
    const portal = config.depositContract;

    // 1. Wait for a proposal (a dispute game) at or past the withdrawal's
    // block. waitToProve also lifts the withdrawal off the receipt's
    // MessagePassed log — the log the guest synthesizes for exactly this.
    console.error(`  waiting for a proposal covering L2 block ${receipt.blockNumber}`);
    const { game, output, withdrawal } = await l1.waitToProve({
        receipt,
        targetChain: l2Chain,
        pollingInterval: 4_000,
    });
    console.error(`  withdrawal ${withdrawal.withdrawalHash}`);
    console.error(`  game ${output.outputIndex} proposes L2 block ${output.l2BlockNumber}`);

    // 2. The storage proof against the proposed block: eth_getProof on the
    // message passer, answered from the block's withdrawal trie.
    const proveArgs = await l2.buildProveWithdrawal({ output, withdrawal });

    // 3. Prove.
    await waitL1(l1, caller.proveWithdrawal(proveArgs).catch(explainPortalRevert));
    console.error(`  proven against OptimismPortal at ${portal}`);

    // 4. Outlive the delays and resolve the game — the devnet plumbing the
    // header explains. The game proxy's address rides in the metadata word
    // of the factory's GameId: (type ‖ timestamp ‖ address).
    const gameProxy = getAddress(`0x${game.metadata.slice(26)}`);
    const skipTime = async (seconds: bigint, what: string) => {
        console.error(`  advancing anvil time by ${seconds}s (${what})`);
        await l1.request({
            method: "evm_increaseTime" as never,
            params: [Number(seconds)] as never,
        });
        await l1.request({ method: "evm_mine" as never, params: [] as never });
    };
    const maturity = await l1.readContract({
        address: portal,
        abi: portalDelaysAbi,
        functionName: "proofMaturityDelaySeconds",
    });
    const clock = await l1.readContract({
        address: gameProxy,
        abi: gameAbi,
        functionName: "maxClockDuration",
    });
    await skipTime(
        (maturity > clock ? maturity : clock) + 60n,
        `proof maturity ${maturity}s, game clock ${clock}s`,
    );

    const status = await l1.readContract({
        address: gameProxy,
        abi: gameAbi,
        functionName: "status",
    });
    if (status === 0) {
        console.error("  resolving the dispute game");
        await waitL1(
            l1,
            caller.writeContract({
                address: gameProxy,
                abi: gameAbi,
                functionName: "resolveClaim",
                args: [0n, 0n],
            }),
        );
        await waitL1(
            l1,
            caller.writeContract({ address: gameProxy, abi: gameAbi, functionName: "resolve" }),
        );
    }

    // The anchor registry accepts a game's claim only once it has been
    // *resolved* for the finality delay (the air gap) — the clock starts at
    // resolution, not at creation, so this jump has to come after resolve
    // or finalize reverts with OptimismPortal_InvalidRootClaim.
    const finality = await l1.readContract({
        address: portal,
        abi: portalDelaysAbi,
        functionName: "disputeGameFinalityDelaySeconds",
    });
    await skipTime(finality + 60n, `dispute game finality delay ${finality}s`);

    // 5. Finalize: the portal executes the withdrawal's call.
    await waitL1(
        l1,
        caller.finalizeWithdrawal({ targetChain: l2Chain, withdrawal }).catch(explainPortalRevert),
    );
    console.error("  finalized through OptimismPortal");
}

async function waitL1(l1: ReturnType<typeof l1Public>, tx: Promise<`0x${string}`>): Promise<void> {
    const receipt = await l1.waitForTransactionReceipt({ hash: await tx });
    if (receipt.status !== "success")
        throw new Error(`L1 transaction ${receipt.transactionHash} reverted`);
}
