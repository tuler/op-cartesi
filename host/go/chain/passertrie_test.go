package chain

import (
	"bytes"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
)

// The empty passer trie is the empty Ethereum trie, and diverges from it the
// moment the outputs root slot is seeded — which is why genesis does not
// commit the empty trie root.
func TestPasserTrieEmpty(t *testing.T) {
	p := NewPasserTrie()
	if got, want := p.Root(), common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421"); got != want {
		t.Errorf("empty trie root %s, want the empty Ethereum trie root %s", got, want)
	}
	if err := p.SetOutputsRoot(NewOutputTree().Root()); err != nil {
		t.Fatal(err)
	}
	if p.Root() == common.HexToHash("0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421") {
		t.Error("seeding the outputs root slot did not change the root")
	}
}

// A proof from the trie must verify against its root under geth's own
// verifier, with the secure key and the RLP-encoded true value — the same
// algorithm OptimismPortal's SecureMerkleTrie implements in Solidity.
func TestPasserTrieProofVerifies(t *testing.T) {
	p := NewPasserTrie()
	if err := p.SetOutputsRoot(NewOutputTree().Root()); err != nil {
		t.Fatal(err)
	}
	var hashes []common.Hash
	for i := range 5 {
		h := crypto.Keccak256Hash(fmt.Appendf(nil, "withdrawal %d", i))
		hashes = append(hashes, h)
		if err := p.InsertWithdrawal(h); err != nil {
			t.Fatal(err)
		}
	}
	root := p.Root()
	for _, h := range hashes {
		slot, proof, err := p.ProveWithdrawal(h)
		if err != nil {
			t.Fatal(err)
		}
		db := rawdb.NewMemoryDatabase()
		for _, node := range proof {
			if err := db.Put(crypto.Keccak256(node), node); err != nil {
				t.Fatal(err)
			}
		}
		value, err := trie.VerifyProof(root, crypto.Keccak256(slot.Bytes()), db)
		if err != nil {
			t.Fatalf("proof for %s does not verify: %v", h, err)
		}
		// The verified value is the RLP encoding of true — the byte
		// SecureMerkleTrie.verifyInclusionProof is given as _value.
		if !bytes.Equal(value, []byte{0x01}) {
			t.Errorf("slot value %x, want 01", value)
		}
	}

	// The outputs root is provable from the same root.
	proof, err := p.Prove(OutputsRootSlot)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	for _, node := range proof {
		if err := db.Put(crypto.Keccak256(node), node); err != nil {
			t.Fatal(err)
		}
	}
	value, err := trie.VerifyProof(root, crypto.Keccak256(OutputsRootSlot.Bytes()), db)
	if err != nil {
		t.Fatalf("outputs root proof does not verify: %v", err)
	}
	got, err := p.SlotValue(OutputsRootSlot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(common.LeftPadBytes(got, 32), NewOutputTree().Root().Bytes()) {
		t.Errorf("outputs root slot holds %x", got)
	}
	if len(value) == 0 {
		t.Error("verified outputs root value is empty")
	}
}

// A missing withdrawal produces an exclusion proof: it verifies, and yields
// no value — which is exactly what makes forging a withdrawal impossible
// without forging the root.
func TestPasserTrieExclusion(t *testing.T) {
	p := NewPasserTrie()
	if err := p.SetOutputsRoot(NewOutputTree().Root()); err != nil {
		t.Fatal(err)
	}
	if err := p.InsertWithdrawal(crypto.Keccak256Hash([]byte("real"))); err != nil {
		t.Fatal(err)
	}
	slot, proof, err := p.ProveWithdrawal(crypto.Keccak256Hash([]byte("forged")))
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	for _, node := range proof {
		if err := db.Put(crypto.Keccak256(node), node); err != nil {
			t.Fatal(err)
		}
	}
	value, err := trie.VerifyProof(p.Root(), crypto.Keccak256(slot.Bytes()), db)
	if err != nil {
		t.Fatalf("exclusion proof does not verify: %v", err)
	}
	if value != nil {
		t.Errorf("absent slot verified to value %x", value)
	}
}

// Copies diverge without corrupting each other — the property forks and
// reorgs depend on.
func TestPasserTrieCopyDiverges(t *testing.T) {
	base := NewPasserTrie()
	if err := base.SetOutputsRoot(NewOutputTree().Root()); err != nil {
		t.Fatal(err)
	}
	if err := base.InsertWithdrawal(crypto.Keccak256Hash([]byte("shared"))); err != nil {
		t.Fatal(err)
	}
	baseRoot := base.Root()

	a, b := base.Copy(), base.Copy()
	if err := a.InsertWithdrawal(crypto.Keccak256Hash([]byte("a"))); err != nil {
		t.Fatal(err)
	}
	if err := b.InsertWithdrawal(crypto.Keccak256Hash([]byte("b"))); err != nil {
		t.Fatal(err)
	}
	if a.Root() == b.Root() {
		t.Error("divergent copies reached the same root")
	}
	if base.Root() != baseRoot {
		t.Error("updating copies changed the original")
	}
}

// The withdrawal hash is Hashing.hashWithdrawal: keccak256 of the ABI
// encoding of the struct fields. Pinned against a hand-computed vector so a
// drift in the encoding shows up here rather than on L1.
func TestWithdrawalHash(t *testing.T) {
	w := &Withdrawal{
		Nonce:    new(big.Int).SetUint64(0),
		Sender:   common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Target:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:    big.NewInt(1),
		GasLimit: big.NewInt(100000),
		Data:     nil,
	}
	got, err := w.Hash()
	if err != nil {
		t.Fatal(err)
	}
	// abi.encode(0, 0x11…, 0x22…, 1, 100000, "") laid out by hand: five head
	// words, the offset word 0xc0, and the empty bytes' length word.
	var packed []byte
	word := func(v *big.Int) []byte { return common.BigToHash(v).Bytes() }
	packed = append(packed, word(big.NewInt(0))...)
	packed = append(packed, common.LeftPadBytes(w.Sender.Bytes(), 32)...)
	packed = append(packed, common.LeftPadBytes(w.Target.Bytes(), 32)...)
	packed = append(packed, word(big.NewInt(1))...)
	packed = append(packed, word(big.NewInt(100000))...)
	packed = append(packed, word(big.NewInt(0xc0))...)
	packed = append(packed, word(big.NewInt(0))...)
	if want := crypto.Keccak256Hash(packed); got != want {
		t.Errorf("withdrawal hash %s, want %s", got, want)
	}
}

// ParseWithdrawalOutput accepts exactly the guest's encoding and nothing
// else.
func TestParseWithdrawalOutput(t *testing.T) {
	w := &Withdrawal{
		Nonce:    new(big.Int).Lsh(big.NewInt(1), 240), // version 1, counter 0
		Sender:   common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Target:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:    big.NewInt(7),
		GasLimit: big.NewInt(100000),
		Data:     []byte{0xde, 0xad},
	}
	encoded := encodeWithdrawalOutput(t, w)
	parsed, ok := ParseWithdrawalOutput(encoded)
	if !ok {
		t.Fatal("round-tripped withdrawal did not parse")
	}
	if parsed.Nonce.Cmp(w.Nonce) != 0 || parsed.Sender != w.Sender || parsed.Target != w.Target ||
		parsed.Value.Cmp(w.Value) != 0 || parsed.GasLimit.Cmp(w.GasLimit) != 0 || !bytes.Equal(parsed.Data, w.Data) {
		t.Errorf("parsed %+v, want %+v", parsed, w)
	}

	if _, ok := ParseWithdrawalOutput([]byte("not an output")); ok {
		t.Error("garbage parsed as a withdrawal")
	}
	// A plain notice is not a withdrawal.
	plain, err := noticeArgs.Pack([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ParseWithdrawalOutput(append(append([]byte{}, noticeSelector...), plain...)); ok {
		t.Error("a plain notice parsed as a withdrawal")
	}
	// A bare Withdrawal encoding without the Notice wrapper is not one
	// either: outputs on the wire are always Cartesi output calls.
	if _, ok := ParseWithdrawalOutput(encoded[4+64:]); ok {
		t.Error("an unwrapped payload parsed as a withdrawal")
	}
}

// encodeWithdrawalOutput is EncodeOutput with test-fatal error handling.
func encodeWithdrawalOutput(t *testing.T, w *Withdrawal) []byte {
	t.Helper()
	encoded, err := w.EncodeOutput()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The roots PasserTrieVectors.t.sol hardcodes — the two-withdrawal trie and
// the single-slot fixture — used to be reasserted here so that both suites
// broke together. They now live in conformance/commitments/passer-trie.json,
// where TestConformancePasserTrie asserts them before writing and checks
// every proof in the file with geth's own verifier, which is the algorithm
// SecureMerkleTrie implements. The Solidity suite still carries its copies;
// repointing it at the file is noted in conformance/README.md.
