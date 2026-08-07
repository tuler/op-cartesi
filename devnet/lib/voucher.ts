// The second half of every withdrawal, and the same half whatever is being
// withdrawn: the guest decides what the call is; this only proves the
// machine really emitted it. Wait for a proposal covering the voucher's
// block, open that proposal's root claim, take the outputs root out of it,
// and hand Cartesi's own verifier a proof against it.

import { type Address, type Hex, parseAbi } from "viem";
import { config, l1Public, l1Wallet, l2Public } from "./env.ts";

const factoryAbi = parseAbi([
    "function gameCount() view returns (uint256)",
    "function gameAtIndex(uint256 index) view returns (uint32, uint64, address)",
]);

const gameAbi = parseAbi(["function l2BlockNumber() view returns (uint256)"]);

const validatorAbi = parseAbi([
    "function accept(uint256 gameIndex, (bytes32,bytes32,bytes32,bytes32) claim)",
]);

const executorAbi = parseAbi(["function executeOutput(bytes output, (uint64,bytes32[]) proof)"]);

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Proves and executes the voucher emitted by an L2 transaction; returns the
 * executed output's chain-wide index. */
export async function executeVoucher(txHash: Hex): Promise<bigint> {
    const factory = config.disputeGameFactory;
    const validator = config.outputsValidator;
    const executor = config.outputExecutor;
    if (!factory || !validator || !executor) {
        throw new Error("run ./devnet/deploy-l1.sh and ./devnet/deploy-outputs.sh first");
    }
    const l1 = l1Public();
    const l2 = l2Public();
    const caller = l1Wallet(config.callerKey);

    // 1. The voucher's chain-wide index and the block that produced it.
    const receipt = await l2.waitForTransactionReceipt({ hash: txHash, pollingInterval: 2_000 });
    const emissions = await l2.getTransactionEmissions({ hash: txHash });
    const first = emissions.outputs[0];
    if (!first) throw new Error("the transaction emitted no provable output");
    const index = BigInt(first.index);
    const outBlock = receipt.blockNumber;
    console.error(`  voucher is output ${index}, in L2 block ${outBlock}`);

    // 2. Wait for a proposal whose L2 block is at or past the voucher's.
    console.error(`  waiting for a proposal covering L2 block ${outBlock}`);
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
            if (block >= outBlock) {
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

    // 3. Open that proposal's root claim on L1. The four words are the
    //    preimage op-node hashed; the third is the Cartesi outputs root.
    const block = await l2.getBlock({ blockNumber: proposedBlock });
    if (!block.withdrawalsRoot) throw new Error(`L2 block ${proposedBlock} has no withdrawalsRoot`);
    const zero: Hex = "0x0000000000000000000000000000000000000000000000000000000000000000";
    await waitL1(
        l1,
        caller.writeContract({
            address: validator,
            abi: validatorAbi,
            functionName: "accept",
            args: [gameIndex, [zero, block.stateRoot, block.withdrawalsRoot, block.hash]],
        }),
    );
    console.error(`  accepted outputs root ${block.withdrawalsRoot} from game ${gameIndex}`);

    // 4. Prove the voucher against that block and execute it. The proof is
    //    against the proposed block's tree, not the emitting block's — a
    //    withdrawal has to stay provable as the tree grows past it.
    const proof = await l2.getOutputProof({ index, blockNumber: proposedBlock });
    await waitL1(
        l1,
        caller.writeContract({
            address: executor,
            abi: executorAbi,
            functionName: "executeOutput",
            args: [proof.output, [index, proof.outputHashesSiblings]],
        }),
    );
    console.error(`  executed output ${index}`);
    return index;
}

async function waitL1(l1: ReturnType<typeof l1Public>, tx: Promise<Hex>): Promise<void> {
    const receipt = await l1.waitForTransactionReceipt({ hash: await tx });
    if (receipt.status !== "success")
        throw new Error(`L1 transaction ${receipt.transactionHash} reverted`);
}
