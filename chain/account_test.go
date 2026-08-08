package chain

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/machine"
)

// testDriveBase is the spec's recommended well-known start for the accounts
// drive (docs/ACCOUNTS-DRIVE-SPEC.md §4). The devnet's `cartesi build`
// auto-places the drive, and the shim discovers it by label — this constant
// is the fallback convention.
const testDriveBase = uint64(0x80000000000000)

// formatTestDrive builds a drive image with the same geometry the devnet
// guest (app/) formats at boot, so the host is exercised against the
// exact layout it will meet on a real machine.
func formatTestDrive(t *testing.T) (*drive.MemStore, *drive.Drive) {
	t.Helper()
	mem := drive.NewMemStore(1 << 20)
	d, err := drive.Format(mem, drive.Config{
		DriveLength:      1 << 20,
		Profile:          drive.ProfileSparse,
		Capacity:         4096,
		RegistryOffset:   0x100,
		RegistryCapacity: 8,
		SparseOffset:     266240, // 4096 + 4096*64
		SparseCapacity:   4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mem, d
}

// AccountAt must read what the guest wrote — through the drive library, over
// the machine's ReadMemory, with no fork and no execution.
func TestAccountAtReadsTheDrive(t *testing.T) {
	ctx := context.Background()
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	balance := new(big.Int).Lsh(big.NewInt(3), 100) // needs the full uint256 path

	m := machine.NewMock([]byte("accounts"))
	mem, d := formatTestDrive(t)
	if err := d.SetAccount(alice, 7, balance); err != nil {
		t.Fatal(err)
	}
	m.SetAccountsDrive(testDriveBase, mem.B)

	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}

	nonce, bal, err := c.AccountAt(ctx, c.GenesisHash(), alice)
	if err != nil {
		t.Fatal(err)
	}
	if nonce != 7 || bal.Cmp(balance) != 0 {
		t.Fatalf("got nonce %d balance %s, want 7 and %s", nonce, bal, balance)
	}

	// An absent account is a result, not an error: zero nonce, zero balance.
	nonce, bal, err = c.AccountAt(ctx, c.GenesisHash(), common.HexToAddress("0xb0b0"))
	if err != nil {
		t.Fatal(err)
	}
	if nonce != 0 || bal.Sign() != 0 {
		t.Fatalf("absent account answered nonce %d balance %s, want zeros", nonce, bal)
	}

	// The zero address is absent by construction — the drive keys empty
	// slots with it — and must answer zeros, not the drive's key refusal:
	// it is the default from that cast's provider fills nonces for.
	nonce, bal, err = c.AccountAt(ctx, c.GenesisHash(), common.Address{})
	if err != nil {
		t.Fatal(err)
	}
	if nonce != 0 || bal.Sign() != 0 {
		t.Fatalf("zero address answered nonce %d balance %s, want zeros", nonce, bal)
	}

	// Building a block forks the machine; the fork carries the drive, so the
	// new head answers too — that is the "parked machine per recent block"
	// property the RPC leans on.
	buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))
	nonce, bal, err = c.AccountAt(ctx, c.HeadBlock().Hash(), alice)
	if err != nil {
		t.Fatal(err)
	}
	if nonce != 7 || bal.Cmp(balance) != 0 {
		t.Fatalf("head block answered nonce %d balance %s, want 7 and %s", nonce, bal, balance)
	}

	// A block the chain never had answers ErrNoSnapshot, like Inspect.
	if _, _, err := c.AccountAt(ctx, common.HexToHash("0xdead"), alice); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("unknown block: want ErrNoSnapshot, got %v", err)
	}
}

// A machine without a drive must say so, not invent balances.
func TestAccountAtWithoutDriveErrs(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestChain(t, "no-drive")
	if _, _, err := c.AccountAt(ctx, c.GenesisHash(), common.HexToAddress("0xa11ce")); !errors.Is(err, ErrNoAccountsDrive) {
		t.Fatalf("want ErrNoAccountsDrive, got %v", err)
	}
}

// A declared drive whose bytes hold no v1 header is the same answer as no
// drive: there is no ledger to read. This is also what protects the Remote's
// well-known-address fallback from misreading arbitrary memory.
func TestAccountAtUnformattedDriveErrs(t *testing.T) {
	ctx := context.Background()
	m := machine.NewMock([]byte("blank-drive"))
	m.SetAccountsDrive(testDriveBase, make([]byte, 1<<20))
	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.AccountAt(ctx, c.GenesisHash(), common.HexToAddress("0xa11ce")); !errors.Is(err, ErrNoAccountsDrive) {
		t.Fatalf("want ErrNoAccountsDrive, got %v", err)
	}
}
