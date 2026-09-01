package engineapi

import (
	"context"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

// newWithdrawingServer is newPublicServer with a mock that emits one
// withdrawal per input, so eth_getProof has something to prove.
func newWithdrawingServer(t *testing.T, w *chain.Withdrawal) (*rpcClient, *chain.Chain) {
	t.Helper()
	encoded, err := w.EncodeOutput()
	if err != nil {
		t.Fatal(err)
	}
	m := machine.NewMock([]byte("proof-api-test"))
	m.OutputFn = func([]byte) []machine.Output {
		return []machine.Output{{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: encoded}}
	}
	pool := mempool.New(64)
	c, err := chain.New(context.Background(), chain.Config{ChainID: 901, GenesisTimestamp: 1000}, m, pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(c, pool, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &rpcClient{t: t, url: srv.URL}, c
}

// eth_getProof must serve what viem's buildProveWithdrawal reads: a
// storageHash equal to the header's withdrawalsRoot, and a storage proof for
// the withdrawal's sentMessages slot that verifies against it.
func TestEthGetProofServesWithdrawalProof(t *testing.T) {
	w := &chain.Withdrawal{
		Nonce:    new(big.Int).Lsh(big.NewInt(1), 240),
		Sender:   common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Target:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:    big.NewInt(5),
		GasLimit: big.NewInt(100000),
	}
	client, c := newWithdrawingServer(t, w)
	payload := sequenceOneBlock(t, c)

	wHash, err := w.Hash()
	if err != nil {
		t.Fatal(err)
	}
	// The slot viem computes: keccak256(abi.encode(withdrawalHash, 0)).
	slot := crypto.Keccak256Hash(wHash.Bytes(), common.Hash{}.Bytes())

	var res AccountResult
	client.call("eth_getProof", &res, chain.MessagePasserAddress, []string{slot.Hex()}, "latest")

	if res.StorageHash != *payload.WithdrawalsRoot {
		t.Fatalf("storageHash %s, want the header's withdrawalsRoot %s", res.StorageHash, *payload.WithdrawalsRoot)
	}
	if len(res.AccountProof) != 0 {
		t.Errorf("account proof has %d nodes, want none — there is no account trie", len(res.AccountProof))
	}
	if len(res.StorageProof) != 1 {
		t.Fatalf("storage proof count %d, want 1", len(res.StorageProof))
	}
	if got := res.StorageProof[0].Value.ToInt(); got.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("slot value %v, want 1", got)
	}

	// The served proof verifies against the served root with geth's own
	// verifier — the algorithm the portal's SecureMerkleTrie implements.
	db := rawdb.NewMemoryDatabase()
	for _, node := range res.StorageProof[0].Proof {
		if err := db.Put(crypto.Keccak256(node), node); err != nil {
			t.Fatal(err)
		}
	}
	value, err := trie.VerifyProof(res.StorageHash, crypto.Keccak256(slot.Bytes()), db)
	if err != nil {
		t.Fatalf("served proof does not verify: %v", err)
	}
	if string(value) != "\x01" {
		t.Errorf("verified value %x, want 01", value)
	}
}

// Every other address is refused, pointing at the cartesi_ replacement.
func TestEthGetProofRefusesOtherAddresses(t *testing.T) {
	client, c := newWithdrawingServer(t, &chain.Withdrawal{
		Nonce: big.NewInt(0), Value: big.NewInt(0), GasLimit: big.NewInt(1),
	})
	sequenceOneBlock(t, c)

	rpcErr := client.callError("eth_getProof", common.HexToAddress("0x42"), []string{}, "latest")
	if rpcErr == nil {
		t.Fatal("eth_getProof answered for an arbitrary account")
	}
	if !strings.Contains(rpcErr.Message, "cartesi_getAccountProof") {
		t.Errorf("error %q does not point at the replacement", rpcErr.Message)
	}
}
