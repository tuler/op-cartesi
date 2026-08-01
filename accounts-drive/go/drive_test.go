package drive

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"
)

func h2b(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func addr(b byte) (a [20]byte) {
	for i := range a {
		a[i] = b
	}
	return
}

// TestKeccakVectors pins the hash to the spec's Appendix B vectors (generated
// with go-ethereum's keccak) and the well-known empty-string digest.
func TestKeccakVectors(t *testing.T) {
	empty := Keccak256()
	if hex.EncodeToString(empty[:]) != "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470" {
		t.Fatalf("keccak256(\"\") = %x", empty)
	}
	seed := make([]byte, 32)
	bb := addr(0xbb)
	got := Keccak256(seed, bb[:])
	if hex.EncodeToString(got[:]) != "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92" {
		t.Fatalf("keccak256(seed‖bb..) = %x", got)
	}
	sparse := Keccak256(seed, sparseKey(3, bb))
	if hex.EncodeToString(sparse[:]) != "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3" {
		t.Fatalf("keccak256(seed‖sparse key) = %x", sparse)
	}
}

// TestHomeSlots pins the five spec home slots at C=8.
func TestHomeSlots(t *testing.T) {
	d := &Drive{} // zero seed
	want := map[byte]uint64{0xaa: 5, 0xbb: 1, 0xcc: 7, 0xdd: 1, 0xee: 1}
	for b, w := range want {
		a := addr(b)
		if got := d.home(a[:], 8); got != w {
			t.Errorf("home(%02x..) mod 8 = %d, want %d", b, got, w)
		}
	}
	bb := addr(0xbb)
	if got := d.home(bb[:], 1<<19); got != 342121 {
		t.Errorf("home(bb..) mod 2^19 = %d, want 342121", got)
	}
	if got := d.home(sparseKey(3, bb), 1<<19); got != 117477 {
		t.Errorf("sparse home = %d, want 117477", got)
	}
}

// TestRecordEncodings pins the spec's record encodings.
func TestRecordEncodings(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{DriveLength: 1 << 16, Capacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetAccount(addr(0xbb), 7, big.NewInt(5)); err != nil {
		t.Fatal(err)
	}
	want := h2b(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000"+
		"0000000000000000000000000000000000000000000000000000000000000005")
	got := s.B[4096+64*1 : 4096+64*2] // home(bb..) mod 8 = 1
	if !bytes.Equal(got, want) {
		t.Fatalf("account record:\n got %x\nwant %x", got, want)
	}
}

func TestSparse32RecordEncoding(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{
		DriveLength: 1 << 16, Capacity: 8, Profile: ProfileSparse,
		RegistryOffset: 0x100, RegistryCapacity: 8,
		SparseOffset: 8192, SparseCapacity: 8, SparseSlotSize: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := d.RegisterToken(addr(byte(i+1)), 8); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetTokenBalance(addr(0xbb), 3, big.NewInt(1000000)); err != nil {
		t.Fatal(err)
	}
	// sparse home(id 3, bb..) mod 8: LE64(e5cad187...) = 0xbfc3cb1987d1cae5 → mod 8 = 5
	got := s.B[8192+32*5 : 8192+32*6]
	want := h2b(t, "03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240")
	if !bytes.Equal(got, want) {
		t.Fatalf("sparse32 record:\n got %x\nwant %x", got, want)
	}
}

// TestWalkthrough replays the spec's Appendix B probe walkthrough at C=8.
func TestWalkthrough(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{DriveLength: 1 << 16, Capacity: 8, LoadLimit: 7})
	if err != nil {
		t.Fatal(err)
	}
	one := big.NewInt(1)
	for _, b := range []byte{0xbb, 0xdd, 0xee, 0xaa} {
		if err := d.SetAccount(addr(b), 0, one); err != nil {
			t.Fatal(err)
		}
	}
	slotAddr := func(i int) byte { return s.B[4096+64*i] }
	for slot, want := range map[int]byte{1: 0xbb, 2: 0xdd, 3: 0xee, 5: 0xaa} {
		if got := slotAddr(slot); got != want {
			t.Fatalf("slot %d holds %02x, want %02x", slot, got, want)
		}
	}
	// Absent key with home 1 terminates via the empty slot 4.
	if _, found, err := d.GetAccount(addr(0x99)); err != nil || found {
		t.Fatalf("expected absence, got found=%v err=%v", found, err)
	}
	// Delete dd: ee backward-shifts into slot 2, slot 3 zeroes.
	if ok, err := d.DeleteAccount(addr(0xdd), false); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if slotAddr(2) != 0xee || slotAddr(3) != 0 {
		t.Fatalf("after delete: slot2=%02x slot3=%02x", slotAddr(2), slotAddr(3))
	}
	for _, b := range []byte{0xbb, 0xee, 0xaa} {
		if _, found, _ := d.GetAccount(addr(b)); !found {
			t.Fatalf("%02x.. lost after delete", b)
		}
	}
	if _, found, _ := d.GetAccount(addr(0xdd)); found {
		t.Fatal("dd.. still found after delete")
	}
	if n, _ := d.LiveCount(); n != 3 {
		t.Fatalf("liveCount = %d, want 3", n)
	}
}

func TestTableFullAndNonceProtection(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{DriveLength: 1 << 16, Capacity: 8, LoadLimit: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []byte{0xaa, 0xbb, 0xcc, 0xdd} {
		if err := d.SetAccount(addr(b), 5, big.NewInt(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetAccount(addr(0xee), 0, big.NewInt(1)); !errors.Is(err, ErrTableFull) {
		t.Fatalf("want ErrTableFull, got %v", err)
	}
	// Updating an existing account is not an insert and must still work.
	if err := d.SetAccount(addr(0xaa), 6, big.NewInt(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteAccount(addr(0xbb), false); !errors.Is(err, ErrNonceProtected) {
		t.Fatalf("want ErrNonceProtected, got %v", err)
	}
	if ok, err := d.DeleteAccount(addr(0xbb), true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := d.SetAccount(addr(0xee), 0, big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
}

func TestWideProfile(t *testing.T) {
	s := NewMemStore(1 << 18)
	d, err := Format(s, Config{
		DriveLength: 1 << 18, Capacity: 64, Profile: ProfileWide, SlotSize: 128,
		RegistryOffset: 0x100, RegistryCapacity: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterToken(addr(0x01), 32); err != nil {
		t.Fatal(err)
	}
	id2, err := d.RegisterToken(addr(0x02), 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterToken(addr(0x03), 8); err != nil {
		t.Fatal(err)
	}
	// 64 + 32 + 8 + 8 = 112; another 32 would need 144 > 128.
	if _, err := d.RegisterToken(addr(0x04), 32); !errors.Is(err, ErrTableFull) {
		t.Fatalf("want column overflow, got %v", err)
	}
	if _, err := d.RegisterToken(addr(0x01), 8); !errors.Is(err, ErrDuplicateToken) {
		t.Fatalf("want ErrDuplicateToken, got %v", err)
	}
	// Auto-create an account by setting a token balance, then check the
	// native record write preserves the column.
	user := addr(0x77)
	if err := d.SetTokenBalance(user, id2, big.NewInt(12345)); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAccount(user, 9, big.NewInt(500)); err != nil {
		t.Fatal(err)
	}
	v, err := d.GetTokenBalance(user, id2)
	if err != nil || v.Int64() != 12345 {
		t.Fatal(v, err)
	}
	a, found, _ := d.GetAccount(user)
	if !found || a.Nonce != 9 || a.Balance.Int64() != 500 {
		t.Fatal(a, found)
	}
	// Width 8 column overflows past 2^64-1.
	big65 := new(big.Int).Lsh(big.NewInt(1), 64)
	if err := d.SetTokenBalance(user, id2, big65); !errors.Is(err, ErrOverflow) {
		t.Fatalf("want ErrOverflow, got %v", err)
	}
	// Reopen: registry and balances survive.
	d2, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Tokens()) != 3 {
		t.Fatal("registry lost on reopen")
	}
	v, _ = d2.GetTokenBalance(user, id2)
	if v.Int64() != 12345 {
		t.Fatal("balance lost on reopen")
	}
}

func TestSparseProfile(t *testing.T) {
	s := NewMemStore(1 << 18)
	d, err := Format(s, Config{
		DriveLength: 1 << 18, Capacity: 64, Profile: ProfileSparse,
		RegistryOffset: 0x100, RegistryCapacity: 8,
		SparseOffset: 1 << 17, SparseCapacity: 16, SparseLoadLimit: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.RegisterToken(addr(0x01), 32)
	if err != nil {
		t.Fatal(err)
	}
	user := addr(0x77)
	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	if err := d.SetTokenBalance(user, id, huge); err != nil {
		t.Fatal(err)
	}
	v, err := d.GetTokenBalance(user, id)
	if err != nil || v.Cmp(huge) != 0 {
		t.Fatal(v, err)
	}
	if n, _ := d.SparseLiveCount(); n != 1 {
		t.Fatalf("sparseLiveCount = %d", n)
	}
	// Zero deletes the record.
	if err := d.SetTokenBalance(user, id, new(big.Int)); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.SparseLiveCount(); n != 0 {
		t.Fatalf("sparseLiveCount after zero = %d", n)
	}
	v, _ = d.GetTokenBalance(user, id)
	if v.Sign() != 0 {
		t.Fatal("balance survives delete")
	}
	if _, err := d.GetTokenBalance(user, 9); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("want ErrUnknownToken, got %v", err)
	}
}

// TestCompactRecord pins the 32-byte profile-0 record: encoding, the uint32
// nonce and uint64 balance bounds, and the registry width rule.
func TestCompactRecord(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{
		DriveLength: 1 << 16, Capacity: 8, SlotSize: 32,
		RegistryOffset: 0x100, RegistryCapacity: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetAccount(addr(0xbb), 7, big.NewInt(5)); err != nil {
		t.Fatal(err)
	}
	got := s.B[4096+32*1 : 4096+32*2] // home(bb..) mod 8 = 1
	want := h2b(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb070000000000000000000005")
	if !bytes.Equal(got, want) {
		t.Fatalf("compact record:\n got %x\nwant %x", got, want)
	}
	a, found, err := d.GetAccount(addr(0xbb))
	if err != nil || !found || a.Nonce != 7 || a.Balance.Int64() != 5 {
		t.Fatal(a, found, err)
	}
	if err := d.SetAccount(addr(0xbb), 1<<32, big.NewInt(5)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("nonce past uint32: want ErrOverflow, got %v", err)
	}
	big65 := new(big.Int).Lsh(big.NewInt(1), 64)
	if err := d.SetAccount(addr(0xbb), 7, big65); !errors.Is(err, ErrOverflow) {
		t.Fatalf("balance past uint64: want ErrOverflow, got %v", err)
	}
	if _, err := d.RegisterToken(addr(0x01), 32); !errors.Is(err, ErrBadWidth) {
		t.Fatalf("compact registry width: want ErrBadWidth, got %v", err)
	}
	if _, err := d.RegisterToken([20]byte{}, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteAccount(addr(0xbb), false); !errors.Is(err, ErrNonceProtected) {
		t.Fatalf("want ErrNonceProtected, got %v", err)
	}
}

func TestCompactSlotSizeOnlyProfile0(t *testing.T) {
	_, err := Format(NewMemStore(1<<16), Config{
		DriveLength: 1 << 16, Capacity: 8, SlotSize: 32, Profile: ProfileSparse,
		RegistryOffset: 0x100, RegistryCapacity: 2,
		SparseOffset: 8192, SparseCapacity: 8,
	})
	if err == nil {
		t.Fatal("32-byte account slots outside profile 0 must not format")
	}
}

func TestSparse32RequiresWidth8(t *testing.T) {
	s := NewMemStore(1 << 16)
	d, err := Format(s, Config{
		DriveLength: 1 << 16, Capacity: 8, Profile: ProfileSparse,
		RegistryOffset: 0x100, RegistryCapacity: 8,
		SparseOffset: 8192, SparseCapacity: 8, SparseSlotSize: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RegisterToken(addr(0x01), 16); !errors.Is(err, ErrBadWidth) {
		t.Fatalf("want ErrBadWidth, got %v", err)
	}
}
