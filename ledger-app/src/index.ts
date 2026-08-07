// The devnet guest: @cartesi/routed-guest booted with the devnet genesis.
//
// Everything standard — admission, routing, the built-ins, the drives —
// lives in the library. What remains here is what is genuinely this
// application's: genesis parameters from the image environment (part of the
// snapshot, so covered by the genesis state root; defaults match
// devnet/env.sh), and the app-specific contracts.

import { Guest } from "@cartesi/routed-guest";
import { getAddress, parseAbi } from "viem";

const CHAIN_ID = BigInt(process.env.CHAIN_ID ?? "901");
const OWNER = getAddress(process.env.OWNER ?? "0x90F79bf6EB2c4f870365E785982E1f101E93b906");

async function main(): Promise<void> {
    const guest = await Guest.boot({ chainId: CHAIN_ID, owner: OWNER });

    // The devnet's one application contract: a counter — the smallest honest
    // demonstration of the fall-through. An address outside every reserved
    // namespace, ABI-driven dispatch, and revert-safe state in a journaled
    // store. Recorded in the ABI drive, so `cast call` and viem can discover
    // and speak to it from the snapshot alone.
    const counterAbi = parseAbi([
        "function increment()",
        "function count() view returns (uint256)",
    ]);
    const state = guest.store(8);
    const read = async () => {
        const b = await state.readAt(0, 8);
        return new DataView(b.buffer, b.byteOffset, b.byteLength).getBigUint64(0, true);
    };
    await guest.contract({
        address: "0xc0de000000000000000000000000000000000001",
        abi: counterAbi,
        transactions: {
            increment: async () => {
                const b = new Uint8Array(8);
                new DataView(b.buffer).setBigUint64(0, (await read()) + 1n, true);
                await state.writeAt(0, b);
            },
        },
        views: {
            count: () => read(),
        },
    });

    await guest.run();
}

main().catch((e) => {
    // An escaped error would halt the machine, and a halted machine is a
    // halted chain. run() only rejects when finish itself fails, i.e. the
    // rollup device is gone — nothing left to do but say so.
    console.error(e);
    process.exit(1);
});
