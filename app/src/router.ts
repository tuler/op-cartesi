// The router (EVM-COMPAT §5): parse → enforce → move value → dispatch on
// `to` → commit one of three outcomes.
//
// Everything a handler does to the ledger runs inside the journal, so REVERT
// can undo the handler and the value transfer while still committing the
// nonce bump and the fee — failed transactions are not free. FAIL commits
// the same as ACCEPT and reports the error: it is what a handler returns
// once it has written state the journal does not cover.
//
// REJECT is the router's alone, and means one thing: the charge cannot be
// recorded. That is either because there is no charge (a deposit — its
// failure mode is unchanged, EVM-COMPAT §5), because the transaction never
// passed enforcement, or because the nonce bump and fee debit themselves
// refused. Nothing a handler does can produce it: a handler that throws
// reverts, and a handler that crashes reverts, so a broken application
// costs its sender a nonce and a fee instead of a free retry.

import {
    addrKey,
    BRIDGE_ADDRESS,
    CONFIG_ADDRESS,
    decodeEvmCall,
    encodeEvmLog,
    encodeWithdrawal,
    errorRevert,
    L1_INFO_DEPOSITOR,
    L1BLOCK_ADDRESS,
    L2_BRIDGE_ADDRESS,
    L2_MESSENGER_ADDRESS,
    MESSAGE_PASSER_ADDRESS,
    messagePassedLog,
    parseInput,
    REGISTRY_ADDRESS,
    recoverSender,
    sameAddress,
    versionedWithdrawalNonce,
    WITHDRAWAL_DEFAULT_GAS_LIMIT,
    withdrawalHash,
} from "@op-cartesi/evm";
import { type Address, concat, type Hex, toHex } from "viem";
import { Bridge } from "./handlers/bridge.ts";
import { Config } from "./handlers/config.ts";
import { Erc20Facade } from "./handlers/erc20.ts";
import { L1Block } from "./handlers/l1block.ts";
import { L2Bridge } from "./handlers/l2bridge.ts";
import { Messenger } from "./handlers/messenger.ts";
import { MessagePasser } from "./handlers/passer.ts";
import { PortalReceiver } from "./handlers/portal.ts";
import { Registry } from "./handlers/registry.ts";
import { AccountsDriveError, InsufficientFunds, type Ledger } from "./ledger.ts";
import {
    type AdvanceOutcome,
    type BlockContext,
    type Emission,
    type Handler,
    type OutputsSink,
    TAG_APP,
    TAG_FAIL,
    TAG_RETURN,
    TAG_REVERT,
    type TickContext,
    type TickOutcome,
    type TxContext,
    type ViewOutcome,
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
    outcome: "accept" | "revert" | "fail" | "reject" | "opaque";
    reason?: string;
}

export interface InspectResult {
    accept: boolean;
    reports: Hex[];
}

class BufferSink implements OutputsSink {
    emissions: Emission[] = [];

    /** The chain-wide input index seeds withdrawal nonces; the per-input
     * ordinal is the sink's own, and rolls back with the buffered emissions
     * when the outcome drops them.
     *
     * One input can run more than one sink — the block's ticks each get
     * theirs, so one tick's revert cannot drop another's outputs — so the
     * ordinal is handed in and read back out, keeping withdrawal nonces
     * unique across every sink the input opens. */
    constructor(
        private readonly inputIndex: bigint,
        private withdrawals = 0n,
    ) {}

    /** The ordinal to hand the next sink of this input. */
    get nextOrdinal(): bigint {
        return this.withdrawals;
    }

    notice(payload: Hex): void {
        this.emissions.push({ kind: "notice", payload });
    }

    voucher(v: { destination: Address; value?: bigint; payload?: Hex }): void {
        this.emissions.push({
            kind: "voucher",
            destination: v.destination,
            value: v.value ?? 0n,
            payload: v.payload ?? "0x",
        });
    }

    report(payload: Hex): void {
        this.emissions.push({
            kind: "report",
            payload: concat([toHex(new Uint8Array([TAG_APP])), payload]),
        });
    }

    log(emitter: Address, topics: Hex[], data: Hex): void {
        this.notice(encodeEvmLog(emitter, topics, data));
    }

    withdrawal(w: {
        sender: Address;
        target: Address;
        value?: bigint;
        gasLimit?: bigint;
        data?: Hex;
    }): void {
        const message = {
            nonce: versionedWithdrawalNonce(this.inputIndex, this.withdrawals++),
            sender: w.sender,
            target: w.target,
            value: w.value ?? 0n,
            gasLimit: w.gasLimit ?? WITHDRAWAL_DEFAULT_GAS_LIMIT,
            data: w.data ?? "0x",
        } as const;
        this.notice(encodeWithdrawal(message));
        // The MessagePassed event OP tooling reads off the receipt — viem's
        // getWithdrawals in particular — attributed to the message passer
        // address whose storage trie the withdrawal is entering.
        const { topics, data } = messagePassedLog({
            ...message,
            withdrawalHash: withdrawalHash(message),
        });
        this.log(MESSAGE_PASSER_ADDRESS, topics, data);
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
    /** The handlers that declared a tick, in registration order — which is
     * the order they run in, every block. */
    private tickers: NonNullable<Handler["onBlock"]>[] = [];
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
        // The standard messaging pair (DESIGN §6): routed always, live only
        // once the owner registers the L1 side — until then both revert.
        const messenger = new Messenger();
        const l2Bridge = new L2Bridge();
        messenger.bridge = l2Bridge;
        this.manifest.set(addrKey(L2_MESSENGER_ADDRESS), messenger);
        this.manifest.set(addrKey(L2_BRIDGE_ADDRESS), l2Bridge);
        // The passer itself: initiateWithdrawal, viem's entry into the
        // withdrawal flow — needs no registration, a withdrawal is a burn.
        this.manifest.set(addrKey(MESSAGE_PASSER_ADDRESS), new MessagePasser());
        registry.lookup = (addr) => {
            const h = this.resolve(addr, null);
            return h ? { payable: h.payable ?? false } : undefined;
        };
        registry.list = () => [...this.manifest.keys()];
    }

    /** Adds a manifest entry — the application API's entry into the router.
     * The manifest is fixed before the first input (EVM-COMPAT §10); this
     * exists for boot-time registration, not dynamic deploy. */
    register(address: Address, handler: Handler): void {
        const key = addrKey(address);
        if (this.manifest.has(key)) {
            throw new Error(`address ${address} is already routed`);
        }
        this.manifest.set(key, handler);
        const onBlock = handler.onBlock;
        if (onBlock) this.tickers.push(onBlock.bind(handler));
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
    private resolve(
        to: Address,
        block: BlockContext | null,
        deposit?: { sender: Address },
    ): Handler | undefined {
        const fixed = this.manifest.get(addrKey(to));
        if (fixed) return fixed;
        if (block && sameAddress(to, block.appContract)) return this.portalReceiver;
        if (deposit && this.ledger.portalKind(deposit.sender) !== undefined)
            return this.portalReceiver;
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
            const res = await this.advanceDeposit(block, parsed);
            if (!this.isAttributesDeposit(parsed, res)) return res;
            // The block's one guaranteed input has landed and its attributes
            // are stored: this is where the block begins for an application.
            return {
                ...res,
                emissions: [...res.emissions, ...(await this.runTicks(block))],
            };
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
            return {
                accept: false,
                outcome: "reject",
                reason: "mint refused (drive full)",
                emissions: [],
            };
        }
        // The mint survives a revert (OP's deposit guarantee); everything
        // after this mark does not.
        const afterMint = this.ledger.journal.mark();
        return this.execute(ctx, afterMint, null);
    }

    /** Is this input the chain's own per-block attributes deposit, applied?
     *
     * Both halves matter. The **sender** is the authority — op-node's
     * canonical depositor, not merely the L1Block address — so a user
     * transaction carrying attributes-shaped calldata cannot mint extra
     * ticks by paying a fee. And the **outcome** must be accept: a malformed
     * attributes input reverts having stored nothing, and a block whose L1
     * context never landed is not one the application should be told about.
     *
     * op-node deposits exactly one of these per block, at its head, so this
     * fires exactly once per block — including a block with no transactions
     * in it, which is the whole point (EVM-COMPAT §2). */
    private isAttributesDeposit(
        d: Extract<ReturnType<typeof parseInput>, { kind: "deposit" }>,
        res: AdvanceResult,
    ): boolean {
        return (
            res.outcome === "accept" &&
            d.to !== undefined &&
            sameAddress(d.to, L1BLOCK_ADDRESS) &&
            sameAddress(d.from, L1_INFO_DEPOSITOR)
        );
    }

    /** Runs the block's ticks, in registration order, each isolated from the
     * others.
     *
     * The isolation is the point. This input is mandatory in a way no other
     * input is: the sequencer aborts the whole block build if a deposit fails
     * hard, so a tick must never take the input, the block's L1 attributes,
     * or another contract's tick down with it. Hence the attributes being
     * stored before we are called, one journal mark and one output sink per
     * tick, and no path from here to REJECT.
     *
     * Within a tick the outcome model is the usual one (EVM-COMPAT §5):
     * accept keeps everything, revert rolls that tick's ledger writes back
     * and drops its outputs while keeping its reports, fail keeps both and
     * reports the error. What is missing is the charge — a tick has no
     * sender, so there is no nonce to bump and no fee to take. Its cycles
     * are billed to the attributes deposit and count against the block gas
     * limit, which is the honest place for them and the reason a tick must
     * stay bounded. */
    private async runTicks(block: BlockContext): Promise<Emission[]> {
        const l1 = this.l1Block.attributes;
        if (!l1 || this.tickers.length === 0) return [];
        const emissions: Emission[] = [];
        // Withdrawal nonces are (inputIndex, ordinal): the ordinal carries
        // across the input's sinks, and a reverted tick's outputs give theirs
        // back with the rest of what was dropped.
        let ordinal = 0n;
        for (const onBlock of this.tickers) {
            // Copies, not the live objects: `l1` is what the L1Block views
            // serve for the rest of the block and `block` is what the parked
            // machine answers eth_call from, and neither is an application's
            // to write. The context a tick gets is read-only by construction
            // everywhere else; here it is read-only in fact.
            const tick: TickContext = { block: { ...block }, l1: { ...l1 } };
            const mark = this.ledger.journal.mark();
            const sink = new BufferSink(block.inputIndex, ordinal);
            let outcome: TickOutcome;
            try {
                outcome = await onBlock(tick, sink, this.ledger);
            } catch (e) {
                // contractHandler already catches for application callbacks;
                // a hand-written handler that throws lands here and is held
                // to the same rule — a tick cannot escape upward.
                outcome = { kind: "revert", data: errorRevert(describe(e)) };
            }
            if (outcome.kind === "revert") {
                await this.ledger.journal.rollbackTo(mark);
                await this.ledger.afterRollback();
                emissions.push(...sink.reportsOnly(), {
                    kind: "report",
                    payload: tagged(TAG_REVERT, outcome.data),
                });
                continue;
            }
            emissions.push(...sink.emissions);
            if (outcome.kind === "fail") {
                emissions.push({ kind: "report", payload: tagged(TAG_FAIL, outcome.data) });
            }
            ordinal = sink.nextOrdinal;
        }
        this.ledger.journal.commit();
        return emissions;
    }

    private async advanceSigned(
        block: BlockContext,
        tx: Extract<ReturnType<typeof parseInput>, { kind: "signed" }>,
    ): Promise<AdvanceResult> {
        const reject = (reason: string): AdvanceResult => ({
            accept: false,
            outcome: "reject",
            reason,
            emissions: [],
        });
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
     * rolls back to. `chargeable` is the signer to bump-and-charge on accept,
     * revert and fail (null for deposits, which are enforcement-exempt).
     *
     * `chargeable` is also what decides whether a thrown failure can revert:
     * with someone to charge, it can, and the sender pays for the attempt.
     * Without — a deposit — there is nothing to keep, so it rejects, which is
     * the deposit semantics EVM-COMPAT §5 leaves unchanged. */
    private async execute(
        ctx: TxContext,
        mark: number,
        chargeable: { address: Address; fee: bigint } | null,
    ): Promise<AdvanceResult> {
        const sink = new BufferSink(ctx.block.inputIndex);
        let outcome: AdvanceOutcome;
        try {
            await this.ledger.debitEther(ctx.sender, ctx.value);
            await this.ledger.creditEther(ctx.to, ctx.value);
            const handler = this.resolve(
                ctx.to,
                ctx.block,
                ctx.isDeposit ? { sender: ctx.sender } : undefined,
            );
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
            // Drive refusals (tableFull, overflow, registryFull) land here
            // along with anything else that escaped. They are a revert when
            // the sender can be charged for the attempt: the write never
            // happened, so rolling back to the mark leaves the drive as it
            // was, and the sender pays rather than retrying for free.
            outcome = chargeable
                ? { kind: "revert", data: errorRevert(describe(e)) }
                : { kind: "reject", reason: describe(e) };
        }

        // The charge is the last thing to move, and the only thing that can
        // still turn an accepted input into a rejected one: if the nonce bump
        // and fee cannot be written, there is no honest way to finish.
        const charge = async (): Promise<AdvanceResult | null> => {
            if (!chargeable) return null;
            try {
                await this.ledger.bumpNonceAndCharge(chargeable.address, chargeable.fee);
                return null;
            } catch (e) {
                await this.rollbackAll();
                return { accept: false, outcome: "reject", reason: describe(e), emissions: [] };
            }
        };

        switch (outcome.kind) {
            case "accept": {
                const refused = await charge();
                if (refused) return refused;
                this.ledger.journal.commit();
                return { accept: true, outcome: "accept", emissions: sink.emissions };
            }
            case "fail": {
                // Committed exactly like accept, outputs included — the
                // handler told us it had already written state the journal
                // cannot undo, so undoing the half it *can* would tear the
                // two apart. The error reaches the caller as a report.
                const refused = await charge();
                if (refused) return refused;
                this.ledger.journal.commit();
                const emissions: Emission[] = [
                    ...sink.emissions,
                    { kind: "report", payload: tagged(TAG_FAIL, outcome.data) },
                ];
                return { accept: true, outcome: "fail", emissions };
            }
            case "revert": {
                await this.ledger.journal.rollbackTo(mark);
                await this.ledger.afterRollback();
                const refused = await charge();
                if (refused) return refused;
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
            return {
                accept: false,
                reports: [tagged(TAG_REVERT, errorRevert("not an EvmCall query"))],
            };
        }
        if (!call.simulate) {
            const handler = this.resolve(call.to, null);
            if (!handler) {
                // eth_call to an EOA returns empty, not an error.
                return { accept: true, reports: [tagged(TAG_RETURN, "0x")] };
            }
            if (!handler.view) {
                return {
                    accept: false,
                    reports: [tagged(TAG_REVERT, errorRevert("not a view target"))],
                };
            }
            let outcome: ViewOutcome;
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
        const sink = new BufferSink(ctx.block.inputIndex);
        let outcome: AdvanceOutcome;
        try {
            await this.ledger.debitEther(ctx.sender, ctx.value);
            await this.ledger.creditEther(ctx.to, ctx.value);
            const handler = this.resolve(ctx.to, ctx.block);
            if (!handler) outcome = { kind: "accept" };
            else if (ctx.value > 0n && !handler.payable)
                outcome = { kind: "revert", data: errorRevert("not payable") };
            else if (!handler.advance)
                outcome = { kind: "revert", data: errorRevert("not a transaction target") };
            else outcome = await handler.advance(ctx, sink, this.ledger);
        } catch (e) {
            outcome = { kind: "revert", data: errorRevert(describe(e)) };
        }
        await this.ledger.journal.rollbackTo(mark);
        await this.ledger.afterRollback();
        // A fail answers with its own tag: the simulation says not only "this
        // would not succeed" but "running it for real would still change
        // state", which a caller weighing whether to send it needs to know.
        if (outcome.kind === "revert")
            return { accept: false, reports: [tagged(TAG_REVERT, outcome.data)] };
        if (outcome.kind === "fail")
            return { accept: false, reports: [tagged(TAG_FAIL, outcome.data)] };
        return { accept: true, reports: [tagged(TAG_RETURN, "0x")] };
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
