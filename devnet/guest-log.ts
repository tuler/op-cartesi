#!/usr/bin/env bun
// Follows the chain and prints what the guest program inside the Cartesi
// Machine had to say about every transaction — its reports.
//
//   bun devnet/guest-log.ts [rpc]
//
// The `machine` pane shows the emulator's console: Linux booting, and
// anything the guest writes to stdout. That is the machine talking. This is
// the guest talking *about the chain*: reports are the machine's per-input
// diagnostics, explicitly not provable and therefore never in a receipt, and
// for a rejected input they are the only account of why it failed
// (engineapi/cartesi.go). They are read back over cartesi_getTransactionEmissions,
// which means this works against any node — it is pointed at the sequencer's
// RPC here, but the verifier's serves the same thing.
//
// Reports carry the router's one-byte tag (EVM-COMPAT §8, evm-compat/js
// types.ts): app diagnostics, eth_call return data, or revert data.

import { TAG_APP, TAG_RETURN, TAG_REVERT } from "@cartesi/evm-compat";
import { decodeErrorResult, formatEther, type Hex, hexToBytes, size, slice } from "viem";
import { config, l2Public } from "./lib/env.ts";

const POLL_MS = Number(process.env.GUEST_LOG_POLL_MS ?? 500);
// Enough of a report to read; the rest is a scroll away in the RPC.
const MAX_REPORT_CHARS = 400;

// Any node serves the same emissions, so the RPC is an argument: the
// sequencer's by default, the verifier's if you want to watch it agree.
const rpc = process.argv[2] ?? config.l2Rpc;
const client = l2Public(rpc);

const TAGS: Record<number, string> = {
    [TAG_APP]: "report",
    [TAG_RETURN]: "return",
    [TAG_REVERT]: "revert",
};

/** Error(string) and Panic(uint256), so a revert report reads as its reason
 * rather than as its selector. */
const revertAbi = [
    { type: "error", name: "Error", inputs: [{ type: "string" }] },
    { type: "error", name: "Panic", inputs: [{ type: "uint256" }] },
] as const;

/** Printable UTF-8 comes out as text, anything else as hex: the guest writes
 * both, and a JSON diagnostic is unreadable as 0x… */
function readable(payload: Hex): string {
    if (size(payload) === 0) return "(empty)";
    const bytes = hexToBytes(payload);
    let text: string;
    try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    } catch {
        return truncate(payload);
    }
    // Control bytes mean this decoded as text by accident: show the bytes.
    // Whitespace is text — tab, newline, vertical tab, form feed, carriage
    // return — and nothing else below 0x20 is.
    const isText = (c: number) => c > 0x1f || (c >= 0x09 && c <= 0x0d);
    for (const ch of text) if (!isText(ch.codePointAt(0)!)) return truncate(payload);
    return truncate(text.replace(/\n+$/, "").replace(/\n/g, "\n           "));
}

function truncate(s: string): string {
    return s.length > MAX_REPORT_CHARS ? `${s.slice(0, MAX_REPORT_CHARS)}… (${s.length} chars)` : s;
}

function formatReport(report: Hex): string {
    if (size(report) === 0) return "  (empty report)";
    const tag = hexToBytes(slice(report, 0, 1))[0]!;
    const label = TAGS[tag];
    // An untagged report is a guest that does not follow the router's
    // convention, which is legal — it is shown whole rather than dissected.
    if (label === undefined) return `  report   ${readable(report)}`;
    const payload = size(report) > 1 ? slice(report, 1) : "0x";
    if (tag === TAG_REVERT && size(payload) >= 4) {
        try {
            const decoded = decodeErrorResult({ abi: revertAbi, data: payload });
            return `  revert   ${decoded.errorName}(${decoded.args?.join(", ") ?? ""})`;
        } catch {
            // Not a standard error selector; fall through to the raw payload.
        }
    }
    return `  ${label.padEnd(8)} ${readable(payload)}`;
}

function shortHash(hash: Hex): string {
    return `${hash.slice(0, 10)}…${hash.slice(-6)}`;
}

/** Cycles are the chain's gas: big numbers that only ever need a magnitude. */
function formatCycles(cycles: bigint): string {
    if (cycles >= 1_000_000_000n) return `${(Number(cycles) / 1e9).toFixed(2)}G`;
    if (cycles >= 1_000_000n) return `${(Number(cycles) / 1e6).toFixed(1)}M`;
    if (cycles >= 1_000n) return `${(Number(cycles) / 1e3).toFixed(1)}k`;
    return `${cycles}`;
}

async function printBlock(number: bigint): Promise<void> {
    const block = await client.getBlock({ blockNumber: number, includeTransactions: true });
    const lines: string[] = [];
    let cycles = 0n;
    for (const tx of block.transactions) {
        const emissions = await client.getTransactionEmissions({ hash: tx.hash });
        cycles += BigInt(emissions.cycles);
        const interesting =
            !emissions.accepted || emissions.reports.length > 0 || emissions.outputs.length > 0;
        if (!interesting) continue;
        const verdict = emissions.accepted ? "accepted" : "REJECTED";
        const to = tx.to ? shortHash(tx.to) : "(create)";
        const value = tx.value > 0n ? ` value ${formatEther(tx.value)}` : "";
        lines.push(
            `  ${verdict} ${shortHash(tx.hash)} to ${to}${value}` +
                ` — ${formatCycles(BigInt(emissions.cycles))} cycles`,
        );
        for (const output of emissions.outputs) {
            lines.push(`  output#${BigInt(output.index)} ${readable(output.data)}`);
        }
        for (const report of emissions.reports) lines.push(formatReport(report));
    }
    const txs = `${block.transactions.length} tx`;
    console.log(`block ${number} ${shortHash(block.hash)} ${txs}, ${formatCycles(cycles)} cycles`);
    for (const line of lines) console.log(line);
}

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// The loop outlives the node: an engine that is restarting takes its RPC with
// it, and this pane should pick the chain back up rather than end.
let next: bigint | undefined;
let complained = false;
for (;;) {
    try {
        const head = await client.getBlockNumber({ cacheTime: 0 });
        if (next === undefined) {
            // A restart of this pane alone joins at the head instead of
            // replaying the whole chain.
            next = head;
            console.log(`following the guest from block ${head} on ${rpc}`);
        }
        // A reorg (the safe chain replacing an unsafe one) can move the head
        // backwards, below what has already been printed; follow it back
        // rather than waiting for it to catch up. `next` being exactly one
        // past the head is the ordinary caught-up state, not a reorg.
        if (head + 1n < next) next = head + 1n;
        for (; next <= head; next++) await printBlock(next);
        complained = false;
    } catch (error) {
        if (!complained) {
            console.error(`waiting for the chain: ${(error as Error).message.split("\n")[0]}`);
            complained = true;
        }
        await sleep(2000);
        continue;
    }
    await sleep(POLL_MS);
}
