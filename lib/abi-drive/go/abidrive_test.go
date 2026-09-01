package abidrive

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	drive "github.com/tuler/op-cartesi/lib/accounts-drive/go"
)

var (
	bridge = addr("c751000000000000000000000000000000000001")
	app    = addr("c0de000000000000000000000000000000000001")
)

func addr(hex string) (a [20]byte) {
	const digits = "0123456789abcdef"
	for i := 0; i < 20; i++ {
		a[i] = byte(strings.IndexByte(digits, hex[2*i]))<<4 | byte(strings.IndexByte(digits, hex[2*i+1]))
	}
	return a
}

func testConfig() Config {
	return Config{DriveLength: 262144, Capacity: 64, HeapOffset: 4096}
}

func TestFormatRegisterReopen(t *testing.T) {
	mem := drive.NewMemStore(262144)
	d, err := Format(mem, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Register(bridge, KindSystem, []byte(`[{"name":"withdrawEther"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := d.Register(app, KindApp, []byte(`[{"name":"increment"}]`)); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(mem)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Count() != 2 {
		t.Fatalf("count %d, want 2", reopened.Count())
	}
	e, found, err := reopened.Lookup(app)
	if err != nil || !found {
		t.Fatalf("lookup app: found=%v err=%v", found, err)
	}
	if e.Kind != KindApp || !bytes.Equal(e.Abi, []byte(`[{"name":"increment"}]`)) {
		t.Fatalf("app entry %+v", e)
	}
	if _, found, _ := reopened.Lookup(addr("00000000000000000000000000000000000000aa")); found {
		t.Fatal("unregistered address answered found")
	}
}

func TestRegisterRefusals(t *testing.T) {
	mem := drive.NewMemStore(262144)
	d, _ := Format(mem, testConfig())
	if err := d.Register(app, KindApp, []byte("x")); err != nil {
		t.Fatal(err)
	}
	// Identical re-registration is a no-op; different content is a duplicate.
	if err := d.Register(app, KindApp, []byte("x")); err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
	if err := d.Register(app, KindApp, []byte("y")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}

	tiny, _ := Format(drive.NewMemStore(262144), Config{DriveLength: 262144, Capacity: 1, HeapOffset: 4096})
	if err := tiny.Register(app, KindApp, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tiny.Register(bridge, KindSystem, []byte("x")); !errors.Is(err, ErrRegistryFull) {
		t.Fatalf("want ErrRegistryFull, got %v", err)
	}

	shallow, _ := Format(drive.NewMemStore(4100), Config{DriveLength: 4100, Capacity: 2, HeapOffset: 4096})
	if err := shallow.Register(app, KindApp, []byte("too big for four bytes")); !errors.Is(err, ErrHeapFull) {
		t.Fatalf("want ErrHeapFull, got %v", err)
	}
}

func TestOpenRefusesForeignBytes(t *testing.T) {
	if _, err := Open(drive.NewMemStore(4096)); !errors.Is(err, ErrNotFormatted) {
		t.Fatalf("want ErrNotFormatted, got %v", err)
	}
	mem := drive.NewMemStore(262144)
	if _, err := Format(mem, testConfig()); err != nil {
		t.Fatal(err)
	}
	mem.B[8] = 9 // version
	if _, err := Open(mem); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}

// The golden image is written by the TypeScript reference writer
// (abi-drive/js/golden.ts): reading it here is what pins host and guest to
// the same bytes.
func TestGoldenImageFromTypeScript(t *testing.T) {
	b, err := os.ReadFile("testdata/golden-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Open(&drive.MemStore{B: b})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := d.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries %d, want 2", len(entries))
	}
	if entries[0].Address != bridge || entries[0].Kind != KindSystem {
		t.Fatalf("entry 0: %+v", entries[0])
	}
	if entries[1].Address != app || entries[1].Kind != KindApp {
		t.Fatalf("entry 1: %+v", entries[1])
	}
	if !bytes.Contains(entries[1].Abi, []byte(`"increment"`)) || !bytes.Contains(entries[1].Abi, []byte(`"count"`)) {
		t.Fatalf("app abi: %s", entries[1].Abi)
	}
}
