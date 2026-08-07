// The application API (EVM-COMPAT §10): a contract is an address, an ABI,
// and callbacks — the library turns that into a router Handler, so every
// application gets the standard's dispatch, outcomes and encodings without
// writing any of it.
//
// Dispatch is ABI-driven: calldata decodes against the registered ABI, the
// function's stateMutability decides which side may run it (transactions
// for nonpayable/payable, views for view/pure), and view results are
// ABI-encoded from plain return values. Errors follow the EVM's own rule —
// an exception inside a contract is a revert, not a consensus reject — with
// two deliberate exceptions: an explicit `Revert` carries chosen revert
// data, and a drive-format refusal (AccountsDriveError) escalates to the
// router, which rejects, as the spec mandates for resource exhaustion.

import {
    errorRevert,
    tryDecodeCalldata,
    type AdvanceOutcome,
    type CallContext,
    type OutputsSink,
    type TxContext,
    type ViewOutcome,
} from "@cartesi/evm-compat";
import {
    encodeFunctionResult,
    type Abi,
    type AbiStateMutability,
    type Address,
    type ContractFunctionArgs,
    type ContractFunctionName,
    type ContractFunctionReturnType,
    type Hex,
} from "viem";
import { AccountsDriveError, type Ledger } from "./ledger.ts";
import type { Handler } from "./types.ts";

/** Thrown by a callback to revert with chosen data (or an Error(string)
 * reason). Any other exception reverts with the exception's message. */
export class Revert extends Error {
    readonly data: Hex;

    constructor(reason: string | { data: Hex }) {
        super(typeof reason === "string" ? reason : "revert");
        this.name = "Revert";
        this.data = typeof reason === "string" ? errorRevert(reason) : reason.data;
    }
}

export interface TransactionEnv {
    tx: TxContext;
    ledger: Ledger;
    out: OutputsSink;
}

export interface ViewEnv {
    call: CallContext;
    ledger: Ledger;
}

type Mutable = "nonpayable" | "payable";
type Readonly_ = "pure" | "view";

export type TransactionCallbacks<abi extends Abi> = {
    [name in ContractFunctionName<abi, Mutable>]?: (
        args: ContractFunctionArgs<abi, Mutable, name>,
        env: TransactionEnv,
    ) => Promise<void> | void;
};

export type ViewCallbacks<abi extends Abi> = {
    [name in ContractFunctionName<abi, Readonly_>]?: (
        args: ContractFunctionArgs<abi, Readonly_, name>,
        env: ViewEnv,
    ) =>
        | Promise<ContractFunctionReturnType<abi, Readonly_, name>>
        | ContractFunctionReturnType<abi, Readonly_, name>;
};

export interface ContractSpec<abi extends Abi> {
    /** The routed address — the application's choice, outside the reserved
     * namespaces (Guest.contract enforces that). */
    address: Address;
    /** Standard JSON ABI (viem's Abi). Recorded verbatim in the ABI drive. */
    abi: abi;
    payable?: boolean;
    transactions?: TransactionCallbacks<abi>;
    views?: ViewCallbacks<abi>;
}

function mutabilityOf(abi: Abi, functionName: string): AbiStateMutability | undefined {
    for (const item of abi) {
        if (item.type === "function" && item.name === functionName) return item.stateMutability;
    }
    return undefined;
}

function revertOutcome(e: unknown): { kind: "revert"; data: Hex } {
    if (e instanceof Revert) return { kind: "revert", data: e.data };
    if (e instanceof Error) return { kind: "revert", data: errorRevert(e.message) };
    return { kind: "revert", data: errorRevert(String(e)) };
}

/** Widened boundary around viem's encoder: by dispatch time the function
 * name is a runtime string and the result an untyped value — the per-name
 * typing already happened at the callback's signature. */
function encodeResult(abi: Abi, functionName: string, result: unknown): Hex {
    return encodeFunctionResult({ abi, functionName, result });
}

/** Builds the router Handler for a contract spec. Guest.contract is the
 * normal entry; this is exported for tests and custom wiring. */
export function contractHandler<const abi extends Abi>(spec: ContractSpec<abi>): Handler {
    // Mapped types carry an implicit index signature, so the callback maps
    // widen to unknown-valued records; each callback re-enters typed through
    // the typeof-function guard at its call site.
    const transactions: Record<string, unknown> = spec.transactions ?? {};
    const views: Record<string, unknown> = spec.views ?? {};

    return {
        payable: spec.payable ?? false,

        async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
            const decoded = tryDecodeCalldata(spec.abi, ctx.data);
            if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
            const name = decoded.functionName;
            const mutability = mutabilityOf(spec.abi, name);
            if (mutability === "view" || mutability === "pure") {
                return { kind: "revert", data: errorRevert(`${name} is not a transaction`) };
            }
            const callback = transactions[name];
            if (typeof callback !== "function") {
                return { kind: "revert", data: errorRevert(`${name} is not implemented`) };
            }
            try {
                await callback(decoded.args ?? [], { tx: ctx, ledger, out });
                return { kind: "accept" };
            } catch (e) {
                if (e instanceof AccountsDriveError) throw e; // resource exhaustion → reject (spec §8)
                return revertOutcome(e);
            }
        },

        async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
            const decoded = tryDecodeCalldata(spec.abi, call.data);
            if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
            const name = decoded.functionName;
            const mutability = mutabilityOf(spec.abi, name);
            if (mutability !== "view" && mutability !== "pure") {
                return { kind: "revert", data: errorRevert(`${name} is not a view`) };
            }
            const callback = views[name];
            if (typeof callback !== "function") {
                return { kind: "revert", data: errorRevert(`${name} is not implemented`) };
            }
            try {
                const result = await callback(decoded.args ?? [], { call, ledger });
                return { kind: "return", data: encodeResult(spec.abi, name, result) };
            } catch (e) {
                return revertOutcome(e);
            }
        },
    };
}
