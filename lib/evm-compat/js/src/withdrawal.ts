// Withdrawals ride the provable channel the way events do (events.ts): a
// notice whose payload is Withdrawal(uint256 nonce, address sender, address
// target, uint256 value, uint256 gasLimit, bytes data) — the fields of the
// OP Stack's WithdrawalTransaction, in order. The shim recognizes the shape,
// hashes it exactly as Hashing.hashWithdrawal does, and puts the hash's
// sentMessages storage slot into the block's withdrawal trie — after which
// OptimismPortal.proveWithdrawalTransaction can verify it like any OP
// withdrawal.
//
// It is deliberately a notice and not a voucher: a voucher is executable by
// the Cartesi output executor, and a message the portal can finalize must
// not have a second executor to be paid by. As a notice it still enters the
// Cartesi outputs tree, so it is provable both ways but executable only
// through the portal.

import {
    type Address,
    concat,
    decodeAbiParameters,
    encodeAbiParameters,
    type Hex,
    keccak256,
    parseAbiParameters,
    toFunctionSelector,
} from "viem";

/** OP's L2ToL1MessagePasser predeploy address: the account whose storage
 * trie the withdrawal commitment stands in for, the emitter of the
 * synthesized MessagePassed logs, and — through the guest's passer handler —
 * a live initiateWithdrawal target, so viem's stock withdrawal flow works
 * unchanged. */
export const MESSAGE_PASSER_ADDRESS: Address = "0x4200000000000000000000000000000000000016";

export const WITHDRAWAL_SIGNATURE = "Withdrawal(uint256,address,address,uint256,uint256,bytes)";
export const WITHDRAWAL_SELECTOR: Hex = toFunctionSelector(WITHDRAWAL_SIGNATURE);

const WITHDRAWAL_PARAMS = parseAbiParameters(
    "uint256 nonce, address sender, address target, uint256 value, uint256 gasLimit, bytes data",
);

/** The gas the L1 call gets when the guest does not choose one — OP's
 * RECEIVE_DEFAULT_GAS_LIMIT, plenty for paying ether to an address. */
export const WITHDRAWAL_DEFAULT_GAS_LIMIT = 100_000n;

/** The OP Stack's WithdrawalTransaction, as the guest emits it. */
export interface WithdrawalMessage {
    nonce: bigint;
    sender: Address;
    target: Address;
    value: bigint;
    gasLimit: bigint;
    data: Hex;
}

/** Encodes the withdrawal as a notice payload. */
export function encodeWithdrawal(w: WithdrawalMessage): Hex {
    return concat([
        WITHDRAWAL_SELECTOR,
        encodeAbiParameters(WITHDRAWAL_PARAMS, [
            w.nonce,
            w.sender,
            w.target,
            w.value,
            w.gasLimit,
            w.data,
        ]),
    ]);
}

/** Cartesi's Notice(bytes) output call, the wire framing every notice —
 * withdrawals included — travels under. */
export const NOTICE_SELECTOR: Hex = toFunctionSelector("Notice(bytes)");

/** Decodes a raw machine output — the Notice(bytes) call encoding the RPC
 * serves in emissions — into the withdrawal it wraps, or undefined when the
 * output is not a withdrawal notice. The counterpart of the node's
 * ParseWithdrawalOutput. */
export function decodeWithdrawalOutput(output: Hex): WithdrawalMessage | undefined {
    if (!output.toLowerCase().startsWith(NOTICE_SELECTOR.toLowerCase())) return undefined;
    try {
        const [payload] = decodeAbiParameters(
            parseAbiParameters("bytes"),
            `0x${output.slice(NOTICE_SELECTOR.length)}`,
        );
        return decodeWithdrawal(payload);
    } catch {
        return undefined;
    }
}

/** Hashing.hashWithdrawal: the withdrawal hash OptimismPortal keys its
 * replay protection by, and whose sentMessages slot the trie holds. */
export function withdrawalHash(w: WithdrawalMessage): Hex {
    return keccak256(
        encodeAbiParameters(WITHDRAWAL_PARAMS, [
            w.nonce,
            w.sender,
            w.target,
            w.value,
            w.gasLimit,
            w.data,
        ]),
    );
}

/** The L2ToL1MessagePasser.sentMessages storage slot of a withdrawal hash:
 * keccak256(abi.encode(hash, uint256(0))) — what eth_getProof is asked for. */
export function withdrawalSlot(hash: Hex): Hex {
    return keccak256(encodeAbiParameters([{ type: "bytes32" }, { type: "uint256" }], [hash, 0n]));
}

/** Decodes a notice payload back into the withdrawal it encodes, or
 * undefined when the payload is not a withdrawal. */
export function decodeWithdrawal(payload: Hex): WithdrawalMessage | undefined {
    if (!payload.toLowerCase().startsWith(WITHDRAWAL_SELECTOR.toLowerCase())) return undefined;
    try {
        const [nonce, sender, target, value, gasLimit, data] = decodeAbiParameters(
            WITHDRAWAL_PARAMS,
            `0x${payload.slice(WITHDRAWAL_SELECTOR.length)}`,
        );
        return { nonce, sender, target, value, gasLimit, data };
    } catch {
        return undefined;
    }
}

/** The nonce a withdrawal carries: OP's message version 1 in the top 16
 * bits (Encoding.encodeVersionedNonce), and below it a value that is unique
 * without any stored counter — the chain-wide input index shifted past a
 * per-input ordinal. Uniqueness is what matters: the portal's replay
 * protection is per withdrawal hash, so two otherwise-identical withdrawals
 * must differ here. Nothing on L1 requires the counter to be dense. */
export function versionedWithdrawalNonce(inputIndex: bigint, ordinal: bigint): bigint {
    if (ordinal >= 1n << 32n) throw new Error("too many withdrawals in one input");
    return (1n << 240n) | (inputIndex << 32n) | ordinal;
}
