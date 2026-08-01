package chain

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/machine"
)

// driveHome recomputes spec §6.1's home slot, independently of the walk
// under test.
func driveHome(cfg drive.Config, addr common.Address) uint64 {
	h := drive.Keccak256(cfg.Seed[:], addr.Bytes())
	return binary.LittleEndian.Uint64(h[:8]) % cfg.Capacity
}

// grindAddress finds an address whose home slot is the wanted one, skipping
// the given addresses — cheap with the devnet's zero seed and 4096 slots.
func grindAddress(t *testing.T, cfg drive.Config, want uint64, not ...common.Address) common.Address {
	t.Helper()
	for i := int64(1); i < 1_000_000; i++ {
		addr := common.BigToAddress(big.NewInt(i))
		if driveHome(cfg, addr) != want {
			continue
		}
		skip := false
		for _, n := range not {
			if addr == n {
				skip = true
			}
		}
		if !skip {
			return addr
		}
	}
	t.Fatal("no address grinds to the wanted home slot")
	return common.Address{}
}

// The walk half of an account proof — record, probe, page ranges — must be
// assembled from ReadMemory alone, so it is testable against the mock; only
// the proof attachment needs an emulator (asserted at the bottom).
func TestAccountProofWalk(t *testing.T) {
	ctx := context.Background()
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	balance := new(big.Int).Lsh(big.NewInt(3), 100)

	m := machine.NewMock([]byte("account-proof"))
	mem, d := formatTestDrive(t)
	if err := d.SetAccount(alice, 7, balance); err != nil {
		t.Fatal(err)
	}
	m.SetAccountsDrive(testDriveBase, mem.B)
	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.Config()

	proof, err := c.AccountProofAt(ctx, c.GenesisHash(), alice, false)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Found || proof.Nonce != 7 || proof.Balance.Cmp(balance) != 0 {
		t.Fatalf("record: found=%v nonce=%d balance=%s", proof.Found, proof.Nonce, proof.Balance)
	}
	if proof.DriveBase != testDriveBase {
		t.Fatalf("drive base %#x, want %#x", proof.DriveBase, testDriveBase)
	}
	if proof.Config.Capacity != cfg.Capacity || proof.Config.Seed != cfg.Seed {
		t.Fatal("geometry does not echo the drive header")
	}
	// Alice is alone in the table, so she sits at her home slot and the walk
	// is one slot long.
	home := driveHome(cfg, alice)
	if proof.HomeSlot != home || proof.SlotIndex != home || proof.WalkLength != 1 {
		t.Fatalf("walk: home=%d slot=%d len=%d, want home=slot=%d len=1", proof.HomeSlot, proof.SlotIndex, proof.WalkLength, home)
	}
	// The record bytes are the drive's own, and the walk page covers them.
	slotOff := cfg.TableOffset + uint64(cfg.SlotSize)*home
	if !bytes.Equal(proof.Record, mem.B[slotOff:slotOff+uint64(cfg.SlotSize)]) {
		t.Fatal("record bytes are not the drive's slot bytes")
	}
	if len(proof.WalkPages) != 1 {
		t.Fatalf("%d walk pages, want 1", len(proof.WalkPages))
	}
	page := proof.WalkPages[0]
	if page.DriveOffset > slotOff || slotOff+uint64(cfg.SlotSize) > page.DriveOffset+4096 {
		t.Fatalf("walk page [%#x,%#x) does not cover the record at %#x", page.DriveOffset, page.DriveOffset+4096, slotOff)
	}
	if page.Address != testDriveBase+page.DriveOffset {
		t.Fatalf("page address %#x, want base+offset %#x", page.Address, testDriveBase+page.DriveOffset)
	}
	if !bytes.Equal(page.Data, mem.B[page.DriveOffset:page.DriveOffset+4096]) {
		t.Fatal("walk page bytes are not the drive's bytes")
	}
	// The header page rides along, so a verifier gets the walk's constants
	// under the same root.
	if proof.HeaderPage.DriveOffset != 0 || !bytes.Equal(proof.HeaderPage.Data, mem.B[:4096]) {
		t.Fatal("header page is not the drive's first 4096 bytes")
	}
	if proof.HeaderPage.Proof != nil || page.Proof != nil {
		t.Fatal("proofs attached without being requested")
	}

	// Absence is a result with a walk of its own: the probe ends at an empty
	// slot inside the served pages (spec §6.6 is what makes that provable).
	bob := common.HexToAddress("0xb0b0")
	absent, err := c.AccountProofAt(ctx, c.GenesisHash(), bob, false)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Found || absent.Balance.Sign() != 0 || absent.Nonce != 0 {
		t.Fatal("absent account reported as found")
	}
	if absent.WalkLength == 0 || len(absent.WalkPages) == 0 {
		t.Fatal("absence must still carry a walk and its pages")
	}

	// The proof half needs machine.get_proof, which the mock does not have:
	// requesting proofs must fail loudly, not serve unproven pages.
	if _, err := c.AccountProofAt(ctx, c.GenesisHash(), alice, true); !errors.Is(err, machine.ErrProofsUnsupported) {
		t.Fatalf("withProofs on the mock: got %v, want ErrProofsUnsupported", err)
	}
}

// A probe chain that wraps the table end covers slots at both ends of the
// table, so the page set has to come from two byte ranges.
func TestAccountProofWalkWrapsTheTable(t *testing.T) {
	ctx := context.Background()
	m := machine.NewMock([]byte("account-proof-wrap"))
	mem, d := formatTestDrive(t)
	cfg := d.Config()

	last := cfg.Capacity - 1
	first := grindAddress(t, cfg, last)
	second := grindAddress(t, cfg, last, first)
	// Both home at the last slot; robin hood places the first there and
	// probes the second across the wrap into slot 0.
	if err := d.SetAccount(first, 1, big.NewInt(10)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAccount(second, 2, big.NewInt(20)); err != nil {
		t.Fatal(err)
	}
	m.SetAccountsDrive(testDriveBase, mem.B)
	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}

	proof, err := c.AccountProofAt(ctx, c.GenesisHash(), second, false)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Found || proof.SlotIndex != 0 || proof.WalkLength != 2 {
		t.Fatalf("wrapped walk: found=%v slot=%d len=%d, want slot 0 in 2", proof.Found, proof.SlotIndex, proof.WalkLength)
	}
	if len(proof.WalkPages) != 2 {
		t.Fatalf("%d walk pages, want the table's last and first", len(proof.WalkPages))
	}
	firstPage := cfg.TableOffset &^ 4095
	lastPage := (cfg.TableOffset + uint64(cfg.SlotSize)*last) &^ 4095
	if proof.WalkPages[0].DriveOffset != firstPage || proof.WalkPages[1].DriveOffset != lastPage {
		t.Fatalf("pages at %#x and %#x, want %#x and %#x",
			proof.WalkPages[0].DriveOffset, proof.WalkPages[1].DriveOffset, firstPage, lastPage)
	}
}
