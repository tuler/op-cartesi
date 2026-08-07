package chain

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	abidrive "github.com/tuler/op-cartesi/abi-drive/go"
	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/machine"
)

// testAbiDriveBase places the fixture ABI drive away from the accounts
// drive; any address works — the mock serves whatever ranges are installed
// and the chain discovers them by the locator, not by convention.
const testAbiDriveBase = uint64(0x90000000000000)

func formatTestAbiDrive(t *testing.T) (*drive.MemStore, *abidrive.Drive) {
	t.Helper()
	mem := drive.NewMemStore(262144)
	d, err := abidrive.Format(mem, abidrive.Config{DriveLength: 262144, Capacity: 64, HeapOffset: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return mem, d
}

// CodeAt must answer markers for everything the guest routes — recorded
// contracts from the ABI drive, façades derived from the token registry —
// and nil for the rest.
func TestCodeAtServesMarkers(t *testing.T) {
	ctx := context.Background()
	config := common.HexToAddress("0xc751000000000000000000000000000000000002")
	counter := common.HexToAddress("0xc0de000000000000000000000000000000000001")
	l1Token := common.HexToAddress("0x00000000000000000000000000000000000070ce")

	abiMem, abiDrive := formatTestAbiDrive(t)
	if err := abiDrive.Register(config, abidrive.KindSystem, []byte(`[{"name":"setFee"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := abiDrive.Register(counter, abidrive.KindApp, []byte(`[{"name":"increment"}]`)); err != nil {
		t.Fatal(err)
	}
	accountsMem, accounts := formatTestDrive(t)
	if _, err := accounts.RegisterToken(l1Token, 32); err != nil {
		t.Fatal(err)
	}

	m := machine.NewMock([]byte("code"))
	m.SetAccountsDrive(testDriveBase, accountsMem.B)
	m.SetAbiDrive(testAbiDriveBase, abiMem.B)
	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := c.GenesisHash()

	cases := []struct {
		name string
		addr common.Address
		want []byte
	}{
		{"system built-in", config, CodeMarker(CodeKindSystem)},
		{"application", counter, CodeMarker(CodeKindApp)},
		{"token façade", L2TokenAddress(l1Token), CodeMarker(CodeKindToken)},
		{"EOA", common.HexToAddress("0xa11ce"), nil},
	}
	for _, tc := range cases {
		code, err := c.CodeAt(ctx, at, tc.addr)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !bytes.Equal(code, tc.want) {
			t.Fatalf("%s: code %x, want %x", tc.name, code, tc.want)
		}
	}

	// An unknown block answers ErrNoSnapshot, like AccountAt and Inspect.
	if _, err := c.CodeAt(ctx, common.HexToHash("0xdead"), counter); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("unknown block: want ErrNoSnapshot, got %v", err)
	}

	// ContractsAt lists the same surface: drive entries first (registration
	// order, ABIs included), then façades derived from the registry.
	contracts, err := c.ContractsAt(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) != 3 {
		t.Fatalf("contracts %d, want 3", len(contracts))
	}
	if contracts[0].Address != config || contracts[0].Kind != CodeKindSystem ||
		!bytes.Equal(contracts[0].Abi, []byte(`[{"name":"setFee"}]`)) {
		t.Fatalf("contract 0: %+v", contracts[0])
	}
	if contracts[1].Address != counter || contracts[1].Kind != CodeKindApp {
		t.Fatalf("contract 1: %+v", contracts[1])
	}
	facade := contracts[2]
	if facade.Address != L2TokenAddress(l1Token) || facade.Kind != CodeKindToken ||
		facade.Abi != nil || facade.L1Token == nil || *facade.L1Token != l1Token {
		t.Fatalf("contract 2: %+v", facade)
	}

	if _, err := c.ContractsAt(ctx, common.HexToHash("0xdead")); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("unknown block: want ErrNoSnapshot, got %v", err)
	}
}

// A machine with neither drive routes nothing the host can see: every
// address answers empty, not an error — the mock-machine dev node keeps
// serving wallets.
func TestCodeAtWithoutDrivesAnswersEmpty(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestChain(t, "no-drives")
	code, err := c.CodeAt(ctx, c.GenesisHash(), common.HexToAddress("0xc0de"))
	if err != nil {
		t.Fatal(err)
	}
	if code != nil {
		t.Fatalf("codeless machine answered %x", code)
	}
}

// A token registered mid-chain makes its façade appear at the head block —
// the drive travels with the machine fork, exactly like balances do.
func TestCodeAtSeesRegistrationsOnTheFork(t *testing.T) {
	ctx := context.Background()
	l1Token := common.HexToAddress("0x00000000000000000000000000000000000071ce")
	accountsMem, _ := formatTestDrive(t)
	m := machine.NewMock([]byte("code-fork"))
	m.SetAccountsDrive(testDriveBase, accountsMem.B)
	c, err := New(ctx, testConfig(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	facade := L2TokenAddress(l1Token)
	code, err := c.CodeAt(ctx, c.GenesisHash(), facade)
	if err != nil || code != nil {
		t.Fatalf("pre-registration: code %x err %v", code, err)
	}
	// Register through the genesis machine's installed image (the fixture
	// slice is shared, as SetAccountsDrive documents), then re-read.
	d, err := drive.Open(&drive.MemStore{B: accountsMem.B})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterToken(l1Token, 32); err != nil {
		t.Fatal(err)
	}
	code, err = c.CodeAt(ctx, c.GenesisHash(), facade)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(code, CodeMarker(CodeKindToken)) {
		t.Fatalf("post-registration: code %x, want token marker", code)
	}
}
