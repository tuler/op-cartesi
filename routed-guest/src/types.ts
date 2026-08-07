// The runtime's handler contract, plus a re-export of the wire vocabulary —
// so runtime-internal modules say `../types.ts` for both.

import type { AdvanceOutcome, CallContext, OutputsSink, TxContext, ViewOutcome } from "@cartesi/evm-compat";
import type { Ledger } from "./ledger.ts";

export type {
    AdvanceOutcome,
    BlockContext,
    CallContext,
    Emission,
    OutputsSink,
    TxContext,
    ViewOutcome,
} from "@cartesi/evm-compat";
export { TAG_APP, TAG_FAIL, TAG_RETURN, TAG_REVERT } from "@cartesi/evm-compat";

/** A handler: native guest code at an address. Either entry is optional —
 * an advance-only handler reverts calls, a view-only handler reverts
 * transactions. */
export interface Handler {
    /** Refuse value unless declared payable (Ethereum's rule). */
    payable?: boolean;
    advance?(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome>;
    view?(call: CallContext, ledger: Ledger): Promise<ViewOutcome>;
}
