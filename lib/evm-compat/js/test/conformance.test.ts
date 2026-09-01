// Replays the shared conformance vectors (../../../../conformance) against this
// package's encoders.
//
// These encodings have two independent implementations — the guest's, here,
// and the node's, in Go — and before the vectors existed they were pinned to
// each other by constants copied from one side into the other's tests. Now
// both sides read one file: the Go generator writes it
// (host/go/chain/conformance_test.go,
// which still asserts the historical constants so the migration is lossless),
// and this replays it. A drift in either encoding breaks here, and it breaks
// before it breaks a withdrawal on L1.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
    type Address,
    concat,
    encodeAbiParameters,
    type Hex,
    keccak256,
    parseAbiParameters,
} from "viem";
import { EVM_LOG_SELECTOR, encodeEvmLog } from "../src/events.ts";
import { encodeEvmCall } from "../src/evmcall.ts";
import {
    decodeWithdrawalOutput,
    encodeWithdrawal,
    NOTICE_SELECTOR,
    WITHDRAWAL_SELECTOR,
    withdrawalHash,
    withdrawalSlot,
} from "../src/withdrawal.ts";

const conformance = join(fileURLToPath(new URL(".", import.meta.url)), "../../../../conformance");

function vector<T>(path: string): T {
    return JSON.parse(readFileSync(join(conformance, path), "utf8")) as T;
}

/** Cartesi's Notice(bytes) framing, the wire form every output travels
 * under. The guest emits it through @cartesi/rollup; reproduced here so the
 * vectors' `output` field can be checked without a machine. */
function notice(payload: Hex): Hex {
    return concat([NOTICE_SELECTOR, encodeAbiParameters(parseAbiParameters("bytes"), [payload])]);
}

const bytes = (hex: Hex): Uint8Array =>
    hex === "0x" ? new Uint8Array(0) : Buffer.from(hex.slice(2), "hex");

interface WithdrawalFile {
    selector: Hex;
    noticeSelector: Hex;
    cases: {
        name: string;
        withdrawal: {
            nonce: string;
            sender: Address;
            target: Address;
            value: string;
            gasLimit: string;
            data: Hex;
        };
        payload: Hex;
        output: Hex;
        hash: Hex;
        slot: Hex;
    }[];
    notWithdrawals: { name: string; output: Hex }[];
}

test("withdrawal encodings match the vectors", () => {
    const file = vector<WithdrawalFile>("encodings/withdrawal.json");
    assert.equal(file.selector, WITHDRAWAL_SELECTOR);
    assert.equal(file.noticeSelector, NOTICE_SELECTOR);

    for (const c of file.cases) {
        const w = {
            nonce: BigInt(c.withdrawal.nonce),
            sender: c.withdrawal.sender,
            target: c.withdrawal.target,
            value: BigInt(c.withdrawal.value),
            gasLimit: BigInt(c.withdrawal.gasLimit),
            data: c.withdrawal.data,
        };
        assert.equal(encodeWithdrawal(w), c.payload, `${c.name}: payload`);
        assert.equal(notice(c.payload), c.output, `${c.name}: notice framing`);
        assert.equal(withdrawalHash(w), c.hash, `${c.name}: hash`);
        assert.equal(withdrawalSlot(c.hash), c.slot, `${c.name}: sentMessages slot`);

        // And the decoder recognizes what the node recorded.
        const decoded = decodeWithdrawalOutput(c.output);
        assert.ok(decoded, `${c.name}: the vector's output did not decode`);
        assert.deepEqual(decoded, w, `${c.name}: round trip`);
    }

    for (const n of file.notWithdrawals) {
        assert.equal(decodeWithdrawalOutput(n.output), undefined, `${n.name} decoded`);
    }
});

interface EvmLogFile {
    selector: Hex;
    cases: {
        name: string;
        emitter: Address;
        topics: Hex[];
        data: Hex;
        payload: Hex;
        output: Hex;
    }[];
}

test("event encodings match the vectors", () => {
    const file = vector<EvmLogFile>("encodings/evmlog.json");
    assert.equal(file.selector, EVM_LOG_SELECTOR);

    for (const c of file.cases) {
        assert.equal(encodeEvmLog(c.emitter, c.topics, c.data), c.payload, `${c.name}: payload`);
        assert.equal(notice(c.payload), c.output, `${c.name}: notice framing`);
    }
});

interface EvmCallFile {
    selector: Hex;
    cases: {
        name: string;
        chainId: number;
        from: Address;
        to: Address;
        value: string;
        data: Hex;
        payload: Hex;
    }[];
}

test("eth_call envelopes match the vectors", () => {
    const file = vector<EvmCallFile>("encodings/evmcall.json");

    for (const c of file.cases) {
        const encoded = encodeEvmCall({
            chainId: BigInt(c.chainId),
            from: c.from,
            to: c.to,
            value: BigInt(c.value),
            data: bytes(c.data),
        });
        assert.equal(encoded, c.payload, `${c.name}: payload`);
    }
});

interface OutputsTreeFile {
    cases: { name: string; outputs: Hex[]; leaves: Hex[] }[];
}

test("output leaves are keccak256 of the raw output", () => {
    const file = vector<OutputsTreeFile>("commitments/outputs-tree.json");

    for (const c of file.cases) {
        c.outputs.forEach((output, i) => {
            assert.equal(keccak256(output), c.leaves[i], `${c.name}: leaf ${i}`);
        });
    }
});
