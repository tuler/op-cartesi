// The bridge (EVM-COMPAT §9): withdrawals out, debiting the authenticated
// sender — by two different L1 mechanisms.
//
// withdrawEther is payable: the router has already moved msg.value to the
// bridge's own balance, so the handler burns its own funds — the
// contract-holds-funds capability rule — and emits an OP Stack Withdrawal
// message paying `to`. It finalizes through OptimismPortal against the
// block's withdrawal trie, drawing on the lockbox the deposits funded.
// withdrawERC20 debits the caller's holding of the token named by its L2
// façade address, and stays a Cartesi voucher: the tokens are escrowed in
// the application contract, which only a voucher executing from that
// contract's context can move.

import { bridgeAbi, errorRevert, transferLog, tryDecodeCalldata } from "@op-cartesi/evm";
import { type Address, encodeFunctionData, parseAbi } from "viem";
import { InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, Handler, OutputsSink, TxContext } from "../types.ts";

const erc20TransferAbi = parseAbi(["function transfer(address to, uint256 amount) returns (bool)"]);

const ZERO_ADDRESS: Address = "0x0000000000000000000000000000000000000000";

export class Bridge implements Handler {
    payable = true;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        const decoded = tryDecodeCalldata(bridgeAbi, ctx.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "withdrawEther": {
                const [to] = decoded.args;
                if (ctx.value === 0n) {
                    return { kind: "revert", data: errorRevert("withdrawEther: no value") };
                }
                // Burn what the router credited us: the wei leaves L2 here
                // and reappears on L1 through OptimismPortal, which pays it
                // from the same lockbox deposits fund — so both directions
                // agree about who holds the money. The withdrawal's sender
                // is the user, exactly as if they had called
                // initiateWithdrawal on the message passer themselves.
                await ledger.debitEther(ctx.to, ctx.value);
                out.withdrawal({ sender: ctx.sender, target: to, value: ctx.value });
                return { kind: "accept" };
            }
            case "withdrawERC20": {
                const [l2Token, to, amount] = decoded.args;
                const token = ledger.tokenByL2Address(l2Token);
                if (!token) return { kind: "revert", data: errorRevert("unknown token") };
                try {
                    await ledger.debitToken(ctx.sender, token.id, amount);
                } catch (e) {
                    if (e instanceof InsufficientFunds) {
                        return {
                            kind: "revert",
                            data: errorRevert("withdrawERC20: amount exceeds balance"),
                        };
                    }
                    throw e;
                }
                out.voucher({
                    destination: token.l1Token,
                    payload: encodeFunctionData({
                        abi: erc20TransferAbi,
                        functionName: "transfer",
                        args: [to, amount],
                    }),
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
