// The OP Stack cross-domain messaging vocabulary (DESIGN §7g): what the
// guest needs to *be* L2CrossDomainMessenger and L2StandardBridge — the
// relayMessage call encoding, the versioned nonce, the v1 message hash the
// messengers key replay protection by, the baseGas formula, and the events
// OP tooling indexes. Everything here mirrors contracts-bedrock byte for
// byte; the Foundry suite pins the encodings against the vendored originals
// (CrossDomainVectors.t.sol), because from the moment a withdrawal carries a
// relayMessage payload these encodings are consensus the guest must
// reproduce exactly.

import {
    type Address,
    concat,
    decodeAbiParameters,
    encodeAbiParameters,
    type Hex,
    keccak256,
    padHex,
    parseAbi,
    parseAbiParameters,
    toFunctionSelector,
    toHex,
} from "viem";

/** OP's predeploys, adopted by the routed guest: the messenger receives and
 * sends cross-domain messages, the bridge holds the token semantics. */
export const L2_MESSENGER_ADDRESS: Address = "0x4200000000000000000000000000000000000007";
export const L2_BRIDGE_ADDRESS: Address = "0x4200000000000000000000000000000000000010";

/** The subset of L2CrossDomainMessenger the guest implements. */
export const l2CrossDomainMessengerAbi = parseAbi([
    "function relayMessage(uint256 nonce, address sender, address target, uint256 value, uint256 minGasLimit, bytes message) payable",
    "function messageNonce() view returns (uint256)",
    "function successfulMessages(bytes32 hash) view returns (bool)",
    "function failedMessages(bytes32 hash) view returns (bool)",
]);

/** The subset of L2StandardBridge the guest implements. finalizeBridge* are
 * only reachable through the messenger; bridge* are user entry points. */
export const l2StandardBridgeAbi = parseAbi([
    "function bridgeERC20(address localToken, address remoteToken, uint256 amount, uint32 minGasLimit, bytes extraData)",
    "function bridgeERC20To(address localToken, address remoteToken, address to, uint256 amount, uint32 minGasLimit, bytes extraData)",
    "function finalizeBridgeERC20(address localToken, address remoteToken, address from, address to, uint256 amount, bytes extraData)",
    "function finalizeBridgeETH(address from, address to, uint256 amount, bytes extraData)",
]);

// ------------------------------------------------------------------ nonces

/** Encoding.encodeVersionedNonce: the version in the top 16 bits. */
export function encodeVersionedNonce(nonce: bigint, version: bigint): bigint {
    return (version << 240n) | nonce;
}

/** Encoding.decodeVersionedNonce. */
export function decodeVersionedNonce(nonce: bigint): { nonce: bigint; version: bigint } {
    return { nonce: nonce & ((1n << 240n) - 1n), version: nonce >> 240n };
}

// ---------------------------------------------------------- relayMessage

const RELAY_MESSAGE_SELECTOR: Hex = toFunctionSelector(
    "relayMessage(uint256,address,address,uint256,uint256,bytes)",
);

const RELAY_MESSAGE_PARAMS = parseAbiParameters(
    "uint256 nonce, address sender, address target, uint256 value, uint256 minGasLimit, bytes message",
);

/** One cross-domain message, as relayMessage carries it. */
export interface CrossDomainMessage {
    nonce: bigint;
    sender: Address;
    target: Address;
    value: bigint;
    minGasLimit: bigint;
    message: Hex;
}

/** Encoding.encodeCrossDomainMessageV1: the relayMessage calldata. */
export function encodeRelayMessage(m: CrossDomainMessage): Hex {
    return concat([
        RELAY_MESSAGE_SELECTOR,
        encodeAbiParameters(RELAY_MESSAGE_PARAMS, [
            m.nonce,
            m.sender,
            m.target,
            m.value,
            m.minGasLimit,
            m.message,
        ]),
    ]);
}

/** The inverse, for the receiving messenger. */
export function decodeRelayMessage(data: Hex | Uint8Array): CrossDomainMessage | undefined {
    const hex = typeof data === "string" ? data : toHex(data);
    if (!hex.toLowerCase().startsWith(RELAY_MESSAGE_SELECTOR.toLowerCase())) return undefined;
    try {
        const [nonce, sender, target, value, minGasLimit, message] = decodeAbiParameters(
            RELAY_MESSAGE_PARAMS,
            `0x${hex.slice(RELAY_MESSAGE_SELECTOR.length)}`,
        );
        return { nonce, sender, target, value, minGasLimit, message };
    } catch {
        return undefined;
    }
}

/** Hashing.hashCrossDomainMessageV1 — the unique identifier both messengers
 * key successfulMessages/failedMessages by. */
export function hashCrossDomainMessageV1(m: CrossDomainMessage): Hex {
    return keccak256(encodeRelayMessage(m));
}

// -------------------------------------------------------------- baseGas

// CrossDomainMessenger's gas model, constants and formula verbatim. Only a
// lower bound matters on L1 — nothing recomputes this value there, it is
// simply committed into the withdrawal hash and enforced as the minimum gas
// the finalizer must supply — but matching the formula keeps the guest's
// withdrawals indistinguishable from a real messenger's.
const RELAY_CONSTANT_OVERHEAD = 200_000n;
const MIN_GAS_DYNAMIC_OVERHEAD_NUMERATOR = 64n;
const MIN_GAS_DYNAMIC_OVERHEAD_DENOMINATOR = 63n;
const MIN_GAS_CALLDATA_OVERHEAD = 16n;
const RELAY_CALL_OVERHEAD = 40_000n;
const RELAY_RESERVED_GAS = 40_000n;
const RELAY_GAS_CHECK_BUFFER = 5_000n;
const FLOOR_CALLDATA_OVERHEAD = 40n;
const ENCODING_OVERHEAD = 260n;
const TX_BASE_GAS = 21_000n;

/** CrossDomainMessenger.baseGas: the gas limit a sendMessage-shaped
 * withdrawal carries. `messageLength` is the inner message's byte length. */
export function baseGas(messageLength: number, minGasLimit: bigint): bigint {
    const executionGas =
        RELAY_CONSTANT_OVERHEAD +
        RELAY_CALL_OVERHEAD +
        RELAY_RESERVED_GAS +
        RELAY_GAS_CHECK_BUFFER +
        (minGasLimit * MIN_GAS_DYNAMIC_OVERHEAD_NUMERATOR) / MIN_GAS_DYNAMIC_OVERHEAD_DENOMINATOR;
    const totalMessageSize = BigInt(messageLength) + ENCODING_OVERHEAD;
    const withCalldata = executionGas + totalMessageSize * MIN_GAS_CALLDATA_OVERHEAD;
    const floor = totalMessageSize * FLOOR_CALLDATA_OVERHEAD;
    return TX_BASE_GAS + (withCalldata > floor ? withCalldata : floor);
}

// ---------------------------------------------------------------- events

// The events OP tooling indexes, as (topics, data) for OutputsSink.log.
// MessagePassed is what viem's getWithdrawals extracts from a receipt;
// SentMessage and the bridge events are what explorers and bridge UIs read.

export const MESSAGE_PASSED_TOPIC: Hex = keccak256(
    toHex("MessagePassed(uint256,address,address,uint256,uint256,bytes,bytes32)"),
);

export function messagePassedLog(w: {
    nonce: bigint;
    sender: Address;
    target: Address;
    value: bigint;
    gasLimit: bigint;
    data: Hex;
    withdrawalHash: Hex;
}): { topics: Hex[]; data: Hex } {
    return {
        topics: [
            MESSAGE_PASSED_TOPIC,
            toHex(w.nonce, { size: 32 }),
            padHex(w.sender, { size: 32 }),
            padHex(w.target, { size: 32 }),
        ],
        data: encodeAbiParameters(
            parseAbiParameters(
                "uint256 value, uint256 gasLimit, bytes data, bytes32 withdrawalHash",
            ),
            [w.value, w.gasLimit, w.data, w.withdrawalHash],
        ),
    };
}

export const SENT_MESSAGE_TOPIC: Hex = keccak256(
    toHex("SentMessage(address,address,bytes,uint256,uint256)"),
);
export const SENT_MESSAGE_EXTENSION_1_TOPIC: Hex = keccak256(
    toHex("SentMessageExtension1(address,uint256)"),
);

export function sentMessageLog(m: {
    target: Address;
    sender: Address;
    message: Hex;
    messageNonce: bigint;
    gasLimit: bigint;
}): { topics: Hex[]; data: Hex } {
    return {
        topics: [SENT_MESSAGE_TOPIC, padHex(m.target, { size: 32 })],
        data: encodeAbiParameters(
            parseAbiParameters(
                "address sender, bytes message, uint256 messageNonce, uint256 gasLimit",
            ),
            [m.sender, m.message, m.messageNonce, m.gasLimit],
        ),
    };
}

export function sentMessageExtension1Log(
    sender: Address,
    value: bigint,
): { topics: Hex[]; data: Hex } {
    return {
        topics: [SENT_MESSAGE_EXTENSION_1_TOPIC, padHex(sender, { size: 32 })],
        data: toHex(value, { size: 32 }),
    };
}

export const RELAYED_MESSAGE_TOPIC: Hex = keccak256(toHex("RelayedMessage(bytes32)"));
export const FAILED_RELAYED_MESSAGE_TOPIC: Hex = keccak256(toHex("FailedRelayedMessage(bytes32)"));

export function relayedMessageLog(msgHash: Hex, failed: boolean): { topics: Hex[]; data: Hex } {
    return {
        topics: [failed ? FAILED_RELAYED_MESSAGE_TOPIC : RELAYED_MESSAGE_TOPIC, msgHash],
        data: "0x",
    };
}

export const ERC20_BRIDGE_INITIATED_TOPIC: Hex = keccak256(
    toHex("ERC20BridgeInitiated(address,address,address,uint256,bytes)"),
);
export const ERC20_BRIDGE_FINALIZED_TOPIC: Hex = keccak256(
    toHex("ERC20BridgeFinalized(address,address,address,uint256,bytes)"),
);

export function erc20BridgeLog(
    topic: Hex,
    e: {
        localToken: Address;
        remoteToken: Address;
        from: Address;
        to: Address;
        amount: bigint;
        extraData: Hex;
    },
): { topics: Hex[]; data: Hex } {
    return {
        topics: [
            topic,
            padHex(e.localToken, { size: 32 }),
            padHex(e.remoteToken, { size: 32 }),
            padHex(e.from, { size: 32 }),
        ],
        data: encodeAbiParameters(
            parseAbiParameters("address to, uint256 amount, bytes extraData"),
            [e.to, e.amount, e.extraData],
        ),
    };
}
