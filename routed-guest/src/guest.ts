// The guest: boots the drives, owns the router, registers application
// contracts, records the manifest into the ABI drive, and runs the rollup
// loop on @deroll/cmio.
//
// Boot order matters (EVM-COMPAT §10): both drives are opened — and on
// first boot formatted and populated — before the first yield, so the
// accounts header and the recorded ABI registry are genesis state, covered
// by the genesis root. Registration is therefore a boot-time act: contracts
// are declared between boot() and run(), never during an input.

import { AbiDrive, type AbiDriveConfig, KIND_APP, KIND_SYSTEM } from "@cartesi/abis";
import { FileStore, MemStore, type Store } from "@cartesi/accounts";
import {
    type BlockContext,
    BRIDGE_ADDRESS,
    bridgeAbi,
    CONFIG_ADDRESS,
    configAbi,
    L1BLOCK_ADDRESS,
    l1BlockAbi,
    REGISTRY_ADDRESS,
    registryAbi,
    sameAddress,
} from "@cartesi/evm-compat";
import { type AdvanceRequest, Rollup } from "@deroll/cmio";
import { type Abi, type Address, getAddress, numberToHex } from "viem";
import { type ContractSpec, contractHandler } from "./contract.ts";
import { Ledger } from "./ledger.ts";
import { Router } from "./router.ts";

export interface GuestOptions {
    chainId: bigint;
    owner: Address;
    /** Accounts drive device; defaults to $ACCOUNTS_DRIVE, then /dev/pmem1. */
    accountsDevice?: string;
    /** ABI drive device; defaults to $ABI_DRIVE, then /dev/pmem2. */
    abiDevice?: string;
    abiDriveConfig?: AbiDriveConfig;
}

/** The system built-ins the boot records into the ABI drive (kind 0). Token
 * façades and the portal receiver are deliberately absent — discoverable
 * from the accounts drive and the standard (ABI-DRIVE-SPEC §4). */
const BUILTINS: ReadonlyArray<{ address: Address; abi: Abi }> = [
    { address: REGISTRY_ADDRESS, abi: registryAbi },
    { address: BRIDGE_ADDRESS, abi: bridgeAbi },
    { address: CONFIG_ADDRESS, abi: configAbi },
    { address: L1BLOCK_ADDRESS, abi: l1BlockAbi },
];

function inSystemNamespace(address: Address): boolean {
    // The reservation is the full pattern (EVM-COMPAT §6): 0xC751 followed by
    // sixteen zero bytes. Application addresses must stay out of it.
    return /^0xc751(00){16}[0-9a-f]{4}$/.test(address.toLowerCase());
}

export class Guest {
    private constructor(
        readonly ledger: Ledger,
        readonly router: Router,
        private readonly abiDrive: AbiDrive,
    ) {}

    private static async assemble(
        ledger: Ledger,
        abiStore: Store,
        opts: GuestOptions,
    ): Promise<Guest> {
        const abiDrive = await AbiDrive.openOrFormat(abiStore, opts.abiDriveConfig);
        const router = new Router(ledger, { chainId: opts.chainId, owner: opts.owner });
        for (const b of BUILTINS) {
            await abiDrive.register(b.address, KIND_SYSTEM, JSON.stringify(b.abi));
        }
        return new Guest(ledger, router, abiDrive);
    }

    /** Boot from the machine's flash devices — the guest entry point. */
    static async boot(opts: GuestOptions): Promise<Guest> {
        const accountsDevice = opts.accountsDevice ?? process.env.ACCOUNTS_DRIVE ?? "/dev/pmem1";
        const abiDevice = opts.abiDevice ?? process.env.ABI_DRIVE ?? "/dev/pmem2";
        const ledger = await Ledger.openDevice(accountsDevice);
        return Guest.assemble(ledger, await FileStore.open(abiDevice), opts);
    }

    /** Boot over in-memory stores — the test entry point. */
    static async inMemory(opts: GuestOptions, abiDriveLength = 262144): Promise<Guest> {
        const ledger = await Ledger.inMemory();
        return Guest.assemble(ledger, new MemStore(abiDriveLength), opts);
    }

    /** Registers an application contract: routes its address, and records
     * address + ABI in the ABI drive so the chain and outside tooling can
     * discover it from the snapshot alone. */
    async contract<const abi extends Abi>(spec: ContractSpec<abi>): Promise<void> {
        const address = getAddress(spec.address);
        if (inSystemNamespace(address) || sameAddress(address, L1BLOCK_ADDRESS)) {
            throw new Error(`${address} is reserved by the standard`);
        }
        this.router.register(address, contractHandler(spec));
        await this.abiDrive.register(address, KIND_APP, JSON.stringify(spec.abi));
    }

    /** What the ABI drive records — for tests and introspection. */
    contracts(): Promise<{ address: Address; kind: number; abi: string }[]> {
        return this.abiDrive.entries();
    }

    /** The rollup loop. Runs forever; resolves only if the rollup device
     * itself fails. */
    async run(): Promise<void> {
        await this.abiDrive.sync();
        await this.ledger.sync();

        const rollup = new Rollup();
        await rollup.run({
            advance: async (req, r) => {
                const block = blockContext(req);
                this.router.lastBlock = block;
                const res = await this.router.advance(block, new Uint8Array(req.payload));
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
                await this.ledger.sync();
                return res.accept;
            },
            inspect: async (req, r) => {
                const res = await this.router.inspect(new Uint8Array(req.payload));
                for (const report of res.reports) r.emitReport(report);
                // The inspect runs on a discarded machine fork; sync anyway so
                // a mid-inspect read of the fork stays coherent.
                await this.ledger.sync();
                return res.accept;
            },
        });
    }
}

function blockContext(req: AdvanceRequest): BlockContext {
    return {
        chainId: req.chainId,
        appContract: getAddress(req.appContract),
        blockNumber: req.blockNumber,
        timestamp: req.blockTimestamp,
        prevRandao: req.prevRandao,
        inputIndex: req.index,
    };
}
