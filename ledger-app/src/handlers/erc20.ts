// The ERC-20 façade (EVM-COMPAT §9): one handler serving every registered
// token at its derived address, mapping the ABI onto the accounts drive's
// sparse table. approve/allowance/transferFrom are deferred — every v1 money
// path debits msg.sender, which the signature already authorizes.

import { decodeFunctionData, encodeFunctionResult, parseAbi, toHex, type Address } from "viem";
import { transferLog } from "../events.ts";
import { errorRevert } from "../evmcall.ts";
import { InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, CallContext, Handler, OutputsSink, TxContext, ViewOutcome } from "../types.ts";

export const erc20Abi = parseAbi([
    "function balanceOf(address owner) view returns (uint256)",
    "function totalSupply() view returns (uint256)",
    "function decimals() view returns (uint8)",
    "function name() view returns (string)",
    "function symbol() view returns (string)",
    "function transfer(address to, uint256 value) returns (bool)",
]);

function facadeOf(ledger: Ledger, to: Address) {
    return ledger.tokenByL2Address(to);
}

export class Erc20Facade implements Handler {
    payable = false;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        const token = facadeOf(ledger, ctx.to);
        if (!token) return { kind: "revert", data: errorRevert("unknown token") };
        let decoded: { functionName: string; args: readonly unknown[] };
        try {
            decoded = decodeFunctionData({ abi: erc20Abi, data: toHex(ctx.data) }) as typeof decoded;
        } catch {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        if (decoded.functionName !== "transfer") {
            return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a transaction`) };
        }
        const [to, value] = decoded.args as readonly [Address, bigint];
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
        const token = facadeOf(ledger, call.to);
        if (!token) return { kind: "revert", data: errorRevert("unknown token") };
        let decoded: { functionName: string; args: readonly unknown[] };
        try {
            decoded = decodeFunctionData({ abi: erc20Abi, data: toHex(call.data) }) as typeof decoded;
        } catch {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        const ret = (name: string, result: unknown): ViewOutcome => ({
            kind: "return",
            data: encodeFunctionResult({ abi: erc20Abi, functionName: name as never, result: result as never }),
        });
        switch (decoded.functionName) {
            case "balanceOf": {
                const [owner] = decoded.args as readonly [Address];
                return ret("balanceOf", await ledger.tokenBalance(owner, token.id));
            }
            case "totalSupply":
                return ret("totalSupply", ledger.totalSupply(token.id));
            case "decimals":
                return ret("decimals", ledger.metadataOf(token.id).decimals);
            case "name":
                return ret("name", ledger.metadataOf(token.id).name);
            case "symbol":
                return ret("symbol", ledger.metadataOf(token.id).symbol);
            default:
                return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a view`) };
        }
    }
}
