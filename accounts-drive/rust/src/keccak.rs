//! Keccak-256 (Ethereum's, pre-FIPS 0x01 domain padding), provided by the
//! RustCrypto [`sha3`] crate — the de-facto standard implementation.
//! Correctness is pinned by the spec's test vectors below.

use sha3::{Digest, Keccak256};

/// Hashes the concatenation of the given byte slices with Ethereum's
/// Keccak-256 — the same function the Cartesi Machine's Merkle tree uses.
pub fn keccak256(parts: &[&[u8]]) -> [u8; 32] {
    let mut h = Keccak256::new();
    for part in parts {
        h.update(part);
    }
    h.finalize().into()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    /// Pins the hash to the spec's Appendix B vectors and the well-known
    /// empty-string digest.
    #[test]
    fn keccak_vectors() {
        assert_eq!(
            hex(&keccak256(&[])),
            "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
        );
        let seed = [0u8; 32];
        let bb = [0xbbu8; 20];
        assert_eq!(
            hex(&keccak256(&[&seed, &bb])),
            "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92"
        );
        // Multi-block absorption sanity: >136 bytes in several parts equals
        // the same bytes in one part.
        let long = vec![0x5au8; 500];
        let (a, b) = long.split_at(137);
        assert_eq!(keccak256(&[a, b]), keccak256(&[&long]));
    }
}
