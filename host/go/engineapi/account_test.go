package engineapi

import (
	"context"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
	drive "github.com/tuler/op-cartesi/lib/accounts-drive/go"
)

// driveBase is where scripts/build-snapshot.ts puts the accounts drive.
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

	// The zero address too: it is the default from a fromless eth_call gets,
	// and cast's provider fills its nonce before calling — this exact request
	// is what a bare `cast call` sends first, and it must not error.
	client.call("eth_getTransactionCount", &nonce, common.Address{}, "latest")
	if nonce != 0 {
		t.Fatalf("zero address nonce = %d, want 0", nonce)
	}
	client.call("eth_getBalance", &bal, common.Address{}, "latest")
	if (*big.Int)(&bal).Sign() != 0 {
		t.Fatalf("zero address balance = %s, want 0", (*big.Int)(&bal))
	}
}

// cartesi_getAccountProof is a proof endpoint: against a machine that cannot
// produce Merkle proofs (the mock — machine.get_proof needs an emulator) it
// must error rather than serve unproven pages. The record/walk assembly it
// errors around is covered on the mock by chain.TestAccountProofWalk; the
// full proof path is emulator-dependent and runs on the devnet.
func TestGetAccountProofWithoutEmulatorErrs(t *testing.T) {
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	client, c := newDriveBackedServer(t, func(d *drive.Drive) {
		if err := d.SetAccount(alice, 1, big.NewInt(5)); err != nil {
			t.Fatal(err)
		}
	})
	sequenceOneBlock(t, c)

	msg := client.callError("cartesi_getAccountProof", alice, "latest").Message
	if !strings.Contains(msg, "mock") {
		t.Fatalf("error %q does not name the mock's missing proof capability", msg)
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
