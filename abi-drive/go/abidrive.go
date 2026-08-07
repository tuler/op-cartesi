// Package abidrive reads and writes the ABI drive
// (docs/ABI-DRIVE-SPEC.md): the raw flash drive on which a routed guest
// records the contract addresses it serves and the standard JSON ABI of
// each. The drive is written once, at the guest's first boot, before the
// first yield — so a reader needs no locking discipline, and the host can
// read it from a stored snapshot (or a parked machine's memory) exactly the
// way it reads balances from the accounts drive.
//
// The package mirrors accounts-drive/go: byte-level access goes through the
// same Store abstraction, addresses are plain [20]byte, and the writer half
// exists for tests and host-side authoring.
package abidrive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
)

const (
	// Magic identifies a formatted ABI drive (spec §3).
	Magic = "ctsiabis"
	// Version is the one layout this package speaks.
	Version = 1

	// KindSystem marks a built-in of the standard; KindApp an application
	// contract (spec §3).
	KindSystem = 0
	KindApp    = 1

	headerLength = 64
	entryLength  = 32
)

var (
	// ErrNotFormatted means the bytes hold no v1 ABI-drive header.
	ErrNotFormatted = errors.New("not an abi drive (bad magic)")
	// ErrBadVersion means a formatted drive of a version this reader refuses.
	ErrBadVersion = errors.New("abi drive version not supported")
	// ErrRegistryFull and ErrHeapFull are the writer's capacity refusals.
	ErrRegistryFull = errors.New("abi drive registry is full")
	ErrHeapFull     = errors.New("abi drive heap is full")
	// ErrDuplicate means the address is already registered with different
	// content (identical re-registration is a no-op).
	ErrDuplicate = errors.New("address already registered")
)

// Entry is one recorded contract.
type Entry struct {
	Address [20]byte
	Kind    byte
	// Abi is the recorded blob: standard JSON ABI text (spec §3).
	Abi []byte
}

// Config is the writer's geometry (spec §5 fixes the devnet's).
type Config struct {
	DriveLength uint32
	Capacity    uint32
	HeapOffset  uint32
}

// Drive is an open ABI drive.
type Drive struct {
	store       drive.Store
	capacity    uint32
	count       uint32
	indexOffset uint32
	heapOffset  uint32
	heapLength  uint32
	heapUsed    uint32
}

// Format writes a fresh header (spec §3) and returns the empty drive.
func Format(s drive.Store, cfg Config) (*Drive, error) {
	indexEnd := uint32(headerLength) + cfg.Capacity*entryLength
	if cfg.HeapOffset < indexEnd || cfg.HeapOffset >= cfg.DriveLength {
		return nil, fmt.Errorf("heap at %d, index ends at %d: bad geometry", cfg.HeapOffset, indexEnd)
	}
	h := make([]byte, headerLength)
	copy(h, Magic)
	binary.LittleEndian.PutUint16(h[8:], Version)
	binary.LittleEndian.PutUint32(h[12:], cfg.Capacity)
	binary.LittleEndian.PutUint32(h[16:], 0)
	binary.LittleEndian.PutUint32(h[20:], headerLength)
	binary.LittleEndian.PutUint32(h[24:], cfg.HeapOffset)
	binary.LittleEndian.PutUint32(h[28:], cfg.DriveLength-cfg.HeapOffset)
	binary.LittleEndian.PutUint32(h[32:], 0)
	if err := s.WriteAt(0, h); err != nil {
		return nil, err
	}
	return &Drive{
		store:       s,
		capacity:    cfg.Capacity,
		indexOffset: headerLength,
		heapOffset:  cfg.HeapOffset,
		heapLength:  cfg.DriveLength - cfg.HeapOffset,
	}, nil
}

// Open validates the header and returns a reader over formatted bytes.
func Open(s drive.Store) (*Drive, error) {
	h := make([]byte, headerLength)
	if err := s.ReadAt(0, h); err != nil {
		return nil, err
	}
	if string(h[:8]) != Magic {
		return nil, ErrNotFormatted
	}
	if v := binary.LittleEndian.Uint16(h[8:]); v != Version {
		return nil, fmt.Errorf("%w: version %d", ErrBadVersion, v)
	}
	return &Drive{
		store:       s,
		capacity:    binary.LittleEndian.Uint32(h[12:]),
		count:       binary.LittleEndian.Uint32(h[16:]),
		indexOffset: binary.LittleEndian.Uint32(h[20:]),
		heapOffset:  binary.LittleEndian.Uint32(h[24:]),
		heapLength:  binary.LittleEndian.Uint32(h[28:]),
		heapUsed:    binary.LittleEndian.Uint32(h[32:]),
	}, nil
}

// Count is the number of recorded contracts.
func (d *Drive) Count() uint32 { return d.count }

func (d *Drive) entryAt(i uint32) (Entry, error) {
	raw := make([]byte, entryLength)
	if err := d.store.ReadAt(uint64(d.indexOffset)+uint64(i)*entryLength, raw); err != nil {
		return Entry{}, err
	}
	var e Entry
	copy(e.Address[:], raw[:20])
	e.Kind = raw[20]
	abiOffset := binary.LittleEndian.Uint32(raw[22:])
	abiLength := binary.LittleEndian.Uint32(raw[26:])
	e.Abi = make([]byte, abiLength)
	if err := d.store.ReadAt(uint64(abiOffset), e.Abi); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Entries returns every recorded contract, in registration order.
func (d *Drive) Entries() ([]Entry, error) {
	out := make([]Entry, 0, d.count)
	for i := uint32(0); i < d.count; i++ {
		e, err := d.entryAt(i)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Lookup returns the entry recorded for the address, or found=false.
func (d *Drive) Lookup(addr [20]byte) (Entry, bool, error) {
	for i := uint32(0); i < d.count; i++ {
		e, err := d.entryAt(i)
		if err != nil {
			return Entry{}, false, err
		}
		if e.Address == addr {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Register records a contract (the writer half; the guest's boot is the one
// real writer — spec §2). Identical re-registration is a no-op.
func (d *Drive) Register(addr [20]byte, kind byte, abiJSON []byte) error {
	if e, found, err := d.Lookup(addr); err != nil {
		return err
	} else if found {
		if e.Kind == kind && bytes.Equal(e.Abi, abiJSON) {
			return nil
		}
		return fmt.Errorf("%w: %x", ErrDuplicate, addr)
	}
	if d.count >= d.capacity {
		return ErrRegistryFull
	}
	if uint64(d.heapUsed)+uint64(len(abiJSON)) > uint64(d.heapLength) {
		return ErrHeapFull
	}
	abiOffset := d.heapOffset + d.heapUsed
	if err := d.store.WriteAt(uint64(abiOffset), abiJSON); err != nil {
		return err
	}
	entry := make([]byte, entryLength)
	copy(entry, addr[:])
	entry[20] = kind
	binary.LittleEndian.PutUint32(entry[22:], abiOffset)
	binary.LittleEndian.PutUint32(entry[26:], uint32(len(abiJSON)))
	if err := d.store.WriteAt(uint64(d.indexOffset)+uint64(d.count)*entryLength, entry); err != nil {
		return err
	}
	d.count++
	d.heapUsed += uint32(len(abiJSON))
	counters := make([]byte, 4)
	binary.LittleEndian.PutUint32(counters, d.count)
	if err := d.store.WriteAt(16, counters); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(counters, d.heapUsed)
	return d.store.WriteAt(32, counters)
}
