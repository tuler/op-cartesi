package drive

import "golang.org/x/crypto/sha3"

// Keccak256 hashes the concatenation of the given byte slices with the
// legacy (pre-FIPS, 0x01-padded) Keccak-256 — the hash of the machine's
// Merkle tree and of Ethereum.
//
// golang.org/x/crypto is the de-facto standard implementation; go-ethereum's
// own hashing is built on the same code. Correctness here is still pinned by
// the spec's test vectors, which were generated independently.
func Keccak256(data ...[]byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	for _, d := range data {
		h.Write(d)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}
