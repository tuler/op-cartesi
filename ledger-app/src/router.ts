// The router (EVM-COMPAT §5): parse → enforce → move value → dispatch on
// `to` → commit one of three outcomes.
//
// Everything a handler does to the ledger runs inside the journal, so REVERT
// can undo the handler and the value transfer while still committing the
// nonce bump and the fee — failed transactions are not free. REJECT is the
// consensus-mandated refusal: the whole input rolls back (in the machine, by
// finishing rejected; in host-side tests, the journal performs the same
// rollback on the bytes). Nothing a handler throws escapes: a crashed
// handler is a deterministic reject, never a halted machine.

import { concat, toHex, type Address, type Hex } from "viem";
import {
    BRIDGE_ADDRESS,
    CONFIG_ADDRESS,
    L1BLOCK_ADDRESS,
    REGISTRY_ADDRESS,
    addrKey,
    sameAddress,
} from "./addresses.ts";
import { encodeEvmLog } from "./events.ts";
import { decodeEvmCall, errorRevert } from "./evmcall.ts";
import { Bridge } from "./handlers/bridge.ts";
import { Config } from "./handlers/config.ts";
import { Erc20Facade } from "./handlers/erc20.ts";
import { L1Block } from "./handlers/l1block.ts";
import { PortalReceiver } from "./handlers/portal.ts";
import { Registry } from "./handlers/registry.ts";
import { AccountsDriveError, InsufficientFunds, type Ledger } from "./ledger.ts";
import { parseInput, recoverSender } from "./tx.ts";
import {
    TAG_APP,
    TAG_RETURN,
    TAG_REVERT,
    type AdvanceOutcome,
    type BlockContext,
    type Emission,
    type Handler,
    type OutputsSink,
    type TxContext,
} from "./types.ts";

export interface RouterConfig {
    chainId: bigint;
    owner: Address;
}

export interface AdvanceResult {
    accept: boolean;
    /** What actually gets emitted, in order, after the outcome was applied. */
    emissions: Emission[];
    /** For observability and tests; not part of the wire. */
    outcome: "accept" | "revert" | "reject" | "opaque";
    reason?: string;
}

export interface InspectResult {
    accept: boolean;
    reports: Hex[];
}

class BufferSink implements OutputsSink {
    emissions: Emission[] = [];

    notice(payload: Hex): void {
        this.emissions.push({ kind: "notice", payload });
    }

    voucher(v: { destination: Address; value?: bigint; payload?: Hex }): void {
        this.emissions.push({ kind: "voucher", destination: v.destination, value: v.value ?? 0n, payload: v.payload ?? "0x" });
    }

    report(payload: Hex): void {
        this.emissions.push({ kind: "report", payload: concat([toHex(new Uint8Array([TAG_APP])), payload]) });
    }

    log(emitter: Address, topics: Hex[], data: Hex): void {
        this.notice(encodeEvmLog(emitter, topics, data));
    }

    /** Reports only — what survives a revert. */
    reportsOnly(): Emission[] {
        return this.emissions.filter((e) => e.kind === "report");
    }
}

function tagged(tag: number, data: Hex): Hex {
    return concat([toHex(new Uint8Array([tag])), data]);
}

export class Router {
    private manifest = new Map<Address, Handler>();
    private erc20 = new Erc20Facade();
    private portalReceiver = new PortalReceiver();
    readonly l1Block = new L1Block();

    constructor(
        readonly ledger: Ledger,
        readonly cfg: RouterConfig,
    ) {
        const registry = new Registry();
        this.manifest.set(addrKey(REGISTRY_ADDRESS), registry);
        this.manifest.set(addrKey(BRIDGE_ADDRESS), new Bridge());
        this.manifest.set(addrKey(CONFIG_ADDRESS), new Config(cfg.owner, CONFIG_ADDRESS));
        this.manifest.set(addrKey(L1BLOCK_ADDRESS), this.l1Block);
        registry.lookup = (addr) => {
            const h = this.resolve(addr, null);
            return h ? { payable: h.payable ?? false } : undefined;
        };
        registry.list = () => [...this.manifest.keys()];
    }

    /** Manifest first, then the portal receiver, then the token façades.
     *
     * The portal receiver resolves two ways: at the envelope's appContract
     * address, and — for deposits — by a *registered portal sender*,
     * whatever the `to`. The sender is the authentication anyway
     * (EVM-COMPAT §9), and sender-routing frees the chain from having to
     * know the application contract address at genesis: on the devnet the
     * portals and the app contract are deployed after the chain starts, and
     * the owner's registration input is what makes them real. */
    private resolve(to: Address, block: BlockContext | null, deposit?: { sender: Address }): Handler | undefined {
        const fixed = this.manifest.get(addrKey(to));
        if (fixed) return fixed;
        if (block && sameAddress(to, block.appContract)) return this.portalReceiver;
        if (deposit && this.ledger.portalKind(deposit.sender) !== undefined) return this.portalReceiver;
        if (this.ledger.tokenByL2Address(to)) return this.erc20;
        return undefined;
    }

    // -------------------------------------------------------------- advance

    async advance(block: BlockContext, raw: Uint8Array): Promise<AdvanceResult> {
        const parsed = parseInput(raw);

        if (parsed.kind === "opaque") {
            // Record-and-accept, the standing doctrine for non-transactions.
            return {
                accept: true,
                outcome: "opaque",
                emissions: [{ kind: "report", payload: tagged(TAG_APP, toHex(raw)) }],
            };
        }
        if (parsed.kind === "refused") {
            return { accept: false, outcome: "reject", reason: parsed.reason, emissions: [] };
        }

        if (parsed.kind === "deposit") {
            return this.advanceDeposit(block, parsed);
        }
        return this.advanceSigned(block, parsed);
    }

    private async advanceDeposit(
        block: BlockContext,
        d: Extract<ReturnType<typeof parseInput>, { kind: "deposit" }>,
    ): Promise<AdvanceResult> {
        if (!d.to) {
            // A deposit with no to (a create) has nothing to route; the mint
            // still credits its sender, per OP's guarantee.
            try {
                await this.ledger.creditEther(d.from, d.mint);
                this.ledger.journal.commit();
                return { accept: true, outcome: "accept", emissions: [] };
            } catch {
                await this.rollbackAll();
                return { accept: false, outcome: "reject", reason: "mint refused", emissions: [] };
            }
        }
        const ctx: TxContext = {
            block,
            sender: d.from,
            to: d.to,
            value: d.value,
            data: d.data,
            isDeposit: true,
            mint: d.mint,
        };
        try {
            await this.ledger.creditEther(d.from, d.mint);
        } catch {
            await this.rollbackAll();
            return { accept: false, outcome: "reject", reason: "mint refused (drive full)", emissions: [] };
        }
        // The mint survives a revert (OP's deposit guarantee); everything
        // after this mark does not.
        const afterMint = this.ledger.journal.mark();
        return this.execute(ctx, afterMint, null);
    }

    private async advanceSigned(
        block: BlockContext,
        tx: Extract<ReturnType<typeof parseInput>, { kind: "signed" }>,
    ): Promise<AdvanceResult> {
        const reject = (reason: string): AdvanceResult => ({ accept: false, outcome: "reject", reason, emissions: [] });
        if (!tx.to) return reject("contract creation is not supported");
        // The chain id pin (EVM-COMPAT §4): EIP-155 signatures must name this
        // chain. Pre-155 legacy transactions carry none and pass, as they do
        // on Ethereum.
        if (tx.chainId !== undefined && tx.chainId !== this.cfg.chainId) {
            return reject(`transaction signed for chain ${tx.chainId}`);
        }
        const sender = await recoverSender(tx);
        if (!sender) return reject("signature does not verify");
        const account = await this.ledger.account(sender);
        if (tx.nonce !== account.nonce) {
            return reject(`nonce ${tx.nonce}, account nonce ${account.nonce}`);
        }
        // The fee this transaction is admitted (and later charged) under —
        // snapshotted before dispatch, so a handler changing the schedule
        // never bills the transaction that carried the change.
        const fee = this.ledger.fee;
        if (account.balance < fee + tx.value) {
            return reject("balance does not cover fee and value");
        }
        const ctx: TxContext = {
            block,
            sender,
            to: tx.to,
            value: tx.value,
            data: tx.data,
            isDeposit: false,
            mint: 0n,
        };
        return this.execute(ctx, this.ledger.journal.mark(), { address: sender, fee });
    }

    /** Value transfer + dispatch + outcome, from a journal mark that revert
     * rolls back to. `chargeable` is the signer to bump-and-charge on accept
     * and revert (null for deposits, which are enforcement-exempt). */
    private async execute(
        ctx: TxContext,
        mark: number,
        chargeable: { address: Address; fee: bigint } | null,
    ): Promise<AdvanceResult> {
        const sink = new BufferSink();
        let outcome: AdvanceOutcome;
        try {
            await this.ledger.debitEther(ctx.sender, ctx.value);
            await this.ledger.creditEther(ctx.to, ctx.value);
            const handler = this.resolve(ctx.to, ctx.block, ctx.isDeposit ? { sender: ctx.sender } : undefined);
            if (!handler) {
                outcome = { kind: "accept" }; // plain transfer; calldata ignored
            } else if (ctx.value > 0n && !handler.payable) {
                outcome = { kind: "revert", data: errorRevert("not payable") };
            } else if (!handler.advance) {
                outcome = { kind: "revert", data: errorRevert("not a transaction target") };
            } else {
                outcome = await handler.advance(ctx, sink, this.ledger);
            }
        } catch (e) {
            outcome = { kind: "reject", reason: describe(e) };
        }

        switch (outcome.kind) {
            case "accept": {
                if (chargeable) {
                    try {
                        await this.ledger.bumpNonceAndCharge(chargeable.address, chargeable.fee);
                    } catch (e) {
                        await this.rollbackAll();
                        return { accept: false, outcome: "reject", reason: describe(e), emissions: [] };
                    }
                }
                this.ledger.journal.commit();
                return { accept: true, outcome: "accept", emissions: sink.emissions };
            }
            case "revert": {
                await this.ledger.journal.rollbackTo(mark);
                await this.ledger.afterRollback();
                if (chargeable) {
                    try {
                        await this.ledger.bumpNonceAndCharge(chargeable.address, chargeable.fee);
                    } catch (e) {
                        await this.rollbackAll();
                        return { accept: false, outcome: "reject", reason: describe(e), emissions: [] };
                    }
                }
                this.ledger.journal.commit();
                // Outputs are dropped — nothing provable came of this input —
                // but diagnostic reports survive, plus the revert data.
                const emissions: Emission[] = [
                    ...sink.reportsOnly(),
                    { kind: "report", payload: tagged(TAG_REVERT, outcome.data) },
                ];
                return { accept: true, outcome: "revert", emissions };
            }
            case "reject": {
                await this.rollbackAll();
                return { accept: false, outcome: "reject", reason: outcome.reason, emissions: [] };
            }
        }
    }

    private async rollbackAll(): Promise<void> {
        // In the machine, finishing rejected rolls everything back anyway;
        // doing it here too keeps host-side stores byte-faithful.
        await this.ledger.journal.rollbackAll();
        await this.ledger.afterRollback();
    }

    // -------------------------------------------------------------- inspect

    async inspect(payload: Uint8Array): Promise<InspectResult> {
        const call = decodeEvmCall(payload);
        if (!call) {
            return { accept: false, reports: [tagged(TAG_REVERT, errorRevert("not an EvmCall query"))] };
        }
        if (!call.simulate) {
            const handler = this.resolve(call.to, null);
            if (!handler) {
                // eth_call to an EOA returns empty, not an error.
                return { accept: true, reports: [tagged(TAG_RETURN, "0x")] };
            }
            if (!handler.view) {
                return { accept: false, reports: [tagged(TAG_REVERT, errorRevert("not a view target"))] };
            }
            let outcome;
            try {
                outcome = await handler.view(
                    { sender: call.from, to: call.to, value: call.value, data: call.data },
                    this.ledger,
                );
            } catch (e) {
                return { accept: false, reports: [tagged(TAG_REVERT, errorRevert(describe(e)))] };
            }
            return outcome.kind === "return"
                ? { accept: true, reports: [tagged(TAG_RETURN, outcome.data)] }
                : { accept: false, reports: [tagged(TAG_REVERT, outcome.data)] };
        }

        // EvmSimulate: the advance path minus signature, nonce and fee, then
        // rolled back — deterministic whether or not the caller's machine
        // fork is discarded. The host measures the cycles.
        const mark = this.ledger.journal.mark();
        const ctx: TxContext = {
            block: this.lastBlock ?? emptyBlock(this.cfg.chainId),
            sender: call.from,
            to: call.to,
            value: call.value,
            data: call.data,
            isDeposit: false,
            mint: 0n,
        };
        const sink = new BufferSink();
        let outcome: AdvanceOutcome;
        try {
            await this.ledger.debitEther(ctx.sender, ctx.value);
            await this.ledger.creditEther(ctx.to, ctx.value);
            const handler = this.resolve(ctx.to, ctx.block);
            if (!handler) outcome = { kind: "accept" };
            else if (ctx.value > 0n && !handler.payable) outcome = { kind: "revert", data: errorRevert("not payable") };
            else if (!handler.advance) outcome = { kind: "revert", data: errorRevert("not a transaction target") };
            else outcome = await handler.advance(ctx, sink, this.ledger);
        } catch (e) {
            outcome = { kind: "revert", data: errorRevert(describe(e)) };
        }
        await this.ledger.journal.rollbackTo(mark);
        await this.ledger.afterRollback();
        return outcome.kind === "revert"
            ? { accept: false, reports: [tagged(TAG_REVERT, outcome.data)] }
            : { accept: true, reports: [tagged(TAG_RETURN, "0x")] };
    }

    /** The parked machine's last-seen context, which for the machine at
     * block N is block N's (EVM-COMPAT §7). */
    lastBlock: BlockContext | null = null;
}

function emptyBlock(chainId: bigint): BlockContext {
    return {
        chainId,
        appContract: "0x0000000000000000000000000000000000000000",
        blockNumber: 0n,
        timestamp: 0n,
        prevRandao: 0n,
        inputIndex: 0n,
    };
}

function describe(e: unknown): string {
    if (e instanceof AccountsDriveError) return `drive: ${e.kind}`;
    if (e instanceof InsufficientFunds) return e.message;
    if (e instanceof Error) return e.message;
    return String(e);
}
