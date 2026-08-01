// Keccak-256 (Ethereum's, pre-FIPS 0x01 domain padding), implemented here so
// the library has no dependencies. Lanes are BigInts; the permutation follows
// the public-domain tiny_sha3 structure, matching the Go reference
// implementation. Correctness is pinned by the spec's test vectors.

const MASK = (1n << 64n) - 1n;

const RNDC = [
  0x0000000000000001n, 0x0000000000008082n, 0x800000000000808an, 0x8000000080008000n,
  0x000000000000808bn, 0x0000000080000001n, 0x8000000080008081n, 0x8000000000008009n,
  0x000000000000008an, 0x0000000000000088n, 0x0000000080008009n, 0x000000008000000an,
  0x000000008000808bn, 0x800000000000008bn, 0x8000000000008089n, 0x8000000000008003n,
  0x8000000000008002n, 0x8000000000000080n, 0x000000000000800an, 0x800000008000000an,
  0x8000000080008081n, 0x8000000000008080n, 0x0000000080000001n, 0x8000000080008008n,
];

const ROTC = [1n, 3n, 6n, 10n, 15n, 21n, 28n, 36n, 45n, 55n, 2n, 14n, 27n, 41n, 56n, 8n, 25n, 43n, 62n, 18n, 39n, 61n, 20n, 44n];
const PILN = [10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1];

function rotl64(x, n) {
  return ((x << n) | (x >> (64n - n))) & MASK;
}

function keccakF(st) {
  const bc = new Array(5);
  for (let round = 0; round < 24; round++) {
    for (let i = 0; i < 5; i++) {
      bc[i] = st[i] ^ st[i + 5] ^ st[i + 10] ^ st[i + 15] ^ st[i + 20];
    }
    for (let i = 0; i < 5; i++) {
      const t = bc[(i + 4) % 5] ^ rotl64(bc[(i + 1) % 5], 1n);
      for (let j = 0; j < 25; j += 5) st[j + i] ^= t;
    }
    let t = st[1];
    for (let i = 0; i < 24; i++) {
      const j = PILN[i];
      const tmp = st[j];
      st[j] = rotl64(t, ROTC[i]);
      t = tmp;
    }
    for (let j = 0; j < 25; j += 5) {
      for (let i = 0; i < 5; i++) bc[i] = st[j + i];
      for (let i = 0; i < 5; i++) st[j + i] ^= ~bc[(i + 1) % 5] & bc[(i + 2) % 5] & MASK;
    }
    st[0] ^= RNDC[round];
  }
}

const RATE = 136; // 1088-bit rate for 256-bit output

/**
 * keccak256 hashes the concatenation of the given Uint8Array chunks and
 * returns a 32-byte Uint8Array.
 * @param {...Uint8Array} data
 * @returns {Uint8Array}
 */
export function keccak256(...data) {
  const st = new Array(25).fill(0n);
  const block = new Uint8Array(RATE);
  const view = new DataView(block.buffer);
  let n = 0;
  const absorb = () => {
    for (let i = 0; i < RATE / 8; i++) {
      st[i] ^= view.getBigUint64(i * 8, true);
    }
    keccakF(st);
    block.fill(0);
    n = 0;
  };
  for (const d of data) {
    let off = 0;
    while (off < d.length) {
      const c = Math.min(RATE - n, d.length - off);
      block.set(d.subarray(off, off + c), n);
      n += c;
      off += c;
      if (n === RATE) absorb();
    }
  }
  block[n] ^= 0x01;
  block[RATE - 1] ^= 0x80;
  absorb();
  const out = new Uint8Array(32);
  const ov = new DataView(out.buffer);
  for (let i = 0; i < 4; i++) ov.setBigUint64(i * 8, st[i], true);
  return out;
}
