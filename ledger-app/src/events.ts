// Events ride the provable channel (EVM-COMPAT §8): a handler's event is a
// notice whose payload is EvmLog(address emitter, bytes32[] topics, bytes
// data). At the outputs level it is an ordinary Notice(bytes); the shim tries
// the EvmLog decode at receipt-synthesis time and falls back to the raw
// CartesiOutput form.

import {
    concat,
    encodeAbiParameters,
    keccak256,
    padHex,
    toFunctionSelector,
    toHex,
    type Address,
    type Hex,
} from "viem";

export const EVM_LOG_SIGNATURE = "EvmLog(address,bytes32[],bytes)";
export const EVM_LOG_SELECTOR: Hex = toFunctionSelector(EVM_LOG_SIGNATURE);

const EVM_LOG_PARAMS = [
    { type: "address" },
    { type: "bytes32[]" },
    { type: "bytes" },
] as const;

export function encodeEvmLog(emitter: Address, topics: Hex[], data: Hex): Hex {
    return concat([EVM_LOG_SELECTOR, encodeAbiParameters(EVM_LOG_PARAMS, [emitter, topics, data])]);
}

/** ERC-20 Transfer(address indexed from, address indexed to, uint256 value). */
export const TRANSFER_TOPIC: Hex = keccak256(toHex("Transfer(address,address,uint256)"));

export function transferLog(from: Address, to: Address, value: bigint): { topics: Hex[]; data: Hex } {
    return {
        topics: [TRANSFER_TOPIC, padHex(from, { size: 32 }), padHex(to, { size: 32 })],
        data: toHex(value, { size: 32 }),
    };
}
