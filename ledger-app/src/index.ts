// The routed guest (docs/EVM-COMPAT.md), on @deroll/cmio.
//
// This sits directly on the Rollup binding rather than @deroll/app: the
// router needs what the app layer's handler-chain model hides — rejecting an
// inspect (revert-with-data for eth_call), and dispatch by address instead
// of each handler sniffing every payload. libcmt underneath keeps the two
// duties EVM-COMPAT §4 names: the yield protocol and the outputs-root
// accumulator in the TX buffer.
//
// The EvmAdvance envelope is transport framing only: block context is read
// from it; msgSender is never an authority (the router recovers signers and
// reads deposit senders itself).

import { Rollup, type AdvanceRequest } from "@deroll/cmio";
import { getAddress, numberToHex, type Address } from "viem";
import { Ledger } from "./ledger.ts";
import { Router } from "./router.ts";
import type { BlockContext } from "./types.ts";

// Genesis parameters, from the image environment (part of the snapshot, so
// covered by the genesis state root). Defaults match devnet/env.sh.
const CHAIN_ID = BigInt(process.env.CHAIN_ID ?? "901");
const OWNER = getAddress(process.env.OWNER ?? "0x90F79bf6EB2c4f870365E785982E1f101E93b906");
// The accounts drive: second flash drive after root, exactly as
// devnet/build-snapshot.sh orders them.
const ACCOUNTS_DRIVE = process.env.ACCOUNTS_DRIVE ?? "/dev/pmem1";

function blockContext(req: AdvanceRequest): BlockContext {
    return {
        chainId: req.chainId,
        appContract: getAddress(req.appContract) as Address,
        blockNumber: req.blockNumber,
        timestamp: req.blockTimestamp,
        prevRandao: req.prevRandao,
        inputIndex: req.index,
    };
}

async function main(): Promise<void> {
    // Open (or, on first boot, format) the drive before the first yield, so
    // the formatted header is part of the stored snapshot and the genesis
    // state root covers it — the same ordering the Lua guest relied on.
    const ledger = await Ledger.openDevice(ACCOUNTS_DRIVE);
    await ledger.sync();
    const router = new Router(ledger, { chainId: CHAIN_ID, owner: OWNER });

    const rollup = new Rollup();
    await rollup.run({
        advance: async (req, r) => {
            const block = blockContext(req);
            router.lastBlock = block;
            const res = await router.advance(block, new Uint8Array(req.payload));
            if (res.accept) {
                for (const e of res.emissions) {
                    if (e.kind === "notice") r.emitNotice(e.payload);
                    else if (e.kind === "voucher") r.emitVoucher(e);
                    else r.emitReport(e.payload);
                }
            } else if (res.reason) {
                // Rejection rolls the machine back, outputs included; the
                // reason still reaches the host as a report, which survive.
                r.emitReport(numberToHex(0x02, { size: 1 }));
                console.error(`reject input ${req.index}: ${res.reason}`);
            }
            // Drive bytes must be current in machine state at the yield
            // (spec §10) — run() calls finish right after we return.
            await ledger.sync();
            return res.accept;
        },
        inspect: async (req, r) => {
            const res = await router.inspect(new Uint8Array(req.payload));
            for (const report of res.reports) r.emitReport(report);
            // The inspect runs on a discarded machine fork; sync anyway so a
            // mid-inspect read of the fork stays coherent.
            await ledger.sync();
            return res.accept;
        },
    });
}

main().catch((e) => {
    // An escaped error would halt the machine, and a halted machine is a
    // halted chain. run() only rejects when finish itself fails, i.e. the
    // rollup device is gone — nothing left to do but say so.
    console.error(e);
    process.exit(1);
});
