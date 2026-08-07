// The ABI drive against its spec (docs/ABI-DRIVE-SPEC.md): format, register,
// reopen from bytes, and every refusal.

import { MemStore } from "@cartesi/accounts";
import { describe, expect, it } from "vitest";
import {
    AbiDrive,
    AbiDriveError,
    DEVNET_ABI_DRIVE_CONFIG,
    KIND_APP,
    KIND_SYSTEM,
} from "../src/index.ts";

const BRIDGE = "0xC751000000000000000000000000000000000001";
const APP = "0xC0DE000000000000000000000000000000000001";

const bridgeAbi = JSON.stringify([
    {
        type: "function",
        name: "withdrawEther",
        stateMutability: "payable",
        inputs: [{ name: "to", type: "address" }],
        outputs: [],
    },
]);
const appAbi = JSON.stringify([
    { type: "function", name: "increment", stateMutability: "nonpayable", inputs: [], outputs: [] },
]);

function store(): MemStore {
    return new MemStore(DEVNET_ABI_DRIVE_CONFIG.driveLength);
}

describe("abi drive", () => {
    it("registers and reads back, and a reopen sees the same bytes", async () => {
        const mem = store();
        const drive = await AbiDrive.format(mem);
        await drive.register(BRIDGE, KIND_SYSTEM, bridgeAbi);
        await drive.register(APP, KIND_APP, appAbi);

        const reopened = await AbiDrive.open(mem);
        expect(reopened.size).toBe(2);
        const entries = await reopened.entries();
        expect(entries.map((e) => e.address.toLowerCase())).toEqual([
            BRIDGE.toLowerCase(),
            APP.toLowerCase(),
        ]);
        expect(entries.map((e) => e.kind)).toEqual([KIND_SYSTEM, KIND_APP]);
        expect(await reopened.abiOf(APP)).toBe(appAbi);
    });

    it("openOrFormat formats blank bytes and opens formatted ones", async () => {
        const mem = store();
        const first = await AbiDrive.openOrFormat(mem);
        await first.register(APP, KIND_APP, appAbi);
        const second = await AbiDrive.openOrFormat(mem);
        expect(second.size).toBe(1);
    });

    it("re-registering identical content is a no-op; different content is a duplicate", async () => {
        const drive = await AbiDrive.format(store());
        await drive.register(APP, KIND_APP, appAbi);
        await drive.register(APP, KIND_APP, appAbi);
        expect(drive.size).toBe(1);
        await expect(drive.register(APP, KIND_APP, bridgeAbi)).rejects.toThrowError(AbiDriveError);
        await expect(drive.register(APP, KIND_SYSTEM, appAbi)).rejects.toThrowError(/duplicate/);
    });

    it("refuses past capacity and past the heap", async () => {
        const tiny = await AbiDrive.format(store(), {
            driveLength: 262144,
            capacity: 1,
            heapOffset: 4096,
        });
        await tiny.register(APP, KIND_APP, appAbi);
        await expect(tiny.register(BRIDGE, KIND_SYSTEM, bridgeAbi)).rejects.toThrowError(
            /registryFull/,
        );

        const shallow = await AbiDrive.format(store(), {
            driveLength: 4106,
            capacity: 2,
            heapOffset: 4096,
        });
        await expect(shallow.register(APP, KIND_APP, appAbi)).rejects.toThrowError(/heapFull/);
    });

    it("refuses foreign bytes and foreign versions", async () => {
        await expect(AbiDrive.open(store())).rejects.toThrowError(/badMagic/);
        const mem = store();
        await AbiDrive.format(mem);
        await mem.writeAt(8, new Uint8Array([9, 0]));
        await expect(AbiDrive.open(mem)).rejects.toThrowError(/badVersion/);
    });

    it("refuses a heap that would overlap the index", async () => {
        await expect(
            AbiDrive.format(store(), { driveLength: 262144, capacity: 64, heapOffset: 64 }),
        ).rejects.toThrowError(/badGeometry/);
    });
});
