// The config contract (EVM-COMPAT §9): the owner tags of the Lua guest
// ("p", "f") grown into ABI methods, authenticated by sender identity — which
// works identically whether the owner speaks through an L1 deposit (the
// deposit's own from) or an ordinary signed L2 transaction (the recovered
// signer). Configuration changes are events: consensus state belongs in the
// outputs tree where it can be proven (the same argument the Lua app made
// for its notices).

import {
    encodeAbiParameters,
    encodeFunctionResult,
    keccak256,
    padHex,
    parseAbi,
    toHex,
    type Address,
    type Hex,
} from "viem";
import { tryDecodeCalldata } from "../abi.ts";
import { applyL1ToL2Alias, sameAddress } from "../addresses.ts";
import { errorRevert } from "../evmcall.ts";
import { AccountsDriveError, type Ledger } from "../ledger.ts";
import type { AdvanceOutcome, CallContext, Handler, OutputsSink, TxContext, ViewOutcome } from "../types.ts";

export const configAbi = parseAbi([
    "function setFee(uint256 fee)",
    "function registerPortal(uint8 kind, address portal)",
    "function registerToken(address l1Token, uint8 width, string name, string symbol, uint8 decimals)",
    "function setTokenMetadata(address l1Token, string name, string symbol, uint8 decimals)",
    "function fee() view returns (uint256)",
    "function owner() view returns (address)",
]);

export const PORTAL_ETHER = 0;
export const PORTAL_ERC20 = 1;

const FEE_SET_TOPIC: Hex = keccak256(toHex("FeeSet(uint256)"));
const PORTAL_REGISTERED_TOPIC: Hex = keccak256(toHex("PortalRegistered(uint8,address,address)"));
const TOKEN_REGISTERED_TOPIC: Hex = keccak256(toHex("TokenRegistered(address,address,uint16)"));

export class Config implements Handler {
    payable = false;

    constructor(
        private owner: Address,
        private configAddress: Address,
    ) {}

    async advance(ctx: TxContext, out: OutputsSink, ledger: Ledger): Promise<AdvanceOutcome> {
        if (!sameAddress(ctx.sender, this.owner)) {
            return { kind: "revert", data: errorRevert("not the owner") };
        }
        const decoded = tryDecodeCalldata(configAbi, ctx.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "setFee": {
                const [fee] = decoded.args;
                ledger.setFee(fee);
                out.log(this.configAddress, [FEE_SET_TOPIC], toHex(fee, { size: 32 }));
                return { kind: "accept" };
            }
            case "registerPortal": {
                const [kind, portal] = decoded.args;
                if (kind !== PORTAL_ETHER && kind !== PORTAL_ERC20) {
                    return { kind: "revert", data: errorRevert("unknown portal kind") };
                }
                const aliased = applyL1ToL2Alias(portal);
                ledger.registerPortal(aliased, kind);
                out.log(
                    this.configAddress,
                    [PORTAL_REGISTERED_TOPIC],
                    encodeAbiParameters(
                        [{ type: "uint8" }, { type: "address" }, { type: "address" }],
                        [kind, portal, aliased],
                    ),
                );
                return { kind: "accept" };
            }
            case "registerToken": {
                const [l1Token, , name, symbol, decimals] = decoded.args;
                try {
                    const { id, l2Token } = await ledger.registerToken(l1Token, { name, symbol, decimals });
                    out.log(
                        this.configAddress,
                        [TOKEN_REGISTERED_TOPIC, padHex(l1Token, { size: 32 }), padHex(l2Token, { size: 32 })],
                        toHex(BigInt(id), { size: 32 }),
                    );
                    return { kind: "accept" };
                } catch (e) {
                    if (e instanceof AccountsDriveError && e.kind === "duplicateToken") {
                        return { kind: "revert", data: errorRevert("token already registered") };
                    }
                    throw e; // registryFull → the router escalates to reject (spec §8)
                }
            }
            case "setTokenMetadata": {
                const [l1Token, name, symbol, decimals] = decoded.args;
                const token = ledger.tokenByL1Address(l1Token);
                if (!token) return { kind: "revert", data: errorRevert("unknown token") };
                ledger.setMetadata(token.id, { name, symbol, decimals });
                return { kind: "accept" };
            }
            default:
                return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a transaction`) };
        }
    }

    async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
        const decoded = tryDecodeCalldata(configAbi, call.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "fee":
                return {
                    kind: "return",
                    data: encodeFunctionResult({ abi: configAbi, functionName: "fee", result: ledger.fee }),
                };
            case "owner":
                return {
                    kind: "return",
                    data: encodeFunctionResult({ abi: configAbi, functionName: "owner", result: this.owner }),
                };
            default:
                return { kind: "revert", data: errorRevert(`${decoded.functionName} is not a view`) };
        }
    }
}
