// Shared types of the routed guest standard (docs/EVM-COMPAT.md).
//
// This is the wire-level vocabulary — contexts, outcomes, emissions, report
// tags — shared by the guest runtime (@op-cartesi/app) and by host
// tooling. The Handler interface itself lives with the runtime, since it
// names the ledger.

import type { Address, Hex } from "viem";

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

/** The outcome model of EVM-COMPAT §5.
 *
 * The three a handler may return:
 *
 * - accept: commit everything.
 * - revert: "nothing of mine changed" — roll back the handler's ledger
 *   writes and the value transfer, keep the nonce bump and the fee, finish
 *   the input accepted, drop the handler's outputs, report the revert data.
 * - fail: "I already changed something" — commit exactly as accept does,
 *   outputs included, and report the error data. For a handler that has
 *   mutated state the journal does not cover (its own RAM), which revert
 *   could not undo.
 *
 * Plus the one only the router produces:
 *
 * - reject: the input finishes rejected and the machine rolls back
 *   wholesale. Reserved for enforcement failure, for deposits (which have
 *   no charge to keep), and for the case where the nonce bump and fee
 *   themselves cannot be recorded. A handler cannot ask for it.
 */
export type AdvanceOutcome =
    | { kind: "accept" }
    | { kind: "revert"; data: Hex }
    | { kind: "fail"; data: Hex }
    | { kind: "reject"; reason: string };

export type ViewOutcome = { kind: "return"; data: Hex } | { kind: "revert"; data: Hex };

/** Buffered emissions: the router collects these during dispatch and flushes
 * only what the outcome allows (outputs on accept; reports always). */
export type Emission =
    | { kind: "notice"; payload: Hex }
    | { kind: "voucher"; destination: Address; value: bigint; payload: Hex }
    | { kind: "report"; payload: Hex };

/** What handlers emit through. `log` is sugar for an EvmLog notice
 * (EVM-COMPAT §8): provable, and decoded into a real receipt log by the
 * shim. `withdrawal` is sugar for a Withdrawal notice (withdrawal.ts): an
 * OP Stack L2→L1 message, finalized on L1 by OptimismPortal against the
 * block's withdrawal trie. The sink assigns its nonce from the input index,
 * so the caller names only the message. */
export interface OutputsSink {
    notice(payload: Hex): void;
    voucher(v: { destination: Address; value?: bigint; payload?: Hex }): void;
    report(payload: Hex): void;
    log(emitter: Address, topics: Hex[], data: Hex): void;
    withdrawal(w: {
        sender: Address;
        target: Address;
        value?: bigint;
        gasLimit?: bigint;
        data?: Hex;
    }): void;
}

/** Report framing (prototype convention, one byte before every report the
 * router emits, so the shim can tell return data and revert data from
 * app-level diagnostics):
 *   0x00 app report (handler passthrough / record-and-accept echo)
 *   0x01 return data (inspect)
 *   0x02 revert data (inspect reject, or advance revert) — no state changed
 *   0x03 failure data (advance fail) — state changed and was kept
 *
 * 0x02 and 0x03 are distinct because they mean opposite things to a caller
 * reading a receipt: a revert is safe to treat as "nothing happened", a
 * failure is not.
 */
export const TAG_APP = 0x00;
export const TAG_RETURN = 0x01;
export const TAG_REVERT = 0x02;
export const TAG_FAIL = 0x03;
