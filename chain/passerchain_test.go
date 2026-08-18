package chain

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// testWithdrawal is the withdrawal the emitting mock produces: an ether
// payment to a fixed recipient, the way the guest bridge emits one.
func testWithdrawal() *Withdrawal {
	return &Withdrawal{
		Nonce:    new(big.Int).Lsh(big.NewInt(1), 240),
		Sender:   common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Target:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:    big.NewInt(1e15),
		GasLimit: big.NewInt(100000),
		Data:     nil,
	}
}

// emitWithdrawalOn makes the mock emit one withdrawal notice per input.
func emitWithdrawalOn(t *testing.T, w *Withdrawal) func([]byte) []machine.Output {
	encoded := encodeWithdrawalOutput(t, w)
	return func([]byte) []machine.Output {
		return []machine.Output{{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: encoded}}
	}
}

// A withdrawal the machine emits must land in the block's withdrawal
// commitment as the exact sentMessages storage slot OptimismPortal proves,
// with the block's outputs root alongside — the full path from a guest
// emission to something viem's buildProveWithdrawal can consume.
func TestWithdrawalEntersPasserTrie(t *testing.T) {
	w := testWithdrawal()
	m := machine.NewMock([]byte("seed"))
	m.OutputFn = emitWithdrawalOn(t, w)
	c, err := New(context.Background(), testConfig(), m, mempool.New(8))
	if err != nil {
		t.Fatal(err)
	}
	env := buildBlock(c, t, isthmusAttrs(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))
	root := *env.ExecutionPayload.WithdrawalsRoot

	wHash, err := w.Hash()
	if err != nil {
		t.Fatal(err)
	}
	slot := withdrawalSlot(wHash)
	proofs, ok, err := c.PasserProofAt(env.ExecutionPayload.BlockHash, []common.Hash{slot, OutputsRootSlot})
	if err != nil || !ok {
		t.Fatalf("no proofs for the block: ok=%v err=%v", ok, err)
	}

	verify := func(p StorageProof) []byte {
		db := rawdb.NewMemoryDatabase()
		for _, node := range p.Proof {
			if err := db.Put(crypto.Keccak256(node), node); err != nil {
				t.Fatal(err)
			}
		}
		value, err := trie.VerifyProof(root, crypto.Keccak256(p.Slot.Bytes()), db)
		if err != nil {
			t.Fatalf("proof for slot %s does not verify against the header root: %v", p.Slot, err)
		}
		return value
	}
	if value := verify(proofs[0]); string(value) != "\x01" {
		t.Errorf("sentMessages slot verifies to %x, want 01", value)
	}
	tree, _ := c.OutputTreeAt(env.ExecutionPayload.BlockHash)
	outputsValue := verify(proofs[1])
	if common.BytesToHash(mustRLPDecodeBytes(t, nil, outputsValue)) != tree.Root() {
		t.Errorf("outputs root slot verifies to %x, want %s", outputsValue, tree.Root())
	}

	// A verifier re-executing the payload reaches the same commitment.
	vm := machine.NewMock([]byte("seed"))
	vm.OutputFn = emitWithdrawalOn(t, w)
	verifier, err := New(context.Background(), testConfig(), vm, mempool.New(8))
	if err != nil {
		t.Fatal(err)
	}
	st, err := verifier.ImportPayload(context.Background(), env.ExecutionPayload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("verifier rejected the withdrawal block: %s (%v)", st.Status, st.ValidationError)
	}
}

// mustRLPDecodeBytes strips the storage value's RLP framing. VerifyProof
// returns the raw trie value, which for storage is an RLP byte string.
func mustRLPDecodeBytes(t *testing.T, _ []byte, encoded []byte) []byte {
	t.Helper()
	// Single byte below 0x80 is its own encoding.
	if len(encoded) == 1 && encoded[0] < 0x80 {
		return encoded
	}
	if len(encoded) >= 1 && encoded[0] >= 0x80 && encoded[0] <= 0xb7 {
		return encoded[1:]
	}
	t.Fatalf("unexpected storage value encoding %x", encoded)
	return nil
}

// A restart must rebuild the withdrawal commitment from stored outputs and
// keep every withdrawal provable — including ones emitted before the
// checkpoint the restore replays from.
func TestRestoreRebuildsWithdrawals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := testWithdrawal()

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := machine.NewMock([]byte("withdrawals"))
	m.OutputFn = emitWithdrawalOn(t, w)
	cfg := persistConfig()
	c, err := New(ctx, cfg, m, mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ctx, store); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i)))}, true))
	}
	head := c.HeadBlock()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, ok, err := Restore(ctx, cfg, reopened,
		func(_ context.Context, dir string) (machine.Machine, error) {
			mm, err := machine.LoadMock(dir)
			if err != nil {
				return nil, err
			}
			mm.OutputFn = emitWithdrawalOn(t, w)
			return mm, nil
		}, mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the store reported itself empty")
	}
	if got := restored.HeadBlock(); got.Hash() != head.Hash() {
		t.Fatalf("restored head %s, want %s", got.Hash(), head.Hash())
	}

	// The withdrawal emitted in block 1 — before the checkpoint — must still
	// prove against the restored head's commitment.
	wHash, err := w.Hash()
	if err != nil {
		t.Fatal(err)
	}
	proofs, ok, err := restored.PasserProofAt(head.Hash(), []common.Hash{withdrawalSlot(wHash)})
	if err != nil || !ok {
		t.Fatalf("no proof after restore: ok=%v err=%v", ok, err)
	}
	db := rawdb.NewMemoryDatabase()
	for _, node := range proofs[0].Proof {
		if err := db.Put(crypto.Keccak256(node), node); err != nil {
			t.Fatal(err)
		}
	}
	value, err := trie.VerifyProof(*head.Header.WithdrawalsHash, crypto.Keccak256(proofs[0].Slot.Bytes()), db)
	if err != nil {
		t.Fatalf("restored proof does not verify: %v", err)
	}
	if string(value) != "\x01" {
		t.Errorf("restored slot value %x, want 01", value)
	}
}
