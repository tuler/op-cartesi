// The adopted OP predeploy (DESIGN §5): L2ToL1MessagePasser at 0x4200…0016,
// the contract viem's initiateWithdrawal targets — so the stock OP withdrawal
// flow (viem.sh/op-stack/guides/withdrawals) runs against this chain without
// knowing the execution layer is not an EVM.
//
// initiateWithdrawal is payable: the router has already moved msg.value to
// the passer's own balance, so the handler burns its own funds — the same
// burn withdrawEther performs — and emits the OP Withdrawal message whose
// hash enters the block's withdrawal trie. The caller chooses the L1 gas
// limit and calldata, exactly the real passer's surface; a plain value
// transfer is the real passer's receive() and withdraws to the sender at
// OP's default gas limit.

import { errorRevert, messagePasserAbi, tryDecodeCalldata } from "@op-cartesi/evm";
import type { Ledger } from "../ledger.ts";
import type { AdvanceOutcome, Handler, OutputsSink, TxContext } from "../types.ts";

export class MessagePasser implements Handler {
    payable = true;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        if (ctx.data.length === 0) {
            // receive(): withdraw the value back to whoever sent it.
            await ledger.debitEther(ctx.to, ctx.value);
            out.withdrawal({ sender: ctx.sender, target: ctx.sender, value: ctx.value });
            return { kind: "accept" };
        }
        const decoded = tryDecodeCalldata(messagePasserAbi, ctx.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        const [target, gasLimit, data] = decoded.args;
        // Burn what the router credited us: the wei leaves L2 here and
        // reappears on L1 through OptimismPortal, paid from the lockbox the
        // deposits funded.
        await ledger.debitEther(ctx.to, ctx.value);
        out.withdrawal({ sender: ctx.sender, target, value: ctx.value, gasLimit, data });
        return { kind: "accept" };
    }
}
