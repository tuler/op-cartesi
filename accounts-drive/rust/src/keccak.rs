//! Keccak-256 (Ethereum's, pre-FIPS 0x01 domain padding), implemented here so
//! the crate has no dependencies. The permutation follows the public-domain
//! tiny_sha3 structure; correctness is pinned by the spec's test vectors.

const RNDC: [u64; 24] = [
    0x0000000000000001,
    0x0000000000008082,
    0x800000000000808a,
    0x8000000080008000,
    0x000000000000808b,
    0x0000000080000001,
    0x8000000080008081,
    0x8000000000008009,
    0x000000000000008a,
    0x0000000000000088,
    0x0000000080008009,
    0x000000008000000a,
    0x000000008000808b,
    0x800000000000008b,
    0x8000000000008089,
    0x8000000000008003,
    0x8000000000008002,
    0x8000000000000080,
    0x000000000000800a,
    0x800000008000000a,
    0x8000000080008081,
    0x8000000000008080,
    0x0000000080000001,
    0x8000000080008008,
];

const ROTC: [u32; 24] = [
    1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44,
];

const PILN: [usize; 24] = [
    10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1,
];

fn keccak_f(st: &mut [u64; 25]) {
    let mut bc = [0u64; 5];
    for round in 0..24 {
        // Theta.
        for i in 0..5 {
            bc[i] = st[i] ^ st[i + 5] ^ st[i + 10] ^ st[i + 15] ^ st[i + 20];
        }
        for i in 0..5 {
            let t = bc[(i + 4) % 5] ^ bc[(i + 1) % 5].rotate_left(1);
            for j in (0..25).step_by(5) {
                st[j + i] ^= t;
            }
        }
        // Rho and pi.
        let mut t = st[1];
        for i in 0..24 {
            let j = PILN[i];
            let tmp = st[j];
            st[j] = t.rotate_left(ROTC[i]);
            t = tmp;
        }
        // Chi.
        for j in (0..25).step_by(5) {
            for i in 0..5 {
                bc[i] = st[j + i];
            }
            for i in 0..5 {
                st[j + i] ^= !bc[(i + 1) % 5] & bc[(i + 2) % 5];
            }
        }
        // Iota.
        st[0] ^= RNDC[round];
    }
}

/// Sponge rate in bytes: 1088-bit rate for a 256-bit output.
const RATE: usize = 136;

/// Hashes the concatenation of the given byte slices with Ethereum's
/// Keccak-256 — the same function the Cartesi Machine's Merkle tree uses.
pub fn keccak256(parts: &[&[u8]]) -> [u8; 32] {
    let mut st = [0u64; 25];
    let mut block = [0u8; RATE];
    let mut n = 0usize;

    fn absorb(st: &mut [u64; 25], block: &[u8; RATE]) {
        for i in 0..RATE / 8 {
            let mut lane = [0u8; 8];
            lane.copy_from_slice(&block[i * 8..i * 8 + 8]);
            st[i] ^= u64::from_le_bytes(lane);
        }
        keccak_f(st);
    }

    for part in parts {
        let mut d: &[u8] = part;
        while !d.is_empty() {
            let c = (RATE - n).min(d.len());
            block[n..n + c].copy_from_slice(&d[..c]);
            n += c;
            d = &d[c..];
            if n == RATE {
                absorb(&mut st, &block);
                block = [0u8; RATE];
                n = 0;
            }
        }
    }
    // Ethereum's pre-FIPS padding: 0x01 … 0x80 (multi-rate pad within one block).
    block[n] ^= 0x01;
    block[RATE - 1] ^= 0x80;
    absorb(&mut st, &block);

    let mut out = [0u8; 32];
    for i in 0..4 {
        out[i * 8..i * 8 + 8].copy_from_slice(&st[i].to_le_bytes());
    }
    out
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
