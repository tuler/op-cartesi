// The runtime's handler contract, plus a re-export of the wire vocabulary —
// so runtime-internal modules say `../types.ts` for both.

import type {
    AdvanceOutcome,
    CallContext,
    OutputsSink,
    TickContext,
    TxContext,
    ViewOutcome,
} from "@op-cartesi/evm";
import type { Ledger } from "./ledger.ts";

export type {
    AdvanceOutcome,
    BlockContext,
    CallContext,
    Emission,
    L1Attributes,
    OutputsSink,
    TickContext,
    TxContext,
    ViewOutcome,
} from "@op-cartesi/evm";
export { TAG_APP, TAG_FAIL, TAG_RETURN, TAG_REVERT } from "@op-cartesi/evm";

/** What a tick may return. REJECT is absent by construction: the input a
 * tick rides on is the block's mandatory attributes deposit, and rejecting
 * it would discard the chain's own L1 context along with the tick. */
export type TickOutcome = Exclude<AdvanceOutcome, { kind: "reject" }>;

/** A handler: native guest code at an address. Every entry is optional —
 * an advance-only handler reverts calls, a view-only handler reverts
 * transactions, and a handler without `onBlock` simply does not tick. */
export interface Handler {
    /** Refuse value unless declared payable (Ethereum's rule). */
    payable?: boolean;
    advance?(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome>;
    view?(call: CallContext, ledger: Ledger): Promise<ViewOutcome>;
    /** The per-block tick: run once per block, at the head of it, with no
     * caller. The router runs it after the block's L1 attributes are
     * stored, isolated per handler — see Router.runTicks. */
    onBlock?(tick: TickContext, out: OutputsSink, ledger: Ledger): Promise<TickOutcome>;
}
