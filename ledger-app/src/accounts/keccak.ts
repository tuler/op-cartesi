// Keccak-256 (Ethereum's, pre-FIPS 0x01 domain padding) via @noble/hashes —
// the audited, pure-JS de-facto standard (the implementation under ethers and
// viem). Correctness is still pinned by the spec's test vectors, which were
// generated independently.
import { keccak_256 } from "@noble/hashes/sha3.js";

// keccak256 hashes the concatenation of the given byte chunks.
export function keccak256(...data: Uint8Array[]): Uint8Array {
  const h = keccak_256.create();
  for (const d of data) h.update(d);
  return h.digest();
}
