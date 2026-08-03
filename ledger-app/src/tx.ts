// Transaction parsing and admission (EVM-COMPAT §5).
//
// viem's op-stack parseTransaction handles the whole table: OP deposits
// (0x7e), legacy RLP, EIP-2930 and EIP-1559 typed envelopes. What this module
// adds is the router's admission policy: which types are allowed, the chain
// id pin, the low-s rule, and sender recovery from the signature — never from
// the envelope's msgSender.

import { recoverTransactionAddress, toHex, type Address, type Hex, type TransactionSerialized } from "viem";
import { parseTransaction } from "viem/op-stack";

/** EIP-2's upper bound on s (secp256k1 n ÷ 2): the twin signature with the
 * same r would give every transaction two encodings. */
const HALF_N = BigInt("0x7fffffffffffffffffffffffffffffff5d576e7357a4501ddfe92f46681b20a0");

export type ParsedInput =
    | {
          kind: "deposit";
          from: Address;
          to: Address | undefined;
          mint: bigint;
          value: bigint;
          data: Uint8Array;
      }
    | {
          kind: "signed";
          type: "legacy" | "eip2930" | "eip1559";
          chainId: bigint | undefined;
          nonce: bigint;
          to: Address | undefined;
          value: bigint;
          data: Uint8Array;
          /** The signature's s, kept from parse time for the low-s rule. */
          s: Hex;
          raw: TransactionSerialized;
      }
    | {
          /** Not a transaction at all. Record-and-accept: a malformed input
           * must never halt the machine, and rejecting it would roll back a
           * message a later guest might understand. */
          kind: "opaque";
      }
    | {
          /** A transaction the chain refuses: blob, set-code, unsigned, or a
           * shape viem knows that this chain does not admit. */
          kind: "refused";
          reason: string;
      };

function dataBytes(data: Hex | undefined): Uint8Array {
    if (!data || data === "0x") return new Uint8Array(0);
    return Buffer.from(data.slice(2), "hex");
}

/** viem brands its serialized-transaction types, so the brand has to be
 * earned rather than asserted: this guard performs the structural check the
 * brand stands for — a legacy transaction is an RLP list (first byte
 * 0xc0–0xff), a typed one carries its envelope byte. Callers only reach it
 * for bytes parseTransaction already vouched for. */
function isSerializedTransaction(value: Hex): value is TransactionSerialized {
    const first = Number.parseInt(value.slice(2, 4), 16);
    return Number.isInteger(first) && (first >= 0xc0 || (first >= 0x01 && first <= 0x04));
}

/** The fields this router reads, across every shape op-stack parseTransaction
 * returns. An explicit assignment target (checked by the compiler, unlike a
 * cast) because the generic's conditional return type collapses on a
 * non-literal Hex and loses the deposit arm. */
interface AnyParsedTx {
    type?: string;
    from?: Address;
    to?: Address | null;
    mint?: bigint;
    value?: bigint;
    data?: Hex;
    chainId?: number;
    nonce?: number;
    r?: Hex;
    s?: Hex;
}

export function parseInput(raw: Uint8Array): ParsedInput {
    const hex = toHex(raw);
    let tx: AnyParsedTx;
    try {
        tx = parseTransaction(hex);
    } catch {
        return { kind: "opaque" };
    }
    if (tx.type === "deposit") {
        if (!tx.from) return { kind: "opaque" }; // no sender, nothing to credit or authenticate
        return {
            kind: "deposit",
            from: tx.from,
            to: tx.to ?? undefined,
            mint: tx.mint ?? 0n,
            value: tx.value ?? 0n,
            data: dataBytes(tx.data),
        };
    }
    if (tx.type !== "legacy" && tx.type !== "eip2930" && tx.type !== "eip1559") {
        return { kind: "refused", reason: `transaction type ${tx.type} not admitted` };
    }
    if (tx.r === undefined || tx.s === undefined) {
        // Parsed but unsigned: carries no verifiable nonce, so recording it
        // unenforced would exempt it from replay protection. Same doctrine
        // as the Lua guest: opaque bytes record-and-accept.
        return { kind: "opaque" };
    }
    if (!isSerializedTransaction(hex)) return { kind: "opaque" }; // unreachable: parseTransaction vouched
    return {
        kind: "signed",
        type: tx.type,
        chainId: tx.chainId !== undefined ? BigInt(tx.chainId) : undefined,
        nonce: BigInt(tx.nonce ?? 0),
        to: tx.to ?? undefined,
        value: tx.value ?? 0n,
        data: dataBytes(tx.data),
        s: tx.s,
        raw: hex,
    };
}

/** Recovers the signer, enforcing low-s first. Returns null for anything
 * that does not verify. */
export async function recoverSender(parsed: Extract<ParsedInput, { kind: "signed" }>): Promise<Address | null> {
    if (BigInt(parsed.s) > HALF_N) return null;
    try {
        return await recoverTransactionAddress({ serializedTransaction: parsed.raw });
    } catch {
        return null;
    }
}
