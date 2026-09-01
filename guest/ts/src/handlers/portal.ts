// The Cartesi-portal receiver (EVM-COMPAT §6, §9). The router resolves it at
// the envelope's application contract address and, for deposits, by a
// registered portal *sender* whatever the `to` — the sender is the
// authentication, and the application contract does not exist yet when the
// chain's genesis is built.
//
// Its calldata is InputEncoding's packed format, not ABI: the router routes
// and the handler owns its calldata. Which layout applies comes from *who
// sent it* — the aliased portal address, registered by the owner — never
// from the bytes.
//
//   ether: sender(20) ‖ value(32) ‖ extra
//   erc20: token(20) ‖ sender(20) ‖ value(32) ‖ extra
//
// Ether deposited through a Cartesi-style ether portal arrives as the mint/value
// addressed here, so by dispatch time the router has already credited this
// handler's own balance; crediting the named beneficiary is the handler
// moving its own funds — the capability rule, satisfied naturally.

import { errorRevert, l2TokenAddress, PORTAL_ETHER, transferLog } from "@op-cartesi/evm";
import { type Address, getAddress, toHex } from "viem";
import { InsufficientFunds, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, Handler, OutputsSink, TxContext } from "../types.ts";

const ZERO_ADDRESS: Address = "0x0000000000000000000000000000000000000000";

function addressAt(data: Uint8Array, off: number): Address {
    return getAddress(toHex(data.slice(off, off + 20)));
}

function uint256At(data: Uint8Array, off: number): bigint {
    return BigInt(toHex(data.slice(off, off + 32)));
}

export class PortalReceiver implements Handler {
    payable = true;

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        if (!ctx.isDeposit) {
            return { kind: "revert", data: errorRevert("not a portal deposit") };
        }
        const kind = ledger.portalKind(ctx.sender);
        if (kind === undefined) {
            // A deposit from something that is not a registered portal must
            // credit nothing: the portal address is the authentication.
            return { kind: "revert", data: errorRevert("unregistered portal") };
        }
        if (kind === PORTAL_ETHER) {
            if (ctx.data.length < 52)
                return { kind: "revert", data: errorRevert("short ether deposit") };
            const beneficiary = addressAt(ctx.data, 0);
            const amount = uint256At(ctx.data, 20);
            try {
                // The router credited this handler with the deposit's value;
                // pass it on to the depositor the portal named.
                await ledger.debitEther(ctx.to, amount);
                await ledger.creditEther(beneficiary, amount);
            } catch (e) {
                if (e instanceof InsufficientFunds) {
                    return {
                        kind: "revert",
                        data: errorRevert("deposit value does not cover the credit"),
                    };
                }
                throw e; // tableFull etc. — a deposit has no charge to keep, so the router rejects
            }
            return { kind: "accept" };
        }
        // PORTAL_ERC20
        if (ctx.data.length < 72)
            return { kind: "revert", data: errorRevert("short erc20 deposit") };
        const l1Token = addressAt(ctx.data, 0);
        const beneficiary = addressAt(ctx.data, 20);
        const amount = uint256At(ctx.data, 40);
        // First-seen registration (spec §8's permissionless policy), width 32
        // like the Lua guest: devnet tokens are arbitrary ERC-20s. Throws
        // registryFull → the router rejects, this being a deposit.
        let token = ledger.tokenByL1Address(l1Token);
        if (!token) {
            const { id } = await ledger.registerToken(l1Token);
            token = { id, l1Token, width: 32 };
        }
        await ledger.creditToken(beneficiary, token.id, amount); // overflow/tableFull → reject
        // The mint convention indexers expect: Transfer(0x0 → beneficiary)
        // from the token's façade address.
        const { topics, data } = transferLog(ZERO_ADDRESS, beneficiary, amount);
        out.log(l2TokenAddress(l1Token), topics, data);
        return { kind: "accept" };
    }
}
