package drive

// The golden vectors are the cross-language conformance suite: every library
// (Go, Rust, C, Lua, JS, Python) replays the same operation scripts and must
// reach byte-identical drives — the determinism the spec demands, made
// testable. Regenerate with:
//
//	go test -run TestGolden -update
//
// The scenarios' expectations are written down here (every op carries the
// outcome it must have), so regeneration cannot silently bless a behavior
// change: a mismatch fails before the file is written.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite ../testdata/golden.json")

const goldenPath = "../testdata/golden.json"

type goldenConfig struct {
	DriveLength      uint64 `json:"driveLength"`
	Capacity         uint64 `json:"capacity"`
	LoadLimit        uint64 `json:"loadLimit"`
	Seed             string `json:"seed"`
	Profile          uint32 `json:"profile"`
	SlotSize         uint32 `json:"slotSize"`
	TableOffset      uint64 `json:"tableOffset"`
	RegistryOffset   uint64 `json:"registryOffset"`
	RegistryCapacity uint64 `json:"registryCapacity"`
	SparseOffset     uint64 `json:"sparseOffset"`
	SparseCapacity   uint64 `json:"sparseCapacity"`
	SparseLoadLimit  uint64 `json:"sparseLoadLimit"`
	SparseSlotSize   uint32 `json:"sparseSlotSize"`
}

type goldenOp struct {
	Op     string `json:"op"`
	Addr   string `json:"addr,omitempty"`
	Token  string `json:"token,omitempty"`
	Width  int    `json:"width,omitempty"`
	ID     uint16 `json:"id,omitempty"`
	Nonce  uint64 `json:"nonce,omitempty"`
	Value  string `json:"value,omitempty"` // 32-byte big-endian, hex
	Force  bool   `json:"force,omitempty"`
	Expect string `json:"expect"`
}

type goldenCheck struct {
	Check string `json:"check"`
	Table string `json:"table,omitempty"`
	Addr  string `json:"addr,omitempty"`
	ID    uint16 `json:"id,omitempty"`
	Index uint64 `json:"index,omitempty"`
	Found bool   `json:"found,omitempty"`
	Nonce uint64 `json:"nonce,omitempty"`
	Value string `json:"value,omitempty"`
	Count uint64 `json:"count,omitempty"`
	Hex   string `json:"hex,omitempty"`
}

type goldenScenario struct {
	Name   string        `json:"name"`
	Config goldenConfig  `json:"config"`
	Ops    []goldenOp    `json:"ops"`
	Checks []goldenCheck `json:"checks"`
	SHA256 string        `json:"sha256"`
}

type goldenFile struct {
	Description string           `json:"description"`
	Scenarios   []goldenScenario `json:"scenarios"`
}

func kindOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrTableFull):
		return "tableFull"
	case errors.Is(err, ErrRegistryFull):
		return "registryFull"
	case errors.Is(err, ErrOverflow):
		return "overflow"
	case errors.Is(err, ErrNonceProtected):
		return "nonceProtected"
	case errors.Is(err, ErrDuplicateToken):
		return "duplicateToken"
	case errors.Is(err, ErrUnknownToken):
		return "unknownToken"
	case errors.Is(err, ErrBadWidth):
		return "badWidth"
	case errors.Is(err, ErrZeroAddress):
		return "zeroAddress"
	case errors.Is(err, ErrProfile):
		return "profile"
	default:
		return "error: " + err.Error()
	}
}

func toAddr(s string) (a [20]byte) {
	b, _ := hex.DecodeString(s)
	copy(a[:], b)
	return
}

func toBig(s string) *big.Int {
	b, _ := hex.DecodeString(s)
	return new(big.Int).SetBytes(b)
}

func be32(v *big.Int) string {
	var b [32]byte
	v.FillBytes(b[:])
	return hex.EncodeToString(b[:])
}

// replay runs a scenario against a fresh MemStore, asserting every op's
// expected outcome, then every check, then the drive hash (unless updating,
// in which case check values and the hash are filled in).
func replay(t *testing.T, sc *goldenScenario, fill bool) {
	t.Helper()
	seed, _ := hex.DecodeString(sc.Config.Seed)
	cfg := Config{
		DriveLength: sc.Config.DriveLength, Capacity: sc.Config.Capacity,
		LoadLimit: sc.Config.LoadLimit, Profile: sc.Config.Profile,
		SlotSize: sc.Config.SlotSize, TableOffset: sc.Config.TableOffset,
		RegistryOffset: sc.Config.RegistryOffset, RegistryCapacity: sc.Config.RegistryCapacity,
		SparseOffset: sc.Config.SparseOffset, SparseCapacity: sc.Config.SparseCapacity,
		SparseLoadLimit: sc.Config.SparseLoadLimit, SparseSlotSize: sc.Config.SparseSlotSize,
	}
	copy(cfg.Seed[:], seed)
	s := NewMemStore(cfg.DriveLength)
	d, err := Format(s, cfg)
	if err != nil {
		t.Fatalf("%s: format: %v", sc.Name, err)
	}
	for i, op := range sc.Ops {
		var err error
		switch op.Op {
		case "setAccount":
			err = d.SetAccount(toAddr(op.Addr), op.Nonce, toBig(op.Value))
		case "deleteAccount":
			_, err = d.DeleteAccount(toAddr(op.Addr), op.Force)
		case "registerToken":
			_, err = d.RegisterToken(toAddr(op.Token), op.Width)
		case "setTokenBalance":
			err = d.SetTokenBalance(toAddr(op.Addr), op.ID, toBig(op.Value))
		default:
			t.Fatalf("%s op %d: unknown op %q", sc.Name, i, op.Op)
		}
		if got := kindOf(err); got != op.Expect {
			t.Fatalf("%s op %d (%s): outcome %q, want %q", sc.Name, i, op.Op, got, op.Expect)
		}
	}
	for i := range sc.Checks {
		c := &sc.Checks[i]
		switch c.Check {
		case "account":
			a, found, err := d.GetAccount(toAddr(c.Addr))
			if err != nil {
				t.Fatalf("%s check %d: %v", sc.Name, i, err)
			}
			if fill {
				c.Found = found
				if found {
					c.Nonce = a.Nonce
					c.Value = be32(a.Balance)
				}
			} else if found != c.Found || (found && (a.Nonce != c.Nonce || be32(a.Balance) != c.Value)) {
				t.Fatalf("%s check %d (account %s): got found=%v nonce=%d bal=%s", sc.Name, i, c.Addr, found, a.Nonce, be32(a.Balance))
			}
		case "tokenBalance":
			v, err := d.GetTokenBalance(toAddr(c.Addr), c.ID)
			if err != nil {
				t.Fatalf("%s check %d: %v", sc.Name, i, err)
			}
			if fill {
				c.Value = be32(v)
			} else if be32(v) != c.Value {
				t.Fatalf("%s check %d (token %d of %s): got %s, want %s", sc.Name, i, c.ID, c.Addr, be32(v), c.Value)
			}
		case "liveCount":
			var n uint64
			var err error
			if c.Table == "sparse" {
				n, err = d.SparseLiveCount()
			} else {
				n, err = d.LiveCount()
			}
			if err != nil {
				t.Fatal(err)
			}
			if fill {
				c.Count = n
			} else if n != c.Count {
				t.Fatalf("%s check %d: %s liveCount %d, want %d", sc.Name, i, c.Table, n, c.Count)
			}
		case "slot":
			tb := d.accountsTable()
			if c.Table == "sparse" {
				tb = d.sparseTable()
			}
			slot, err := d.readSlot(tb, c.Index)
			if err != nil {
				t.Fatal(err)
			}
			if fill {
				c.Hex = hex.EncodeToString(slot)
			} else if hex.EncodeToString(slot) != c.Hex {
				t.Fatalf("%s check %d: slot %d = %x, want %s", sc.Name, i, c.Index, slot, c.Hex)
			}
		default:
			t.Fatalf("%s check %d: unknown check %q", sc.Name, i, c.Check)
		}
	}
	sum := sha256.Sum256(s.B)
	if fill {
		sc.SHA256 = hex.EncodeToString(sum[:])
	} else if hex.EncodeToString(sum[:]) != sc.SHA256 {
		t.Fatalf("%s: drive sha256 = %x, want %s", sc.Name, sum, sc.SHA256)
	}
}

// ---------------------------------------------------------------- scenarios

func repAddr(b byte) string {
	a := addr(b)
	return hex.EncodeToString(a[:])
}

// drvAddr derives a deterministic test address from a label and an index.
func drvAddr(label string, i int) string {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(i))
	h := Keccak256([]byte(label), n[:])
	return hex.EncodeToString(h[:20])
}

func val(v *big.Int) string { return be32(v) }

func num(x int64) string { return be32(big.NewInt(x)) }

func shifted(x int64, bits uint) string {
	return be32(new(big.Int).Lsh(big.NewInt(x), bits))
}

func seedHex(b byte) string {
	s := make([]byte, 32)
	for i := range s {
		s[i] = b
	}
	return hex.EncodeToString(s)
}

func scenarios() []goldenScenario {
	var out []goldenScenario

	// 1. The spec Appendix B walkthrough, with table-full and delete rules.
	{
		sc := goldenScenario{
			Name: "walkthrough",
			Config: goldenConfig{
				DriveLength: 1 << 16, Capacity: 8, LoadLimit: 4, Seed: seedHex(0),
				SlotSize: 64, TableOffset: 4096,
			},
		}
		ops := []goldenOp{
			{Op: "setAccount", Addr: repAddr(0xbb), Nonce: 0, Value: num(1), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xdd), Nonce: 0, Value: num(2), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xee), Nonce: 0, Value: num(3), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xaa), Nonce: 0, Value: num(4), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xcc), Nonce: 0, Value: num(5), Expect: "tableFull"},
			{Op: "deleteAccount", Addr: repAddr(0xdd), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xcc), Nonce: 0, Value: num(5), Expect: "ok"},
			{Op: "setAccount", Addr: repAddr(0xaa), Nonce: 5, Value: shifted(3, 130), Expect: "ok"},
			{Op: "deleteAccount", Addr: repAddr(0xaa), Expect: "nonceProtected"},
			{Op: "deleteAccount", Addr: repAddr(0xaa), Force: true, Expect: "ok"},
		}
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "slot", Table: "accounts", Index: 1},
			{Check: "slot", Table: "accounts", Index: 2},
			{Check: "slot", Table: "accounts", Index: 3},
			{Check: "slot", Table: "accounts", Index: 7},
			{Check: "account", Addr: repAddr(0xbb)},
			{Check: "account", Addr: repAddr(0xdd)},
			{Check: "account", Addr: repAddr(0x99)},
			{Check: "liveCount", Table: "accounts"},
		}
		out = append(out, sc)
	}

	// 2. Profile 0 at some scale: inserts, updates, deletes, single-asset
	// registry entry (zero address = the native asset).
	{
		sc := goldenScenario{
			Name: "profile0",
			Config: goldenConfig{
				DriveLength: 1 << 18, Capacity: 2048, LoadLimit: 1792, Seed: seedHex(0x11),
				SlotSize: 64, TableOffset: 4096,
				RegistryOffset: 0x100, RegistryCapacity: 1,
			},
		}
		var ops []goldenOp
		ops = append(ops, goldenOp{Op: "registerToken", Token: repAddr(0x00), Width: 32, Expect: "ok"})
		for i := 0; i < 40; i++ {
			bal := new(big.Int).Mul(big.NewInt(int64(i+1)), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
			if i%7 == 0 {
				bal.Add(bal, new(big.Int).Lsh(big.NewInt(int64(i+1)), 200))
			}
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-p0", i), Nonce: uint64(i*7 + 1), Value: val(bal), Expect: "ok"})
		}
		for i := 0; i < 8; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-p0", i), Nonce: uint64(i*7 + 2), Value: num(int64(1000 + i)), Expect: "ok"})
		}
		for _, i := range []int{3, 9} {
			ops = append(ops,
				goldenOp{Op: "setAccount", Addr: drvAddr("golden-p0", i), Nonce: 0, Value: num(0), Expect: "ok"},
				goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-p0", i), Expect: "ok"})
		}
		ops = append(ops,
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-p0", 4), Expect: "nonceProtected"},
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-p0", 4), Force: true, Expect: "ok"})
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "account", Addr: drvAddr("golden-p0", 0)},
			{Check: "account", Addr: drvAddr("golden-p0", 3)},
			{Check: "account", Addr: drvAddr("golden-p0", 4)},
			{Check: "account", Addr: drvAddr("golden-p0", 21)},
			{Check: "account", Addr: drvAddr("golden-p0", 39)},
			{Check: "liveCount", Table: "accounts"},
		}
		out = append(out, sc)
	}

	// 3. Wide profile: packed mixed-width columns, column overflow, width
	// checks, auto-created records, forced delete carrying columns away.
	{
		sc := goldenScenario{
			Name: "wide",
			Config: goldenConfig{
				DriveLength: 1 << 18, Capacity: 256, LoadLimit: 224, Seed: seedHex(0x22),
				Profile: ProfileWide, SlotSize: 128, TableOffset: 4096,
				RegistryOffset: 0x100, RegistryCapacity: 6,
			},
		}
		var ops []goldenOp
		widths := []int{32, 8, 8, 16} // 64 column bytes: exactly the 128-byte slot's room
		for i, w := range widths {
			ops = append(ops, goldenOp{Op: "registerToken", Token: drvAddr("golden-wide-token", i), Width: w, Expect: "ok"})
		}
		ops = append(ops,
			goldenOp{Op: "registerToken", Token: drvAddr("golden-wide-token", 4), Width: 8, Expect: "tableFull"},
			goldenOp{Op: "registerToken", Token: drvAddr("golden-wide-token", 5), Width: 20, Expect: "badWidth"},
			goldenOp{Op: "registerToken", Token: drvAddr("golden-wide-token", 0), Width: 8, Expect: "duplicateToken"})
		for i := 0; i < 30; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-wide", i), Nonce: uint64(i), Value: num(int64(i) * 1_000_000_000), Expect: "ok"})
		}
		for i := 0; i < 30; i++ {
			id := uint16(i % 4)
			var v string
			switch widths[id] {
			case 8:
				v = num(int64(i+1) * 1000003)
			case 16:
				v = shifted(int64(i+1), 70)
			case 32:
				v = shifted(int64(i+1), 200)
			}
			ops = append(ops, goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-wide", i), ID: id, Value: v, Expect: "ok"})
		}
		ops = append(ops,
			// Auto-create a record by giving a fresh address a token balance.
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-wide-fresh", 0), ID: 1, Value: num(777), Expect: "ok"},
			// A width-8 column cannot hold 2^64.
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-wide", 0), ID: 1, Value: shifted(1, 64), Expect: "overflow"},
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-wide", 5), ID: 9, Value: num(1), Expect: "unknownToken"},
			// Zeroing a wide column keeps the record.
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-wide", 2), ID: 2, Value: num(0), Expect: "ok"},
			// A forced delete takes the columns with the record.
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-wide", 7), Force: true, Expect: "ok"})
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "account", Addr: drvAddr("golden-wide", 3)},
			{Check: "account", Addr: drvAddr("golden-wide", 7)},
			{Check: "account", Addr: drvAddr("golden-wide-fresh", 0)},
			{Check: "tokenBalance", Addr: drvAddr("golden-wide", 3), ID: 3},
			{Check: "tokenBalance", Addr: drvAddr("golden-wide", 2), ID: 2},
			{Check: "tokenBalance", Addr: drvAddr("golden-wide-fresh", 0), ID: 1},
			{Check: "tokenBalance", Addr: drvAddr("golden-wide", 11), ID: 3},
			{Check: "liveCount", Table: "accounts"},
		}
		out = append(out, sc)
	}

	// 4. Sparse profile: separate holdings table, zero-deletes, u256 values.
	{
		sc := goldenScenario{
			Name: "sparse",
			Config: goldenConfig{
				DriveLength: 1 << 18, Capacity: 128, LoadLimit: 112, Seed: seedHex(0x33),
				Profile: ProfileSparse, SlotSize: 64, TableOffset: 4096,
				RegistryOffset: 0x100, RegistryCapacity: 8,
				SparseOffset: 1 << 17, SparseCapacity: 128, SparseLoadLimit: 112, SparseSlotSize: 64,
			},
		}
		var ops []goldenOp
		widths := []int{32, 32, 8, 16}
		for i, w := range widths {
			ops = append(ops, goldenOp{Op: "registerToken", Token: drvAddr("golden-sparse-token", i), Width: w, Expect: "ok"})
		}
		for i := 0; i < 25; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-sparse", i), Nonce: uint64(i + 1), Value: num(int64(i) * 31337), Expect: "ok"})
		}
		for i := 0; i < 40; i++ {
			id := uint16(i % 4)
			var v string
			switch widths[id] {
			case 8:
				v = num(int64(i+1) * 65537)
			case 16:
				v = shifted(int64(i+1), 100)
			case 32:
				v = shifted(int64(i+1), 180)
			}
			ops = append(ops, goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-sparse", i%25), ID: id, Value: v, Expect: "ok"})
		}
		for i := 0; i < 6; i++ {
			ops = append(ops, goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-sparse", i), ID: uint16(i % 4), Value: num(0), Expect: "ok"})
		}
		ops = append(ops,
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-sparse", 1), ID: 2, Value: shifted(1, 64), Expect: "overflow"},
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-sparse", 1), ID: 7, Value: num(1), Expect: "unknownToken"})
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "tokenBalance", Addr: drvAddr("golden-sparse", 0), ID: 0},
			{Check: "tokenBalance", Addr: drvAddr("golden-sparse", 8), ID: 0},
			{Check: "tokenBalance", Addr: drvAddr("golden-sparse", 9), ID: 1},
			{Check: "tokenBalance", Addr: drvAddr("golden-sparse", 10), ID: 2},
			{Check: "tokenBalance", Addr: drvAddr("golden-sparse", 11), ID: 3},
			{Check: "liveCount", Table: "accounts"},
			{Check: "liveCount", Table: "sparse"},
		}
		out = append(out, sc)
	}

	// 5. Sparse with 32-byte slots: width-8-only, one leaf per holding.
	{
		sc := goldenScenario{
			Name: "sparse32",
			Config: goldenConfig{
				DriveLength: 1 << 16, Capacity: 16, LoadLimit: 14, Seed: seedHex(0x44),
				Profile: ProfileSparse, SlotSize: 64, TableOffset: 4096,
				RegistryOffset: 0x100, RegistryCapacity: 4,
				SparseOffset: 1 << 15, SparseCapacity: 64, SparseLoadLimit: 56, SparseSlotSize: 32,
			},
		}
		var ops []goldenOp
		ops = append(ops,
			goldenOp{Op: "registerToken", Token: drvAddr("golden-s32-token", 0), Width: 8, Expect: "ok"},
			goldenOp{Op: "registerToken", Token: drvAddr("golden-s32-token", 1), Width: 8, Expect: "ok"},
			goldenOp{Op: "registerToken", Token: drvAddr("golden-s32-token", 2), Width: 16, Expect: "badWidth"})
		for i := 0; i < 10; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-s32", i), Nonce: uint64(i + 1), Value: num(int64(i+1) * 5), Expect: "ok"})
		}
		for i := 0; i < 20; i++ {
			ops = append(ops, goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-s32", i%10), ID: uint16(i % 2), Value: num(int64(i+1) * 424243), Expect: "ok"})
		}
		ops = append(ops,
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-s32", 0), ID: 0, Value: shifted(1, 64), Expect: "overflow"},
			goldenOp{Op: "setTokenBalance", Addr: drvAddr("golden-s32", 3), ID: 1, Value: num(0), Expect: "ok"})
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "tokenBalance", Addr: drvAddr("golden-s32", 0), ID: 0},
			{Check: "tokenBalance", Addr: drvAddr("golden-s32", 3), ID: 1},
			{Check: "tokenBalance", Addr: drvAddr("golden-s32", 4), ID: 0},
			{Check: "liveCount", Table: "sparse"},
		}
		out = append(out, sc)
	}

	// 6. Compact profile-0 records: one leaf per account, uint32 nonce,
	// uint64 balance, width-8 registry rule.
	{
		sc := goldenScenario{
			Name: "compact",
			Config: goldenConfig{
				DriveLength: 1 << 16, Capacity: 64, LoadLimit: 56, Seed: seedHex(0x55),
				SlotSize: 32, TableOffset: 4096,
				RegistryOffset: 0x100, RegistryCapacity: 2,
			},
		}
		var ops []goldenOp
		ops = append(ops,
			goldenOp{Op: "registerToken", Token: drvAddr("golden-compact-token", 0), Width: 8, Expect: "ok"},
			goldenOp{Op: "registerToken", Token: drvAddr("golden-compact-token", 1), Width: 16, Expect: "badWidth"})
		for i := 0; i < 20; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-compact", i), Nonce: uint64(i) * 200_000_000, Value: num(int64(i+1) * 1_000_000), Expect: "ok"})
		}
		for i := 0; i < 5; i++ {
			ops = append(ops, goldenOp{Op: "setAccount", Addr: drvAddr("golden-compact", i), Nonce: uint64(i)*200_000_000 + 1, Value: num(int64(i+1) * 999_983), Expect: "ok"})
		}
		ops = append(ops,
			goldenOp{Op: "setAccount", Addr: drvAddr("golden-compact", 6), Nonce: 1 << 32, Value: num(1), Expect: "overflow"},
			goldenOp{Op: "setAccount", Addr: drvAddr("golden-compact", 6), Nonce: 1, Value: shifted(1, 64), Expect: "overflow"},
			goldenOp{Op: "setAccount", Addr: drvAddr("golden-compact", 2), Nonce: 0, Value: num(0), Expect: "ok"},
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-compact", 2), Expect: "ok"},
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-compact", 3), Expect: "nonceProtected"},
			goldenOp{Op: "deleteAccount", Addr: drvAddr("golden-compact", 3), Force: true, Expect: "ok"})
		sc.Ops = ops
		sc.Checks = []goldenCheck{
			{Check: "account", Addr: drvAddr("golden-compact", 0)},
			{Check: "account", Addr: drvAddr("golden-compact", 2)},
			{Check: "account", Addr: drvAddr("golden-compact", 3)},
			{Check: "account", Addr: drvAddr("golden-compact", 19)},
			{Check: "liveCount", Table: "accounts"},
		}
		out = append(out, sc)
	}

	return out
}

func TestGolden(t *testing.T) {
	if *update {
		f := goldenFile{
			Description: "Cross-language conformance vectors for the Cartesi Accounts Drive Format v1. " +
				"Each scenario formats a zeroed drive, applies ops (asserting each outcome), " +
				"verifies checks, and hashes the whole drive image. Generated by the Go reference: " +
				"go test -run TestGolden -update.",
			Scenarios: scenarios(),
		}
		for i := range f.Scenarios {
			replay(t, &f.Scenarios[i], true)
		}
		data, err := json.MarshalIndent(&f, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (generate with: go test -run TestGolden -update)", err)
	}
	var f goldenFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("golden file has no scenarios")
	}
	// The ops and config in the file are replayed as read, so hand-edits to
	// the vectors are exercised, not silently ignored; the scenario source in
	// this file only matters at -update time.
	for i := range f.Scenarios {
		sc := &f.Scenarios[i]
		t.Run(sc.Name, func(t *testing.T) { replay(t, sc, false) })
	}
}

var _ = fmt.Sprintf // keep fmt imported for debug edits
