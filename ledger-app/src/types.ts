// Shared types of the routed guest (docs/EVM-COMPAT.md).
//
// The router owns transaction-level enforcement and dispatches every input
// and every inspect query on its `to` address to a handler. Handlers are
// native code registered in a manifest fixed at snapshot build; this file is
// the vocabulary between the router and its handlers.

import type { Address, Hex } from "viem";
import type { Ledger } from "./ledger.ts";

/** L2 block context, from the EvmAdvance envelope (transport framing only —
 * EVM-COMPAT §4: nothing here is an authority on the sender). */
export interface BlockContext {
    chainId: bigint;
    appContract: Address;
    blockNumber: bigint;
    timestamp: bigint;
    prevRandao: bigint;
    inputIndex: bigint;
}

/** The context a handler's advance entry receives: Ethereum call semantics
 * over a parsed, enforced transaction. */
export interface TxContext {
    block: BlockContext;
    /** Recovered signer for user transactions; the deposit's own (aliased)
     * `from` for deposits. Never the envelope's msgSender. */
    sender: Address;
    to: Address;
    value: bigint;
    data: Uint8Array;
    /** True when the input is an OP deposit transaction (type 0x7e). */
    isDeposit: boolean;
    /** Deposit mint, 0 otherwise. Credited to `sender` before dispatch and
     * kept even on revert (OP's deposit guarantee). */
    mint: bigint;
}

/** A view call (EvmCall over inspect — EVM-COMPAT §7). */
export interface CallContext {
    sender: Address;
    to: Address;
    value: bigint;
    data: Uint8Array;
}

/** The three-outcome model of EVM-COMPAT §5.
 *
 * - accept: commit everything.
 * - revert: roll back the handler's ledger writes and the value transfer,
 *   keep the nonce bump and the fee, finish the input accepted, drop the
 *   handler's outputs, report the revert data.
 * - reject: consensus-mandated refusal; the whole input rolls back.
 */
export type AdvanceOutcome =
    | { kind: "accept" }
    | { kind: "revert"; data: Hex }
    | { kind: "reject"; reason: string };

export type ViewOutcome =
    | { kind: "return"; data: Hex }
    | { kind: "revert"; data: Hex };

/** Buffered emissions: the router collects these during dispatch and flushes
 * only what the outcome allows (outputs on accept; reports always). */
export type Emission =
    | { kind: "notice"; payload: Hex }
    | { kind: "voucher"; destination: Address; value: bigint; payload: Hex }
    | { kind: "report"; payload: Hex };

/** What handlers emit through. `log` is sugar for an EvmLog notice
 * (EVM-COMPAT §8): provable, and decoded into a real receipt log by the
 * shim. */
export interface OutputsSink {
    notice(payload: Hex): void;
    voucher(v: { destination: Address; value?: bigint; payload?: Hex }): void;
    report(payload: Hex): void;
    log(emitter: Address, topics: Hex[], data: Hex): void;
}

/** A handler: native guest code at an address. Either entry is optional —
 * an advance-only handler reverts calls, a view-only handler reverts
 * transactions. */
export interface Handler {
    /** Refuse value unless declared payable (Ethereum's rule). */
    payable?: boolean;
    advance?(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome>;
    view?(call: CallContext, ledger: Ledger): Promise<ViewOutcome>;
}

/** Report framing (prototype convention, one byte before every report the
 * router emits, so the shim can tell return data and revert data from
 * app-level diagnostics):
 *   0x00 app report (handler passthrough / record-and-accept echo)
 *   0x01 return data (inspect)
 *   0x02 revert data (inspect reject, or advance revert)
 */
export const TAG_APP = 0x00;
export const TAG_RETURN = 0x01;
export const TAG_REVERT = 0x02;
