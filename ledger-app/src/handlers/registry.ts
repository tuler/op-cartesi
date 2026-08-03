// The router registry (EVM-COMPAT §6): discovery views over the manifest,
// and the source the shim reads eth_getCode markers from.

import { decodeFunctionData, encodeFunctionResult, parseAbi, toHex, type Address } from "viem";
import { l2TokenAddress } from "../addresses.ts";
import { errorRevert } from "../evmcall.ts";
import type { Ledger } from "../ledger.ts";
import type { CallContext, Handler, ViewOutcome } from "../types.ts";

export const registryAbi = parseAbi([
    "function handlerAt(address target) view returns (bool routed, bool isPayable)",
    "function handlers() view returns (address[])",
    "function l2TokenOf(address l1Token) view returns (address)",
]);

export class Registry implements Handler {
    payable = false;

    /** Wired after construction: the router's manifest lookup. */
    lookup: (addr: Address) => { payable: boolean } | undefined = () => undefined;
    list: () => Address[] = () => [];

    async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
        let decoded: { functionName: string; args?: readonly unknown[] };
        try {
            decoded = decodeFunctionData({ abi: registryAbi, data: toHex(call.data) }) as typeof decoded;
        } catch {
            return { kind: "revert", data: errorRevert("unknown function") };
        }
        switch (decoded.functionName) {
            case "handlerAt": {
                const [target] = decoded.args as readonly [Address];
                const h = this.lookup(target) ?? (ledger.tokenByL2Address(target) ? { payable: false } : undefined);
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: registryAbi,
                        functionName: "handlerAt",
                        result: [h !== undefined, h?.payable ?? false],
                    }),
                };
            }
            case "handlers": {
                const all = [...this.list(), ...ledger.drive.tokens().map((t) => {
                    const l1 = `0x${Buffer.from(t.address).toString("hex")}` as Address;
                    return l2TokenAddress(l1);
                })];
                return {
                    kind: "return",
                    data: encodeFunctionResult({ abi: registryAbi, functionName: "handlers", result: all }),
                };
            }
            case "l2TokenOf": {
                const [l1Token] = decoded.args as readonly [Address];
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: registryAbi,
                        functionName: "l2TokenOf",
                        result: l2TokenAddress(l1Token),
                    }),
                };
            }
            default:
                return { kind: "revert", data: errorRevert("unknown function") };
        }
    }
}
