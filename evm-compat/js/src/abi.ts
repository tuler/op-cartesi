// One decode helper, shared by every handler. The point is the return type:
// viem's decodeFunctionData over a parseAbi'd ABI returns a discriminated
// union — { functionName: "transfer", args: readonly [Address, bigint] } |
// { functionName: "balanceOf", args: readonly [Address] } | … — so a
// switch on functionName narrows args by itself. Handlers get typed
// arguments with no casts; malformed calldata comes back as null instead of
// a throw, because unknown selectors are a revert, not a crash.

import { decodeFunctionData, toHex, type Abi, type DecodeFunctionDataReturnType } from "viem";

export function tryDecodeCalldata<const abi extends Abi>(
    abi: abi,
    data: Uint8Array,
): DecodeFunctionDataReturnType<abi> | null {
    try {
        return decodeFunctionData({ abi, data: toHex(data) });
    } catch {
        return null;
    }
}
