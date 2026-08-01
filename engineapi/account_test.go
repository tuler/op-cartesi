package engineapi

import (
	"context"
	"math/big"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// driveBase is where devnet/build-snapshot.sh puts the accounts drive.
const driveBase = uint64(0x80000000000000)

// newDriveBackedServer is newPublicServer with an accounts drive installed in
// the mock: the drive is formatted with the accounts-drive library — the same
// geometry the devnet guest formats at boot — and pre-credited, so the RPC
// answers can be checked against exactly what was written.
func newDriveBackedServer(t *testing.T, write func(*drive.Drive)) (*rpcClient, *chain.Chain) {
	t.Helper()
	mem := drive.NewMemStore(1 << 20)
	d, err := drive.Format(mem, drive.Config{
		DriveLength:      1 << 20,
		Profile:          drive.ProfileSparse,
		Capacity:         4096,
		RegistryOffset:   0x100,
		RegistryCapacity: 8,
		SparseOffset:     266240,
		SparseCapacity:   4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	write(d)

	m := machine.NewMock([]byte("accounts-rpc-test"))
	m.SetAccountsDrive(driveBase, mem.B)
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

// eth_getBalance and eth_getTransactionCount must serve what the drive holds,
// resolved through the same block tags the rest of the eth namespace takes.
func TestGetBalanceAndNonceFromTheDrive(t *testing.T) {
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	balance := new(big.Int).Lsh(big.NewInt(5), 90) // past uint64, so the full word is served

	client, c := newDriveBackedServer(t, func(d *drive.Drive) {
		if err := d.SetAccount(alice, 42, balance); err != nil {
			t.Fatal(err)
		}
	})
	sequenceOneBlock(t, c)

	var bal hexutil.Big
	client.call("eth_getBalance", &bal, alice, "latest")
	if (*big.Int)(&bal).Cmp(balance) != 0 {
		t.Fatalf("eth_getBalance = %s, want %s", (*big.Int)(&bal), balance)
	}
	var nonce hexutil.Uint64
	client.call("eth_getTransactionCount", &nonce, alice, "latest")
	if nonce != 42 {
		t.Fatalf("eth_getTransactionCount = %d, want 42", nonce)
	}

	// Block-tag resolution: pending maps to the head, an explicit number and a
	// block hash resolve through the retained snapshots. (The mock's drive is
	// static, so every block answers alike; what is being checked is that each
	// tag finds a machine at all.)
	client.call("eth_getTransactionCount", &nonce, alice, "pending")
	if nonce != 42 {
		t.Fatalf("pending nonce = %d, want 42", nonce)
	}
	client.call("eth_getBalance", &bal, alice, "0x0")
	if (*big.Int)(&bal).Cmp(balance) != 0 {
		t.Fatalf("balance at block 0 = %s, want %s", (*big.Int)(&bal), balance)
	}
	client.call("eth_getBalance", &bal, alice, c.GenesisHash())
	if (*big.Int)(&bal).Cmp(balance) != 0 {
		t.Fatalf("balance by block hash = %s, want %s", (*big.Int)(&bal), balance)
	}

	// An account with no record is zeros, not an error — the standard answer
	// for an untouched address.
	bob := common.HexToAddress("0xb0b0")
	client.call("eth_getBalance", &bal, bob, "latest")
	if (*big.Int)(&bal).Sign() != 0 {
		t.Fatalf("absent account balance = %s, want 0", (*big.Int)(&bal))
	}
	client.call("eth_getTransactionCount", &nonce, bob, "latest")
	if nonce != 0 {
		t.Fatalf("absent account nonce = %d, want 0", nonce)
	}
}

// A machine without an accounts drive — the mock in dev mode, or a guest
// predating the drive — answers zeros rather than erroring, so wallets keep
// working against it. An RPC consumer cannot distinguish an empty account
// from a missing ledger anyway.
func TestAccountRPCWithoutDriveServesZeros(t *testing.T) {
	client, _ := newPublicServer(t)
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")

	var bal hexutil.Big
	client.call("eth_getBalance", &bal, alice, "latest")
	if (*big.Int)(&bal).Sign() != 0 {
		t.Fatalf("drive-less balance = %s, want 0", (*big.Int)(&bal))
	}
	var nonce hexutil.Uint64
	client.call("eth_getTransactionCount", &nonce, alice, "latest")
	if nonce != 0 {
		t.Fatalf("drive-less nonce = %d, want 0", nonce)
	}
}
