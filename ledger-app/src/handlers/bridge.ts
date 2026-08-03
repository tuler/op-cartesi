// The bridge (EVM-COMPAT §9): withdrawals as vouchers, debiting the
// authenticated sender.
//
// withdrawEther is payable: the router has already moved msg.value to the
// bridge's own balance, so the handler burns its own funds — the
// contract-holds-funds capability rule — and emits the voucher paying `to`.
// withdrawERC20 debits the caller's holding of the token named by its L2
// façade address, and the voucher tells the L1 token to move itself from the
// application contract that has escrowed it since the deposit.

import { decodeFunctionData, encodeFunctionData, parseAbi, toHex, type Address } from "viem";
import { transferLog } from "../events.ts";
import { errorRevert } from "../evmcall.ts";
import { InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, Handler, OutputsSink, TxContext } from "../types.ts";

export const bridgeAbi = parseAbi([
    "function withdrawEther(address to) payable",
    "function withdrawERC20(address token, address to, uint256 amount)",
]);

const erc20TransferAbi = parseAbi(["function transfer(address to, uint256 amount) returns (bool)"]);

const ZERO_ADDRESS: Address = "0x0000000000000000000000000000000000000000";

export class Bridge implements Handler {
    payable = true;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        let decoded: { functionName: string; args: readonly unknown[] };
        try {
            decoded = decodeFunctionData({ abi: bridgeAbi, data: toHex(ctx.data) }) as typeof decoded;
        } catch {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        switch (decoded.functionName) {
            case "withdrawEther": {
                const [to] = decoded.args as readonly [Address];
                if (ctx.value === 0n) {
                    return { kind: "revert", data: errorRevert("withdrawEther: no value") };
                }
                // Burn what the router credited us: the wei leaves L2 here and
                // reappears on L1 through the voucher.
                await ledger.debitEther(ctx.to, ctx.value);
                out.voucher({ destination: to, value: ctx.value });
                return { kind: "accept" };
            }
            case "withdrawERC20": {
                const [l2Token, to, amount] = decoded.args as readonly [Address, Address, bigint];
                const token = ledger.tokenByL2Address(l2Token);
                if (!token) return { kind: "revert", data: errorRevert("unknown token") };
                try {
                    await ledger.debitToken(ctx.sender, token.id, amount);
                } catch (e) {
                    if (e instanceof InsufficientFunds) {
                        return { kind: "revert", data: errorRevert("withdrawERC20: amount exceeds balance") };
                    }
                    throw e;
                }
                out.voucher({
                    destination: token.l1Token,
                    payload: encodeFunctionData({ abi: erc20TransferAbi, functionName: "transfer", args: [to, amount] }),
                });
                // The burn convention indexers expect: Transfer(sender → 0x0)
                // from the token façade's own address.
                const { topics, data } = transferLog(ctx.sender, ZERO_ADDRESS, amount);
                out.log(l2Token, topics, data);
                return { kind: "accept" };
            }
            default:
                return { kind: "revert", data: errorRevert("unknown function") };
        }
    }
}
