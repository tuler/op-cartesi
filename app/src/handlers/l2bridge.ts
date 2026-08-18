// The L2StandardBridge (0x4200…0010), adopted for real (DESIGN §7g).
//
// Outbound, bridgeERC20/bridgeERC20To debit the sender's ledger holding and
// send finalizeBridgeERC20 to L1StandardBridge through the messenger — the
// message L1's escrow accounting releases against, so the (l1Token,
// l2Token) pair must be the deterministic façade pair the deposits used.
// Inbound, finalizeBridgeERC20/ETH arrive only through the messenger's
// dispatch, which has already authenticated the L1 messenger; this handler
// checks the xDomainMessageSender is the L1 bridge (OP's onlyOtherBridge)
// and credits the ledger.
//
// There is no OptimismMintableERC20 here: L2 tokens are ledger façades at
// l2TokenAddress(l1Token). Requiring exactly that pair on both directions
// is what keeps L1StandardBridge's deposits[localToken][remoteToken]
// bookkeeping releasable — a deposit under any other pair could never be
// withdrawn, so it is refused (recorded failed, hence replayable) rather
// than credited.

import {
    type CrossDomainMessage,
    ERC20_BRIDGE_FINALIZED_TOPIC,
    ERC20_BRIDGE_INITIATED_TOPIC,
    erc20BridgeLog,
    errorRevert,
    L2_BRIDGE_ADDRESS,
    L2_MESSENGER_ADDRESS,
    l2StandardBridgeAbi,
    l2TokenAddress,
    sameAddress,
    transferLog,
    tryDecodeCalldata,
} from "@op-cartesi/evm";
import {
    type Address,
    encodeAbiParameters,
    encodeFunctionData,
    type Hex,
    keccak256,
    padHex,
    toBytes,
    toHex,
} from "viem";
import { type CrossDomainConfig, InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, Handler, OutputsSink, TxContext } from "../types.ts";
import { sendCrossDomainMessage } from "./messenger.ts";

const ZERO_ADDRESS: Address = "0x0000000000000000000000000000000000000000";

const ETH_BRIDGE_FINALIZED_TOPIC: Hex = keccak256(
    toHex("ETHBridgeFinalized(address,address,uint256,bytes)"),
);

export class L2Bridge implements Handler {
    payable = false;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        const cd = ledger.crossDomain();
        if (!cd) {
            return { kind: "revert", data: errorRevert("no standard bridge registered") };
        }
        const decoded = tryDecodeCalldata(l2StandardBridgeAbi, ctx.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "bridgeERC20": {
                const [localToken, remoteToken, amount, minGasLimit, extraData] = decoded.args;
                return this.initiate(ctx, cd, out, ledger, {
                    localToken,
                    remoteToken,
                    to: ctx.sender,
                    amount,
                    minGasLimit: BigInt(minGasLimit),
                    extraData,
                });
            }
            case "bridgeERC20To": {
                const [localToken, remoteToken, to, amount, minGasLimit, extraData] = decoded.args;
                return this.initiate(ctx, cd, out, ledger, {
                    localToken,
                    remoteToken,
                    to,
                    amount,
                    minGasLimit: BigInt(minGasLimit),
                    extraData,
                });
            }
            default:
                // finalizeBridgeERC20 / finalizeBridgeETH: reachable only
                // through the messenger's dispatch, which carries the
                // cross-domain authentication a direct call cannot — OP's
                // onlyOtherBridge.
                return {
                    kind: "revert",
                    data: errorRevert("finalize is only callable by the messenger"),
                };
        }
    }

    private async initiate(
        ctx: TxContext,
        cd: CrossDomainConfig,
        out: OutputsSink,
        ledger: Ledger,
        a: {
            localToken: Address;
            remoteToken: Address;
            to: Address;
            amount: bigint;
            minGasLimit: bigint;
            extraData: Hex;
        },
    ): Promise<AdvanceOutcome> {
        const token = ledger.tokenByL2Address(a.localToken);
        if (!token || !sameAddress(token.l1Token, a.remoteToken)) {
            return { kind: "revert", data: errorRevert("wrong token pair") };
        }
        try {
            await ledger.debitToken(ctx.sender, token.id, a.amount);
        } catch (e) {
            if (e instanceof InsufficientFunds) {
                return { kind: "revert", data: errorRevert("amount exceeds balance") };
            }
            throw e;
        }
        // The burn convention wallets expect, from the façade.
        const burn = transferLog(ctx.sender, ZERO_ADDRESS, a.amount);
        out.log(a.localToken, burn.topics, burn.data);
        const initiated = erc20BridgeLog(ERC20_BRIDGE_INITIATED_TOPIC, {
            localToken: a.localToken,
            remoteToken: a.remoteToken,
            from: ctx.sender,
            to: a.to,
            amount: a.amount,
            extraData: a.extraData,
        });
        out.log(L2_BRIDGE_ADDRESS, initiated.topics, initiated.data);
        // The L1 call: finalizeBridgeERC20 with local/remote swapped to the
        // L1 perspective, sent from this bridge to the other bridge.
        sendCrossDomainMessage(
            cd,
            {
                sender: L2_BRIDGE_ADDRESS,
                target: cd.l1Bridge,
                message: encodeFunctionData({
                    abi: l2StandardBridgeAbi,
                    functionName: "finalizeBridgeERC20",
                    args: [a.remoteToken, a.localToken, ctx.sender, a.to, a.amount, a.extraData],
                }),
                minGasLimit: a.minGasLimit,
                value: 0n,
            },
            out,
            ledger,
        );
        return { kind: "accept" };
    }

    /** The messenger's dispatch entry: the relayed call, with the message's
     * cross-domain sender in m.sender. Throws to fail the relay; emits
     * nothing before its last throwing operation, because the sink does not
     * roll back. */
    async finalizeFromMessenger(
        m: CrossDomainMessage,
        cd: CrossDomainConfig,
        out: OutputsSink,
        ledger: Ledger,
    ): Promise<void> {
        if (!sameAddress(m.sender, cd.l1Bridge)) {
            throw new Error("xDomainMessageSender is not the L1 bridge");
        }
        const decoded = tryDecodeCalldata(l2StandardBridgeAbi, toBytes(m.message));
        if (!decoded) throw new Error("not a bridge message");
        switch (decoded.functionName) {
            case "finalizeBridgeERC20": {
                const [localToken, remoteToken, from, to, amount, extraData] = decoded.args;
                if (m.value !== 0n) throw new Error("ERC-20 message carries value");
                // The only pair a later withdrawal can release: the
                // deterministic façade of the L1 token.
                if (!sameAddress(localToken, l2TokenAddress(remoteToken))) {
                    throw new Error("wrong token pair");
                }
                let token = ledger.tokenByL1Address(remoteToken);
                if (!token) {
                    // First-seen registration, the portal receiver's policy.
                    const { id } = await ledger.registerToken(remoteToken);
                    token = { id, l1Token: remoteToken, width: 32 };
                }
                await ledger.creditToken(to, token.id, amount);
                const mint = transferLog(ZERO_ADDRESS, to, amount);
                out.log(localToken, mint.topics, mint.data);
                const finalized = erc20BridgeLog(ERC20_BRIDGE_FINALIZED_TOPIC, {
                    localToken,
                    remoteToken,
                    from,
                    to,
                    amount,
                    extraData,
                });
                out.log(L2_BRIDGE_ADDRESS, finalized.topics, finalized.data);
                return;
            }
            case "finalizeBridgeETH": {
                const [from, to, amount, extraData] = decoded.args;
                if (m.value !== amount) throw new Error("value does not match amount");
                // The messenger holds the deposit's value; pass it on.
                await ledger.debitEther(L2_MESSENGER_ADDRESS, amount);
                await ledger.creditEther(to, amount);
                out.log(
                    L2_BRIDGE_ADDRESS,
                    [
                        ETH_BRIDGE_FINALIZED_TOPIC,
                        padHex(from, { size: 32 }),
                        padHex(to, { size: 32 }),
                    ],
                    encodeAbiParameters(
                        [{ type: "uint256" }, { type: "bytes" }],
                        [amount, extraData],
                    ),
                );
                return;
            }
            default:
                throw new Error(`unsupported bridge message ${decoded.functionName}`);
        }
    }
}
