// The L2CrossDomainMessenger (0x4200…0007), adopted for real (DESIGN §7g):
// inbound, it authenticates relayMessage deposits from the aliased L1
// messenger and dispatches them; outbound, sendCrossDomainMessage wraps a
// message in relayMessage and emits it as an OP Withdrawal whose sender is
// this predeploy's address — which is exactly what makes
// L1CrossDomainMessenger accept it, since the portal exposes that sender as
// l2Sender() during finalization.
//
// Relay semantics follow OP's: a failed inner call does not reject the
// input — it is rolled back to a journal mark and recorded in
// failedMessages, value left on the messenger's balance, replayable later
// with an ordinary L2 transaction carrying the same arguments. Emission
// discipline inside dispatch: no logs before the last throwing operation,
// because the sink has no rollback — only the ledger does.

import {
    baseGas,
    type CrossDomainMessage,
    decodeVersionedNonce,
    encodeRelayMessage,
    encodeVersionedNonce,
    errorRevert,
    hashCrossDomainMessageV1,
    L2_BRIDGE_ADDRESS,
    L2_MESSENGER_ADDRESS,
    l2CrossDomainMessengerAbi,
    relayedMessageLog,
    sameAddress,
    sentMessageExtension1Log,
    sentMessageLog,
    tryDecodeCalldata,
} from "@op-cartesi/evm";
import { type Address, encodeFunctionResult, type Hex, size } from "viem";
import type { CrossDomainConfig, Ledger } from "../ledger.ts";
import type {
    AdvanceOutcome,
    CallContext,
    Handler,
    OutputsSink,
    TxContext,
    ViewOutcome,
} from "../types.ts";
import type { L2Bridge } from "./l2bridge.ts";

/** OP's MESSAGE_VERSION: every message this chain sends or receives is v1. */
const MESSAGE_VERSION = 1n;

export class Messenger implements Handler {
    payable = true;

    /** The L2 bridge, for dispatching finalizeBridge* — set by the router
     * after both handlers exist. */
    bridge!: L2Bridge;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        const cd = ledger.crossDomain();
        if (!cd) {
            return { kind: "revert", data: errorRevert("no messenger registered") };
        }
        const decoded = tryDecodeCalldata(l2CrossDomainMessengerAbi, ctx.data);
        if (decoded?.functionName !== "relayMessage") {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        const [nonce, sender, target, value, minGasLimit, message] = decoded.args;
        const m: CrossDomainMessage = { nonce, sender, target, value, minGasLimit, message };
        if (decodeVersionedNonce(nonce).version !== MESSAGE_VERSION) {
            // v0 is migrated-legacy-withdrawal machinery; a fresh chain's L1
            // messenger only ever sends v1.
            return { kind: "revert", data: errorRevert("only version 1 messages are supported") };
        }
        const hash = hashCrossDomainMessageV1(m);
        if (ledger.messageStatus(hash) === "successful") {
            return { kind: "revert", data: errorRevert("message has already been relayed") };
        }

        const fromL1 = ctx.isDeposit && sameAddress(ctx.sender, cd.l1MessengerAliased);
        if (fromL1) {
            // First delivery. OP asserts these; an honest L1 messenger cannot
            // violate them.
            if (ctx.value !== value) {
                return { kind: "revert", data: errorRevert("value does not match the message") };
            }
            if (ledger.messageStatus(hash) === "failed") {
                return { kind: "revert", data: errorRevert("message already received") };
            }
        } else {
            // Replay of a recorded failure: anyone may retry, with no value —
            // the original delivery left the value on this handler's balance.
            if (ctx.value !== 0n) {
                return { kind: "revert", data: errorRevert("value must be zero on replay") };
            }
            if (ledger.messageStatus(hash) !== "failed") {
                return { kind: "revert", data: errorRevert("message cannot be replayed") };
            }
        }

        // The inner call, OP-style: its failure is recorded, not propagated.
        // The mark bounds exactly the dispatch — the deposit's mint and the
        // router's value move to this handler happened before it and survive.
        const mark = ledger.journal.mark();
        try {
            await this.dispatch(m, cd, out, ledger);
            ledger.setMessageStatus(hash, "successful");
            {
                const { topics, data } = relayedMessageLog(hash, false);
                out.log(L2_MESSENGER_ADDRESS, topics, data);
            }
        } catch {
            await ledger.journal.rollbackTo(mark);
            await ledger.afterRollback();
            ledger.setMessageStatus(hash, "failed");
            const { topics, data } = relayedMessageLog(hash, true);
            out.log(L2_MESSENGER_ADDRESS, topics, data);
        }
        return { kind: "accept" };
    }

    /** The relayed call. Throws to signal failure; must emit nothing before
     * its last throwing operation. */
    private async dispatch(
        m: CrossDomainMessage,
        cd: CrossDomainConfig,
        out: OutputsSink,
        ledger: Ledger,
    ): Promise<void> {
        if (sameAddress(m.target, L2_BRIDGE_ADDRESS)) {
            await this.bridge.finalizeFromMessenger(m, cd, out, ledger);
            return;
        }
        // A target this guest routes nothing at: a plain call. With value it
        // is a transfer — the messenger passes the value it was carrying on
        // to the target — and with calldata there is nothing to run it
        // against, so it fails and becomes replayable.
        if (size(m.message) > 0) {
            throw new Error("unsupported relay target");
        }
        await ledger.debitEther(L2_MESSENGER_ADDRESS, m.value);
        await ledger.creditEther(m.target, m.value);
    }

    async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
        const decoded = tryDecodeCalldata(l2CrossDomainMessengerAbi, call.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "messageNonce":
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: l2CrossDomainMessengerAbi,
                        functionName: "messageNonce",
                        result: encodeVersionedNonce(ledger.peekMessageNonce(), MESSAGE_VERSION),
                    }),
                };
            case "successfulMessages":
            case "failedMessages": {
                const [hash] = decoded.args;
                const want =
                    decoded.functionName === "successfulMessages" ? "successful" : "failed";
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: l2CrossDomainMessengerAbi,
                        functionName: decoded.functionName,
                        result: ledger.messageStatus(hash as Hex) === want,
                    }),
                };
            }
            default:
                return {
                    kind: "revert",
                    data: errorRevert(`${decoded.functionName} is not a view`),
                };
        }
    }
}

/** The outbound half of CrossDomainMessenger.sendMessage: wrap the message
 * in relayMessage under a fresh versioned nonce, emit the withdrawal that
 * carries it — sender = this messenger, target = the L1 messenger, gas =
 * baseGas — and the SentMessage events explorers index. Used by the L2
 * bridge; any future guest contract sending L1 messages goes through here. */
export function sendCrossDomainMessage(
    cd: CrossDomainConfig,
    args: { sender: Address; target: Address; message: Hex; minGasLimit: bigint; value: bigint },
    out: OutputsSink,
    ledger: Ledger,
): void {
    const nonce = encodeVersionedNonce(ledger.nextMessageNonce(), MESSAGE_VERSION);
    const relay = encodeRelayMessage({
        nonce,
        sender: args.sender,
        target: args.target,
        value: args.value,
        minGasLimit: args.minGasLimit,
        message: args.message,
    });
    out.withdrawal({
        sender: L2_MESSENGER_ADDRESS,
        target: cd.l1Messenger,
        value: args.value,
        gasLimit: baseGas(size(args.message), args.minGasLimit),
        data: relay,
    });
    const sent = sentMessageLog({
        target: args.target,
        sender: args.sender,
        message: args.message,
        messageNonce: nonce,
        gasLimit: baseGas(size(args.message), args.minGasLimit),
    });
    out.log(L2_MESSENGER_ADDRESS, sent.topics, sent.data);
    const ext = sentMessageExtension1Log(args.sender, args.value);
    out.log(L2_MESSENGER_ADDRESS, ext.topics, ext.data);
}
