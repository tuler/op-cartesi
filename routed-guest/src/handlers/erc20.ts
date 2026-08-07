// The ERC-20 façade (EVM-COMPAT §9): one handler serving every registered
// token at its derived address, mapping the ABI onto the accounts drive's
// sparse table. approve/allowance/transferFrom are deferred — every v1 money
// path debits msg.sender, which the signature already authorizes.

import { erc20FacadeAbi as erc20Abi, errorRevert, transferLog, tryDecodeCalldata } from "@cartesi/evm-compat";
import { encodeFunctionResult } from "viem";
import { InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, CallContext, Handler, OutputsSink, TxContext, ViewOutcome } from "../types.ts";

export class Erc20Facade implements Handler {
    payable = false;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        const token = ledger.tokenByL2Address(ctx.to);
        if (!token) return { kind: "revert", data: errorRevert("unknown token") };
        const decoded = tryDecodeCalldata(erc20Abi, ctx.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        if (decoded.functionName !== "transfer") {
            return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a transaction`) };
        }
        const [to, value] = decoded.args;
        try {
            await ledger.debitToken(ctx.sender, token.id, value);
            await ledger.creditToken(to, token.id, value);
        } catch (e) {
            if (e instanceof InsufficientFunds) {
                return { kind: "revert", data: errorRevert("ERC20: transfer amount exceeds balance") };
            }
            throw e; // tableFull etc. — the router escalates to reject
        }
        const { topics, data } = transferLog(ctx.sender, to, value);
        out.log(ctx.to, topics, data);
        return { kind: "accept" };
    }

    async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
        const token = ledger.tokenByL2Address(call.to);
        if (!token) return { kind: "revert", data: errorRevert("unknown token") };
        const decoded = tryDecodeCalldata(erc20Abi, call.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "balanceOf": {
                const [owner] = decoded.args;
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: erc20Abi,
                        functionName: "balanceOf",
                        result: await ledger.tokenBalance(owner, token.id),
                    }),
                };
            }
            case "totalSupply":
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: erc20Abi,
                        functionName: "totalSupply",
                        result: ledger.totalSupply(token.id),
                    }),
                };
            case "decimals":
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: erc20Abi,
                        functionName: "decimals",
                        result: ledger.metadataOf(token.id).decimals,
                    }),
                };
            case "name":
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: erc20Abi,
                        functionName: "name",
                        result: ledger.metadataOf(token.id).name,
                    }),
                };
            case "symbol":
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: erc20Abi,
                        functionName: "symbol",
                        result: ledger.metadataOf(token.id).symbol,
                    }),
                };
            default:
                return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a view`) };
        }
    }
}
