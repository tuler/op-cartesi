// The ABI drive (docs/ABI-DRIVE-SPEC.md): a raw flash drive recording which
// addresses a routed guest serves as contracts, and the ABI of each — the
// machine describing its own interface surface. Together with the accounts
// drive (which names the registered tokens), a stored snapshot answers "what
// does this chain speak?" without running any guest code: the host reads
// drive bytes, exactly as it reads balances.
//
// The drive is written once, at first boot, before the first yield — the
// manifest is fixed at snapshot build (EVM-COMPAT §10), so the recorded
// registry is genesis state covered by the genesis root, and no journaling
// is needed: nothing here changes during an input.
//
// Layout v1 (all integers little-endian):
//
//   header (64 B):
//      0  magic "ctsiabis" (8)
//      8  version  u16 = 1
//     10  flags    u16 = 0
//     12  capacity u32     index slots
//     16  count    u32     used slots
//     20  indexOffset u32  = 64
//     24  heapOffset  u32
//     28  heapLength  u32
//     32  heapUsed    u32
//     36  zero to 64
//
//   index (capacity × 32 B), at indexOffset:
//      0  address (20)
//     20  kind u8          0 system, 1 app
//     21  zero (1)
//     22  abiOffset u32    absolute drive offset of the blob
//     26  abiLength u32
//     30  zero (2)
//
//   heap at heapOffset: UTF-8 JSON ABI blobs (standard Solidity JSON ABI),
//   tightly packed in registration order.

import { FileStore, type Store } from "@cartesi/accounts";
import { type Address, getAddress, toBytes, toHex } from "viem";

export const ABI_DRIVE_MAGIC = "ctsiabis";
export const ABI_DRIVE_VERSION = 1;

export const KIND_SYSTEM = 0;
export const KIND_APP = 1;

const HEADER_LENGTH = 64;
const ENTRY_LENGTH = 32;

export type AbiDriveErrorKind =
    | "badMagic"
    | "badVersion"
    | "badGeometry"
    | "registryFull"
    | "heapFull"
    | "duplicate";

export class AbiDriveError extends Error {
    constructor(
        readonly kind: AbiDriveErrorKind,
        detail?: string,
    ) {
        super(detail ? `${kind}: ${detail}` : kind);
        this.name = "AbiDriveError";
    }
}

export interface AbiDriveConfig {
    driveLength: number;
    capacity: number;
    heapOffset: number;
}

/** The devnet geometry: a 256 KiB drive (2^18, one aligned subtree of the
 * machine's hash tree), 64 index slots, heap from 4096. Consensus once the
 * snapshot is stored, like the accounts drive's. */
export const DEVNET_ABI_DRIVE_CONFIG: AbiDriveConfig = {
    driveLength: 262144,
    capacity: 64,
    heapOffset: 4096,
};

export interface AbiEntry {
    address: Address;
    kind: number;
    /** The ABI, as the registered JSON text. */
    abi: string;
}

function u16(bytes: Uint8Array, off: number): number {
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint16(off, true);
}

function u32(bytes: Uint8Array, off: number): number {
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(off, true);
}

function putU16(bytes: Uint8Array, off: number, v: number): void {
    new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).setUint16(off, v, true);
}

function putU32(bytes: Uint8Array, off: number, v: number): void {
    new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).setUint32(off, v, true);
}

export class AbiDrive {
    private constructor(
        private readonly store: Store,
        private capacity: number,
        private count: number,
        private readonly indexOffset: number,
        private readonly heapOffset: number,
        private readonly heapLength: number,
        private heapUsed: number,
    ) {}

    static async format(
        store: Store,
        cfg: AbiDriveConfig = DEVNET_ABI_DRIVE_CONFIG,
    ): Promise<AbiDrive> {
        const indexEnd = HEADER_LENGTH + cfg.capacity * ENTRY_LENGTH;
        if (cfg.heapOffset < indexEnd || cfg.heapOffset >= cfg.driveLength) {
            throw new AbiDriveError(
                "badGeometry",
                `heap at ${cfg.heapOffset}, index ends at ${indexEnd}`,
            );
        }
        const header = new Uint8Array(HEADER_LENGTH);
        header.set(new TextEncoder().encode(ABI_DRIVE_MAGIC), 0);
        putU16(header, 8, ABI_DRIVE_VERSION);
        putU32(header, 12, cfg.capacity);
        putU32(header, 16, 0);
        putU32(header, 20, HEADER_LENGTH);
        putU32(header, 24, cfg.heapOffset);
        putU32(header, 28, cfg.driveLength - cfg.heapOffset);
        putU32(header, 32, 0);
        await store.writeAt(0, header);
        return new AbiDrive(
            store,
            cfg.capacity,
            0,
            HEADER_LENGTH,
            cfg.heapOffset,
            cfg.driveLength - cfg.heapOffset,
            0,
        );
    }

    static async open(store: Store): Promise<AbiDrive> {
        const header = await store.readAt(0, HEADER_LENGTH);
        if (new TextDecoder().decode(header.slice(0, 8)) !== ABI_DRIVE_MAGIC) {
            throw new AbiDriveError("badMagic");
        }
        if (u16(header, 8) !== ABI_DRIVE_VERSION) {
            throw new AbiDriveError("badVersion", `version ${u16(header, 8)}`);
        }
        return new AbiDrive(
            store,
            u32(header, 12),
            u32(header, 16),
            u32(header, 20),
            u32(header, 24),
            u32(header, 28),
            u32(header, 32),
        );
    }

    /** Open a formatted drive, or format a blank one — the guest's first-boot
     * entry point, mirroring the accounts drive's. */
    static async openOrFormat(
        store: Store,
        cfg: AbiDriveConfig = DEVNET_ABI_DRIVE_CONFIG,
    ): Promise<AbiDrive> {
        const magic = await store.readAt(0, 8);
        return new TextDecoder().decode(magic) === ABI_DRIVE_MAGIC
            ? AbiDrive.open(store)
            : AbiDrive.format(store, cfg);
    }

    get size(): number {
        return this.count;
    }

    /** Records a contract. Registering the same address again with identical
     * kind and ABI bytes is a no-op (boots are idempotent); with different
     * content it is a `duplicate` error. */
    async register(address: Address, kind: number, abiJson: string): Promise<void> {
        const blob = new TextEncoder().encode(abiJson);
        const existing = await this.entryOf(address);
        if (existing) {
            if (existing.kind === kind && existing.abi === abiJson) return;
            throw new AbiDriveError("duplicate", address);
        }
        if (this.count >= this.capacity) throw new AbiDriveError("registryFull");
        if (this.heapUsed + blob.length > this.heapLength) throw new AbiDriveError("heapFull");

        const abiOffset = this.heapOffset + this.heapUsed;
        await this.store.writeAt(abiOffset, blob);

        const entry = new Uint8Array(ENTRY_LENGTH);
        entry.set(toBytes(address), 0);
        entry[20] = kind;
        putU32(entry, 22, abiOffset);
        putU32(entry, 26, blob.length);
        await this.store.writeAt(this.indexOffset + this.count * ENTRY_LENGTH, entry);

        this.count += 1;
        this.heapUsed += blob.length;
        const counters = new Uint8Array(4);
        putU32(counters, 0, this.count);
        await this.store.writeAt(16, counters);
        putU32(counters, 0, this.heapUsed);
        await this.store.writeAt(32, counters);
    }

    async entries(): Promise<AbiEntry[]> {
        const out: AbiEntry[] = [];
        for (let i = 0; i < this.count; i++) {
            out.push(await this.entryAt(i));
        }
        return out;
    }

    async abiOf(address: Address): Promise<string | undefined> {
        return (await this.entryOf(address))?.abi;
    }

    private async entryAt(i: number): Promise<AbiEntry> {
        const raw = await this.store.readAt(this.indexOffset + i * ENTRY_LENGTH, ENTRY_LENGTH);
        const abiOffset = u32(raw, 22);
        const abiLength = u32(raw, 26);
        const blob = await this.store.readAt(abiOffset, abiLength);
        return {
            address: getAddress(toHex(raw.slice(0, 20))),
            kind: raw[20]!,
            abi: new TextDecoder().decode(blob),
        };
    }

    private async entryOf(address: Address): Promise<AbiEntry | undefined> {
        const wanted = getAddress(address);
        for (let i = 0; i < this.count; i++) {
            const e = await this.entryAt(i);
            if (e.address === wanted) return e;
        }
        return undefined;
    }

    /** Flushes drive bytes into machine state before a yield (spec §10);
     * a MemStore backing has no sync. */
    async sync(): Promise<void> {
        if (this.store instanceof FileStore) await this.store.sync();
    }
}
