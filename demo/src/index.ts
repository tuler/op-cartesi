// The devnet application: @op-cartesi/app booted with the devnet genesis.
//
// Everything standard — admission, routing, the built-ins, the drives —
// lives in the library. What remains here is what is genuinely this
// application's: genesis parameters from the image environment (part of the
// snapshot, so covered by the genesis state root; defaults match
// devnet/lib/env.ts), and the app-specific contracts.

import { Application } from "@op-cartesi/app";
import { getAddress, parseAbi } from "viem";

const CHAIN_ID = BigInt(process.env.CHAIN_ID ?? "901");
const OWNER = getAddress(process.env.OWNER ?? "0x90F79bf6EB2c4f870365E785982E1f101E93b906");

async function main(): Promise<void> {
    const app = await Application.boot({ chainId: CHAIN_ID, owner: OWNER });

    // The devnet's one application contract: a counter — the smallest honest
    // demonstration of the fall-through. An address outside every reserved
    // namespace, ABI-driven dispatch, and state that is simply a variable:
    // RAM belongs to the application, is machine state like any other, and
    // does not roll back (EVM-COMPAT §10a). Recorded in the ABI drive, so
    // `cast call` and viem can discover and speak to it from the snapshot
    // alone.
    const counterAbi = parseAbi([
        "function increment()",
        "function count() view returns (uint256)",
        "function blocks() view returns (uint256)",
    ]);
    let count = 0n;
    // The other half of the demonstration: a number that moves with no
    // transactions at all. onBlock runs at the head of every block — the
    // chain's own attributes deposit is the input it rides — so `blocks()`
    // climbs on an idle devnet while `count()` sits still.
    let blocks = 0n;
    await app.contract({
        address: "0xc0de000000000000000000000000000000000001",
        abi: counterAbi,
        transactions: {
            increment: () => {
                count += 1n;
            },
        },
        views: {
            count: () => count,
            blocks: () => blocks,
        },
        onBlock: () => {
            blocks += 1n;
        },
    });

    await app.run();
}

main().catch((e) => {
    // An escaped error would halt the machine, and a halted machine is a
    // halted chain. run() only rejects when finish itself fails, i.e. the
    // rollup device is gone — nothing left to do but say so.
    console.error(e);
    process.exit(1);
});
