// The application API (EVM-COMPAT §10): a contract is an address, an ABI,
// and callbacks — the library turns that into a router Handler, so every
// application gets the standard's dispatch, outcomes and encodings without
// writing any of it.
//
// Dispatch is ABI-driven: calldata decodes against the registered ABI, the
// function's stateMutability decides which side may run it (transactions
// for nonpayable/payable, views for view/pure), and view results are
// ABI-encoded from plain return values.
//
// Errors follow the EVM's own rule — an exception inside a contract is a
// revert — and an application can never do worse than that: nothing thrown
// from a callback rejects the input (EVM-COMPAT §5). A callback that has
// already written state the journal does not cover, i.e. its own RAM,
// throws `Fail` instead of `Revert`: same error report to the caller, but
// the ledger and the outputs stand rather than being rolled back under it.
//
// Besides the two ABI-driven entries there is a third, callerless one:
// `onBlock`, the per-block tick, for logic that must advance whether or not
// anyone sent a transaction. It has no selector and no sender, so it is not
// an ABI entry — see the README, "The per-block tick".

import {
    type AdvanceOutcome,
    type BlockContext,
    type CallContext,
    errorRevert,
    type L1Attributes,
    type OutputsSink,
    type TickContext,
    type TxContext,
    tryDecodeCalldata,
    type ViewOutcome,
} from "@op-cartesi/evm";
import {
    type Abi,
    type AbiStateMutability,
    type Address,
    type ContractFunctionArgs,
    type ContractFunctionName,
    type ContractFunctionReturnType,
    encodeFunctionResult,
    type Hex,
} from "viem";
import type { Ledger } from "./ledger.ts";
import type { Handler, TickOutcome } from "./types.ts";

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

/** Thrown by a callback that has already mutated its own RAM and therefore
 * cannot honestly revert: the ledger writes, the value transfer and the
 * outputs are all kept, and the error data is reported under TAG_FAIL.
 *
 * The rule for choosing (EVM-COMPAT §5): a callback that has written
 * nothing of its own throws `Revert`; one that has throws `Fail`. Ordering
 * the callback so every fallible step precedes the first RAM write keeps it
 * in `Revert` territory, which is the safer half. */
export class Fail extends Error {
    readonly data: Hex;

    constructor(reason: string | { data: Hex }) {
        super(typeof reason === "string" ? reason : "fail");
        this.name = "Fail";
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

/** What a tick callback receives. No `tx`: a tick has no sender, no value
 * and no calldata, because nobody called it. `block` is this block's header
 * context and `l1` the attributes the same input just delivered. */
export interface BlockEnv {
    block: BlockContext;
    l1: L1Attributes;
    ledger: Ledger;
    out: OutputsSink;
}

type Mutable = "nonpayable" | "payable";
type Readonly_ = "pure" | "view";

// Callbacks take the ABI function's parameters as their own — a variadic
// tuple splices the decoded argument types in front of the environment, so
// `transfer(address to, uint256 value)` is written
// `transfer: (to, value, env) => …` with both fully typed. A callback that
// wants no trailing environment simply declares fewer parameters.
//
// ArgsOf pins viem's argument type to something provably spreadable: for a
// concrete abi it IS the argument tuple; only an abi TypeScript cannot
// narrow falls back to an untyped list.
type ArgsOf<
    abi extends Abi,
    mutability extends AbiStateMutability,
    name extends ContractFunctionName<abi, mutability>,
> =
    ContractFunctionArgs<abi, mutability, name> extends readonly unknown[]
        ? ContractFunctionArgs<abi, mutability, name>
        : readonly unknown[];

export type TransactionCallbacks<abi extends Abi> = {
    [name in ContractFunctionName<abi, Mutable>]?: (
        ...params: [...args: [...ArgsOf<abi, Mutable, name>], env: TransactionEnv]
    ) => Promise<void> | void;
};

export type ViewCallbacks<abi extends Abi> = {
    [name in ContractFunctionName<abi, Readonly_>]?: (
        ...params: [...args: [...ArgsOf<abi, Readonly_, name>], env: ViewEnv]
    ) =>
        | Promise<ContractFunctionReturnType<abi, Readonly_, name>>
        | ContractFunctionReturnType<abi, Readonly_, name>;
};

export interface ContractSpec<abi extends Abi> {
    /** The routed address — the application's choice, outside the reserved
     * namespaces (Application.contract enforces that). */
    address: Address;
    /** Standard JSON ABI (viem's Abi). Recorded verbatim in the ABI drive. */
    abi: abi;
    payable?: boolean;
    transactions?: TransactionCallbacks<abi>;
    views?: ViewCallbacks<abi>;
    /** The per-block tick: run once at the head of every block, whether or
     * not the block carries transactions — the hook for logic that advances
     * on its own clock rather than on a caller's.
     *
     * It is not an ABI entry, and deliberately: a tick has no selector and
     * no caller, so recording one would misdescribe the contract's interface
     * surface to the ABI drive and to the tooling that reads it.
     *
     * Same failure rule as a transaction callback (`Revert` before the first
     * private write, `Fail` after), and the same guarantee: nothing thrown
     * here can reject the input. What it cannot be protected from is cost —
     * every block pays for it, on the critical path of block production, so
     * keep it bounded. See the README, "The per-block tick". */
    onBlock?: (env: BlockEnv) => Promise<void> | void;
}

function mutabilityOf(abi: Abi, functionName: string): AbiStateMutability | undefined {
    for (const item of abi) {
        if (item.type === "function" && item.name === functionName) return item.stateMutability;
    }
    return undefined;
}

/** Every exception out of a callback becomes revert — or fail, when the
 * callback said it had already written. Never reject: applications do not
 * get to roll the machine back (EVM-COMPAT §5). */
function failureOutcome(e: unknown): { kind: "revert" | "fail"; data: Hex } {
    if (e instanceof Fail) return { kind: "fail", data: e.data };
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

/** The decoded arguments as a spreadable list — viem types `args` loosely
 * enough that TypeScript cannot prove it iterable for a generic abi. */
function argsOf(decoded: { args?: unknown }): readonly unknown[] {
    return Array.isArray(decoded.args) ? decoded.args : [];
}

/** Builds the router Handler for a contract spec. Application.contract is the
 * normal entry; this is exported for tests and custom wiring. */
export function contractHandler<const abi extends Abi>(spec: ContractSpec<abi>): Handler {
    // Mapped types carry an implicit index signature, so the callback maps
    // widen to unknown-valued records; each callback re-enters typed through
    // the typeof-function guard at its call site.
    const transactions: Record<string, unknown> = spec.transactions ?? {};
    const views: Record<string, unknown> = spec.views ?? {};
    const onBlock = spec.onBlock;

    const handler: Handler = {
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
                await callback(...argsOf(decoded), { tx: ctx, ledger, out });
                return { kind: "accept" };
            } catch (e) {
                return failureOutcome(e);
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
                const result = await callback(...argsOf(decoded), { call, ledger });
                return { kind: "return", data: encodeResult(spec.abi, name, result) };
            } catch (e) {
                // A view has nothing to keep, so Fail and Revert read alike
                // here: the query answers with the error data either way.
                return { kind: "revert", data: failureOutcome(e).data };
            }
        },
    };

    // Left undefined when the contract declares no tick, so the router can
    // skip it without calling into a no-op every block.
    if (onBlock) {
        handler.onBlock = async (
            tick: TickContext,
            out: OutputsSink,
            ledger: Ledger,
        ): Promise<TickOutcome> => {
            try {
                await onBlock({ block: tick.block, l1: tick.l1, ledger, out });
                return { kind: "accept" };
            } catch (e) {
                return failureOutcome(e);
            }
        };
    }
    return handler;
}
