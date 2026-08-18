package mempool

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var testChainID = big.NewInt(901)

// signedTx builds a raw EIP-155 legacy transaction for the gating tests.
// gasPrice differentiates otherwise-identical transactions, so two
// transactions can share a (sender, nonce) without sharing a hash.
func signedTx(t *testing.T, keySeed byte, nonce uint64, gasPrice int64) []byte {
	t.Helper()
	key, err := crypto.ToECDSA(common.LeftPadBytes([]byte{keySeed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	to := common.Address{}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(testChainID), &types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(gasPrice),
		Gas:      1_000_000,
		To:       &to,
		Data:     []byte("payload"),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func unsignedTx(t *testing.T) []byte {
	t.Helper()
	to := common.Address{}
	raw, err := types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &to}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Without a gate the pool keeps its original behavior: anything that decodes
// as a non-deposit transaction queues, signed or not. This is what keeps the
// mock-machine paths (and every pre-gating test in this repo) working.
func TestUngatedPoolAcceptsUnsigned(t *testing.T) {
	p := New(4)
	if _, err := p.Add(unsignedTx(t)); err != nil {
		t.Fatalf("ungated pool rejected an unsigned tx: %v", err)
	}
	if p.Len() != 1 {
		t.Fatalf("pool length %d, want 1", p.Len())
	}
}

// The gate is a courtesy filter in front of the guest's enforcement: stale
// nonces bounce at ingress, current and future nonces queue (a future nonce
// may become valid before inclusion; the guest re-checks either way).
func TestNonceGating(t *testing.T) {
	p := New(16)
	nonces := map[common.Address]uint64{}
	p.SetNonceGate(testChainID, func(addr common.Address) (uint64, error) {
		return nonces[addr], nil
	})

	key1, _ := crypto.ToECDSA(common.LeftPadBytes([]byte{1}, 32))
	alice := crypto.PubkeyToAddress(key1.PublicKey)
	nonces[alice] = 5

	if _, err := p.Add(signedTx(t, 1, 4, 1)); !errors.Is(err, ErrNonceTooLow) {
		t.Fatalf("stale nonce: got %v, want ErrNonceTooLow", err)
	}
	if _, err := p.Add(signedTx(t, 1, 5, 1)); err != nil {
		t.Fatalf("current nonce rejected: %v", err)
	}
	if _, err := p.Add(signedTx(t, 1, 7, 1)); err != nil {
		t.Fatalf("future nonce rejected: %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("pool length %d, want 2", p.Len())
	}
}

// Two different transactions competing for the same (sender, nonce) must not
// both queue — only one could ever execute. The newcomer replaces the queued
// one in place, so a sender whose first attempt lingers in the pool (for any
// reason) is never locked out of the slot they own.
func TestSameNonceReplaces(t *testing.T) {
	p := New(16)
	p.SetNonceGate(testChainID, func(common.Address) (uint64, error) { return 0, nil })

	first := signedTx(t, 1, 0, 1)
	oldHash, err := p.Add(first)
	if err != nil {
		t.Fatal(err)
	}
	second := signedTx(t, 1, 0, 2)
	newHash, err := p.Add(second)
	if err != nil {
		t.Fatalf("replacement rejected: %v", err)
	}
	if newHash == oldHash {
		t.Fatal("replacement returned the old hash")
	}
	if p.Len() != 1 {
		t.Fatalf("pool length %d after replacement, want 1", p.Len())
	}
	if _, ok := p.Get(oldHash); ok {
		t.Fatal("the replaced transaction is still queued")
	}
	if raw, ok := p.Get(newHash); !ok {
		t.Fatal("the replacement is not queued")
	} else if string(raw) != string(second) {
		t.Fatal("the replacement's raw bytes do not match")
	}

	// A different sender at the same nonce is its own slot.
	if _, err := p.Add(signedTx(t, 2, 0, 1)); err != nil {
		t.Fatalf("other sender, same nonce: %v", err)
	}
	if p.Len() != 2 {
		t.Fatalf("pool length %d, want 2", p.Len())
	}

	// Forgetting the replacement (included or invalid) frees the slot for a
	// fresh Add rather than another replacement.
	p.Forget([]common.Hash{newHash})
	if _, err := p.Add(signedTx(t, 1, 0, 3)); err != nil {
		t.Fatalf("slot not freed after Forget: %v", err)
	}
}

// The exact resend: the same raw transaction twice is still ErrKnownTx, not
// a replacement — nothing would change, and the caller should know.
func TestExactResendStillKnown(t *testing.T) {
	p := New(16)
	p.SetNonceGate(testChainID, func(common.Address) (uint64, error) { return 0, nil })
	raw := signedTx(t, 1, 0, 1)
	if _, err := p.Add(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Add(raw); !errors.Is(err, ErrKnownTx) {
		t.Fatalf("exact resend: got %v, want ErrKnownTx", err)
	}
}

// A gated pool must refuse what the guest would refuse for the same reason:
// no recoverable sender under this chain's signer.
func TestGateRejectsUnrecoverableSenders(t *testing.T) {
	p := New(16)
	p.SetNonceGate(testChainID, func(common.Address) (uint64, error) { return 0, nil })

	if _, err := p.Add(unsignedTx(t)); !errors.Is(err, ErrInvalidSender) {
		t.Fatalf("unsigned tx: got %v, want ErrInvalidSender", err)
	}

	// Signed, but for another chain: the chain-id signer must refuse it
	// rather than recover a wrong sender.
	key, _ := crypto.ToECDSA(common.LeftPadBytes([]byte{3}, 32))
	to := common.Address{}
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(902)), &types.LegacyTx{
		Nonce: 0, GasPrice: big.NewInt(1), Gas: 21000, To: &to,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Add(raw); !errors.Is(err, ErrInvalidSender) {
		t.Fatalf("wrong-chain tx: got %v, want ErrInvalidSender", err)
	}
}

// A hook failure must surface, not admit the transaction unchecked.
func TestGateSurfacesHookErrors(t *testing.T) {
	p := New(16)
	boom := errors.New("machine unreachable")
	p.SetNonceGate(testChainID, func(common.Address) (uint64, error) { return 0, boom })
	if _, err := p.Add(signedTx(t, 1, 0, 1)); !errors.Is(err, boom) {
		t.Fatalf("hook error swallowed: %v", err)
	}
	if p.Len() != 0 {
		t.Fatal("transaction queued despite hook failure")
	}
}
