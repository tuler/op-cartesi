package drive

// Keccak-256 (Ethereum's, pre-FIPS 0x01 padding), implemented here so the
// library has no dependencies. The permutation follows the public-domain
// tiny_sha3 structure; correctness is pinned by the spec's test vectors,
// which were generated with go-ethereum's keccak.

var keccakRndc = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

var keccakRotc = [24]uint{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
var keccakPiln = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}

func rotl64(x uint64, n uint) uint64 { return x<<n | x>>(64-n) }

func keccakF(st *[25]uint64) {
	var bc [5]uint64
	for round := 0; round < 24; round++ {
		for i := 0; i < 5; i++ {
			bc[i] = st[i] ^ st[i+5] ^ st[i+10] ^ st[i+15] ^ st[i+20]
		}
		for i := 0; i < 5; i++ {
			t := bc[(i+4)%5] ^ rotl64(bc[(i+1)%5], 1)
			for j := 0; j < 25; j += 5 {
				st[j+i] ^= t
			}
		}
		t := st[1]
		for i := 0; i < 24; i++ {
			j := keccakPiln[i]
			bc[0] = st[j]
			st[j] = rotl64(t, keccakRotc[i])
			t = bc[0]
		}
		for j := 0; j < 25; j += 5 {
			for i := 0; i < 5; i++ {
				bc[i] = st[j+i]
			}
			for i := 0; i < 5; i++ {
				st[j+i] ^= ^bc[(i+1)%5] & bc[(i+2)%5]
			}
		}
		st[0] ^= keccakRndc[round]
	}
}

const keccakRate = 136 // 1088-bit rate for 256-bit output

// Keccak256 hashes the concatenation of the given byte slices.
func Keccak256(data ...[]byte) [32]byte {
	var st [25]uint64
	var block [keccakRate]byte
	n := 0
	absorb := func() {
		for i := 0; i < keccakRate/8; i++ {
			var lane uint64
			for b := 0; b < 8; b++ {
				lane |= uint64(block[i*8+b]) << (8 * b)
			}
			st[i] ^= lane
		}
		keccakF(&st)
		block = [keccakRate]byte{}
		n = 0
	}
	for _, d := range data {
		for len(d) > 0 {
			c := copy(block[n:], d)
			n += c
			d = d[c:]
			if n == keccakRate {
				absorb()
			}
		}
	}
	block[n] ^= 0x01
	block[keccakRate-1] ^= 0x80
	absorb()
	var out [32]byte
	for i := 0; i < 4; i++ {
		for b := 0; b < 8; b++ {
			out[i*8+b] = byte(st[i] >> (8 * b))
		}
	}
	return out
}
