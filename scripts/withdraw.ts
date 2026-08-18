#!/usr/bin/env bun

// Runs an ether withdrawal end to end, through the standard OP Stack path:
// ask the guest for one, then prove and finalize it against OptimismPortal.
//
//   bun scripts/withdraw.ts <recipient> <wei>
//
// This targets the routed guest of `app/`: the withdrawal is a call to the
// bridge predeploy's withdrawEther(to), carrying the wei as msg.value — so
// the sender (SENDER_KEY) must hold that balance on L2; deposit first. The
// bridge burns the value and emits an OP Withdrawal message; the hash's
// sentMessages slot enters the block's withdrawal trie, whose root is the
// header's withdrawalsRoot — the messagePasserStorageRoot of the output root
// op-proposer submits. From there it is the stock flow every OP chain runs:
// eth_getProof for the slot, proveWithdrawalTransaction, wait out the proof
// maturity delay, finalizeWithdrawalTransaction. The ETH comes out of the
// same lockbox the deposits funded.
//
// The devnet's permissioned game must also pass the portal's game-validity
// check at finalization; the script resolves the game and advances anvil
// time past the delays, which is devnet plumbing rather than protocol.

import {
    BRIDGE_ADDRESS,
    bridgeAbi,
    decodeWithdrawalOutput,
    withdrawalHash,
    withdrawalSlot,
} from "@op-cartesi/evm";
import { config, usage } from "devnet/env";
import { l1Public, l1Wallet, l2Public } from "devnet/wallet";
import { type Address, encodeFunctionData, getAddress, type Hex, parseAbi } from "viem";
import { sendL2Tx } from "./lib/l2.ts";

const MESSAGE_PASSER: Address = "0x4200000000000000000000000000000000000016";

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

const [toArg, amountArg] = process.argv.slice(2);
if (!toArg || !amountArg) usage("withdraw.ts <recipient> <wei>");
const to = getAddress(toArg);
const amount = BigInt(amountArg);

const factory = config.disputeGameFactory;
if (!factory) usage("run the devnet with contracts deployed first");

const l1 = l1Public();
const l2 = l2Public();
const caller = l1Wallet(config.callerKey);
const portal = config.depositContract;

console.error(`asking the guest to withdraw ${amount} wei to ${to}`);
const txHash = await sendL2Tx({
    to: BRIDGE_ADDRESS,
    data: encodeFunctionData({ abi: bridgeAbi, functionName: "withdrawEther", args: [to] }),
    value: amount,
});
console.error(`  L2 tx ${txHash}`);

// 1. The withdrawal message the guest emitted, from the recorded emissions.
const receipt = await l2.waitForTransactionReceipt({ hash: txHash, pollingInterval: 2_000 });
const emissions = await l2.getTransactionEmissions({ hash: txHash });
const message = emissions.outputs
    .map((o) => decodeWithdrawalOutput(o.data))
    .find((w) => w !== undefined);
if (!message) throw new Error("the transaction emitted no withdrawal message");
const wHash = withdrawalHash(message);
console.error(`  withdrawal ${wHash}, in L2 block ${receipt.blockNumber}`);

// 2. Wait for a proposal whose L2 block is at or past the withdrawal's.
console.error(`  waiting for a proposal covering L2 block ${receipt.blockNumber}`);
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
        if (block >= receipt.blockNumber) {
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

// 3. The storage proof: eth_getProof for the withdrawal's sentMessages slot
//    against the proposed block — the exact call viem's buildProveWithdrawal
//    makes on any OP chain.
const slot = withdrawalSlot(wHash);
const proof = await l2.getProof({
    address: MESSAGE_PASSER,
    storageKeys: [slot],
    blockNumber: proposedBlock,
});
const block = await l2.getBlock({ blockNumber: proposedBlock });
if (!block.withdrawalsRoot) throw new Error(`L2 block ${proposedBlock} has no withdrawalsRoot`);
if (proof.storageHash !== block.withdrawalsRoot) {
    throw new Error(`storageHash ${proof.storageHash} != withdrawalsRoot ${block.withdrawalsRoot}`);
}

// 4. Prove against the portal.
const withdrawalTx = {
    nonce: message.nonce,
    sender: message.sender,
    target: message.target,
    value: message.value,
    gasLimit: message.gasLimit,
    data: message.data,
} as const;
const zero: Hex = "0x0000000000000000000000000000000000000000000000000000000000000000";
await waitL1(
    caller.writeContract({
        address: portal,
        abi: portalAbi,
        functionName: "proveWithdrawalTransaction",
        args: [
            withdrawalTx,
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

// 5. Resolve the game and outlive the delays — devnet plumbing: anvil time
//    is advanced past the game clock and the proof maturity delay.
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
        caller.writeContract({
            address: game!,
            abi: gameAbi,
            functionName: "resolveClaim",
            args: [0n, 0n],
        }),
    );
    await waitL1(caller.writeContract({ address: game!, abi: gameAbi, functionName: "resolve" }));
}

// 6. Finalize: the portal pays the recipient from the lockbox.
const before = await l1.getBalance({ address: to });
await waitL1(
    caller.writeContract({
        address: portal,
        abi: portalAbi,
        functionName: "finalizeWithdrawalTransaction",
        args: [withdrawalTx],
    }),
);
const after = await l1.getBalance({ address: to });

console.error("");
console.error("withdrawal finalized through OptimismPortal");
console.error(`  recipient ${to}`);
console.error(`  balance   ${before} -> ${after}`);

async function waitL1(tx: Promise<Hex>): Promise<void> {
    const r = await l1.waitForTransactionReceipt({ hash: await tx });
    if (r.status !== "success") throw new Error(`L1 transaction ${r.transactionHash} reverted`);
}
