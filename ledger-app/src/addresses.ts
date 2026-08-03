// The address map of EVM-COMPAT §6.

import { concat, getAddress, keccak256, stringToBytes, toBytes, type Address, type Hex } from "viem";

/** The system namespace: 0xC751 (leet CTSI) ‖ 16 zero bytes ‖ uint16 index —
 * 65,536 slots, mirroring OP's 0x4200…xxxx shape. The reservation is the
 * FULL pattern: the 16-byte zero run is the forgery barrier (grinding an
 * address into it is ~2^144), the two brand bytes are legibility only.
 * Nothing may treat a bare 0xC751 prefix match as authority or as a badge —
 * authority is exact membership in the manifest. */
export const REGISTRY_ADDRESS: Address = "0xc751000000000000000000000000000000000000";
export const BRIDGE_ADDRESS: Address = "0xc751000000000000000000000000000000000001";
export const CONFIG_ADDRESS: Address = "0xc751000000000000000000000000000000000002";

/** The one adopted OP predeploy: L1Block, which already receives the
 * attributes deposit every block (EVM-COMPAT §6). */
export const L1BLOCK_ADDRESS: Address = "0x4200000000000000000000000000000000000015";

/** Domain tag of the token façade derivation. */
export const TOKEN_DOMAIN = "ctsi.erc20.v1";

/** l2TokenAddress derives a registered token's façade address:
 * last20(keccak256("ctsi.erc20.v1" ‖ l1Token)). Computable before the first
 * deposit, and confined to a namespace no registration can steer into the
 * manifest (EVM-COMPAT §6). */
export function l2TokenAddress(l1Token: Address): Address {
    const hash = keccak256(concat([stringToBytes(TOKEN_DOMAIN), toBytes(l1Token)]));
    return getAddress(`0x${hash.slice(-40)}`);
}

/** OP's L1-to-L2 sender aliasing: alias(a) = a + 0x1111…1111 mod 2^160.
 * The guest registers plain L1 portal addresses and aliases them itself,
 * the same choice the Lua app made. */
const ALIAS_OFFSET = BigInt("0x1111000000000000000000000000000000001111");
const TWO_160 = 1n << 160n;

export function applyL1ToL2Alias(l1: Address): Address {
    const aliased = (BigInt(l1) + ALIAS_OFFSET) % TWO_160;
    return getAddress(`0x${aliased.toString(16).padStart(40, "0")}`);
}

/** Case-insensitive address equality (viem addresses may be checksummed). */
export function sameAddress(a: Address, b: Address): boolean {
    return a.toLowerCase() === b.toLowerCase();
}

/** Lowercased key form, for Map keys. */
export function addrKey(a: Address): Address {
    return a.toLowerCase() as Address;
}

export type { Address, Hex };
