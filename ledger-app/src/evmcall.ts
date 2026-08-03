// The inspect envelope (EVM-COMPAT §7): eth_call arrives as
//   EvmCall(uint256 chainId, address from, address to, uint256 value, bytes data)
// and the reserved simulation entry as EvmSimulate with the same fields —
// the advance path minus enforcement, on the inspect fork the host discards.

import {
    concat,
    decodeAbiParameters,
    encodeAbiParameters,
    encodeErrorResult,
    toFunctionSelector,
    type Address,
    type Hex,
} from "viem";

export const EVM_CALL_SELECTOR: Hex = toFunctionSelector("EvmCall(uint256,address,address,uint256,bytes)");
export const EVM_SIMULATE_SELECTOR: Hex = toFunctionSelector("EvmSimulate(uint256,address,address,uint256,bytes)");

const PARAMS = [
    { type: "uint256" }, // chainId
    { type: "address" }, // from
    { type: "address" }, // to
    { type: "uint256" }, // value
    { type: "bytes" }, // data
] as const;

export interface DecodedCall {
    simulate: boolean;
    chainId: bigint;
    from: Address;
    to: Address;
    value: bigint;
    data: Uint8Array;
}

export function decodeEvmCall(payload: Uint8Array): DecodedCall | null {
    if (payload.length < 4) return null;
    const selector = `0x${Buffer.from(payload.slice(0, 4)).toString("hex")}` as Hex;
    const simulate = selector === EVM_SIMULATE_SELECTOR;
    if (!simulate && selector !== EVM_CALL_SELECTOR) return null;
    try {
        const body = `0x${Buffer.from(payload.slice(4)).toString("hex")}` as Hex;
        const [chainId, from, to, value, data] = decodeAbiParameters(PARAMS, body);
        return {
            simulate,
            chainId,
            from,
            to,
            value,
            data: data === "0x" ? new Uint8Array(0) : Buffer.from(data.slice(2), "hex"),
        };
    } catch {
        return null;
    }
}

export function encodeEvmCall(c: Omit<DecodedCall, "simulate"> & { simulate?: boolean }): Hex {
    const selector = c.simulate ? EVM_SIMULATE_SELECTOR : EVM_CALL_SELECTOR;
    return concat([
        selector,
        encodeAbiParameters(PARAMS, [
            c.chainId,
            c.from,
            c.to,
            c.value,
            `0x${Buffer.from(c.data).toString("hex")}`,
        ]),
    ]);
}

/** Solidity Error(string), the standard require-style revert payload. */
export function errorRevert(message: string): Hex {
    return encodeErrorResult({
        abi: [{ type: "error", name: "Error", inputs: [{ type: "string" }] }],
        errorName: "Error",
        args: [message],
    });
}
