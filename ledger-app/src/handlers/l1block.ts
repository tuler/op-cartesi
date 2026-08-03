// The adopted OP predeploy (EVM-COMPAT §6): L1Block at 0x4200…0015.
//
// op-node already addresses the L1-attributes deposit here every block, with
// the Ecotone/Isthmus tightly-packed calldata. The handler stores the decoded
// values and serves the standard views — so tooling that reads L1 context on
// OP chains works, and guest applications gain L1 context they previously
// had no access to.
//
// Packed layout after the 4-byte selector (Ecotone, offsets in bytes):
//   0  uint32  baseFeeScalar        4  uint32  blobBaseFeeScalar
//   8  uint64  sequenceNumber      16  uint64  timestamp
//  24  uint64  number              32  uint256 basefee
//  64  uint256 blobBaseFee         96  bytes32 hash
// 128  bytes32 batcherHash
// Isthmus appends: 160 uint32 operatorFeeScalar, 164 uint64 operatorFeeConstant.

import { decodeFunctionData, encodeFunctionResult, parseAbi, toFunctionSelector, toHex, type Hex } from "viem";
import { errorRevert } from "../evmcall.ts";
import type { Ledger } from "../ledger.ts";
import type { AdvanceOutcome, CallContext, Handler, OutputsSink, TxContext, ViewOutcome } from "../types.ts";

export const l1BlockAbi = parseAbi([
    "function number() view returns (uint64)",
    "function timestamp() view returns (uint64)",
    "function basefee() view returns (uint256)",
    "function blobBaseFee() view returns (uint256)",
    "function hash() view returns (bytes32)",
    "function sequenceNumber() view returns (uint64)",
    "function batcherHash() view returns (bytes32)",
    "function baseFeeScalar() view returns (uint32)",
    "function blobBaseFeeScalar() view returns (uint32)",
]);

const SET_ECOTONE: Hex = toFunctionSelector("setL1BlockValuesEcotone()");
const SET_ISTHMUS: Hex = toFunctionSelector("setL1BlockValuesIsthmus()");

interface L1Attributes {
    baseFeeScalar: number;
    blobBaseFeeScalar: number;
    sequenceNumber: bigint;
    timestamp: bigint;
    number: bigint;
    basefee: bigint;
    blobBaseFee: bigint;
    hash: Hex;
    batcherHash: Hex;
}

function u(view: DataView, off: number, bytes: number): bigint {
    let v = 0n;
    for (let i = 0; i < bytes; i++) v = (v << 8n) | BigInt(view.getUint8(off + i));
    return v;
}

export class L1Block implements Handler {
    payable = false;
    attributes: L1Attributes | null = null;

    async advance(ctx: TxContext, _out: OutputsSink, _ledger: Ledger): Promise<AdvanceOutcome> {
        if (ctx.data.length < 4) return { kind: "revert", data: errorRevert("unknown function") };
        const selector = toHex(ctx.data.slice(0, 4));
        if (selector !== SET_ECOTONE && selector !== SET_ISTHMUS) {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        const body = ctx.data.slice(4);
        if (body.length < 160) return { kind: "revert", data: errorRevert("short attributes") };
        const view = new DataView(body.buffer, body.byteOffset, body.byteLength);
        this.attributes = {
            baseFeeScalar: Number(u(view, 0, 4)),
            blobBaseFeeScalar: Number(u(view, 4, 4)),
            sequenceNumber: u(view, 8, 8),
            timestamp: u(view, 16, 8),
            number: u(view, 24, 8),
            basefee: u(view, 32, 32),
            blobBaseFee: u(view, 64, 32),
            hash: toHex(body.slice(96, 128)),
            batcherHash: toHex(body.slice(128, 160)),
        };
        return { kind: "accept" };
    }

    async view(call: CallContext, _ledger: Ledger): Promise<ViewOutcome> {
        let decoded: { functionName: string };
        try {
            decoded = decodeFunctionData({ abi: l1BlockAbi, data: toHex(call.data) }) as typeof decoded;
        } catch {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        const a = this.attributes;
        if (!a) return { kind: "revert", data: errorRevert("no L1 attributes yet") };
        const values: Record<string, unknown> = {
            number: a.number,
            timestamp: a.timestamp,
            basefee: a.basefee,
            blobBaseFee: a.blobBaseFee,
            hash: a.hash,
            sequenceNumber: a.sequenceNumber,
            batcherHash: a.batcherHash,
            baseFeeScalar: a.baseFeeScalar,
            blobBaseFeeScalar: a.blobBaseFeeScalar,
        };
        const fn = decoded.functionName;
        if (!(fn in values)) return { kind: "revert", data: errorRevert(`${fn} is not served`) };
        return {
            kind: "return",
            data: encodeFunctionResult({ abi: l1BlockAbi, functionName: fn as never, result: values[fn] as never }),
        };
    }
}
