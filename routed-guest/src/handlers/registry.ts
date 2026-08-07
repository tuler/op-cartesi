// The router registry (EVM-COMPAT §6): discovery views over the manifest,
// and the source the shim reads eth_getCode markers from.

import { errorRevert, l2TokenAddress, registryAbi, tryDecodeCalldata } from "@cartesi/evm-compat";
import { type Address, encodeFunctionResult } from "viem";
import type { Ledger } from "../ledger.ts";
import type { CallContext, Handler, ViewOutcome } from "../types.ts";

export class Registry implements Handler {
    payable = false;

    /** Wired after construction: the router's manifest lookup. */
    lookup: (addr: Address) => { payable: boolean } | undefined = () => undefined;
    list: () => Address[] = () => [];

    async view(call: CallContext, ledger: Ledger): Promise<ViewOutcome> {
        const decoded = tryDecodeCalldata(registryAbi, call.data);
        if (!decoded) return { kind: "revert", data: errorRevert("unknown function") };
        switch (decoded.functionName) {
            case "handlerAt": {
                const [target] = decoded.args;
                const h =
                    this.lookup(target) ??
                    (ledger.tokenByL2Address(target) ? { payable: false } : undefined);
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
                const all = [
                    ...this.list(),
                    ...ledger.drive
                        .tokens()
                        .map((t) => l2TokenAddress(`0x${Buffer.from(t.address).toString("hex")}`)),
                ];
                return {
                    kind: "return",
                    data: encodeFunctionResult({
                        abi: registryAbi,
                        functionName: "handlers",
                        result: all,
                    }),
                };
            }
            case "l2TokenOf": {
                const [l1Token] = decoded.args;
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
