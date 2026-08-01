package chain

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/machine"
)

// proofPageLog2 is the proof granularity: one 4 KiB machine page, the same
// unit the spec's read path reasons in (ACCOUNTS-DRIVE-SPEC.md §11–§12). A
// page proof against the machine root carries 64 − 12 = 52 siblings.
const proofPageLog2 = 12

// proofReader is the optional machine capability of proving memory ranges
// (machine.get_proof) — the same shape as the driveLocator capability above
// it in account.go. Remote implements it for real; Mock implements it by
// returning machine.ErrProofsUnsupported, so the walk half of an account
// proof stays testable without an emulator while the proof half fails loudly.
type proofReader interface {
	GetProof(ctx context.Context, address, log2TargetSize uint64) (*machine.MerkleProof, error)
}

// ProvenPage is one 4 KiB page of the accounts drive: where it is (both
// drive-relative and as an absolute machine address), its bytes, and — when
// proofs were requested — its machine proof at log2 size 12 against the
// block's state root.
type ProvenPage struct {
	DriveOffset uint64
	Address     uint64
	Data        []byte
	Proof       *machine.MerkleProof
}

// AccountProof is the provable account read behind cartesi_getAccountProof:
// the record (or its provable absence), the robin-hood probe walk that
// establishes it, and the drive pages a verifier needs to replay that walk
// inside proven bytes (ACCOUNTS-DRIVE-SPEC.md §12). Config carries the
// header's deployment constants — seed, capacities, offsets, slot sizes —
// which are the §12 step-1 inputs; the header page itself is included (and
// proven) so a verifier need not take them on faith.
type AccountProof struct {
	DriveBase uint64
	Config    drive.Config

	Found     bool
	Nonce     uint64
	Balance   *big.Int
	Record    []byte // the raw slot bytes, when Found
	SlotIndex uint64 // the record's slot, when Found

	// HomeSlot and WalkLength describe the probe: slots
	// home … home+WalkLength−1 (cyclic) were examined, the last being the
	// record itself, an empty slot, or the early-terminating record whose
	// displacement proves absence (spec §6.2, §6.6).
	HomeSlot   uint64
	WalkLength uint64

	HeaderPage ProvenPage
	// WalkPages are the 4 KiB pages covering every slot the walk examined,
	// in ascending drive offset (a wrap of the table end contributes pages
	// from both ends).
	WalkPages []ProvenPage
}

// AccountProofAt assembles the provable account read for addr at a block.
// The record, walk and page contents come from ReadMemory against the
// block's parked machine — no fork, no execution, same as AccountAt. When
// withProofs is set, each page (header included) additionally carries a
// machine.get_proof against the block's state root; a machine that cannot
// prove (the mock) fails the whole call then, which is what the RPC wants —
// a proof endpoint must not answer without proofs.
func (c *Chain) AccountProofAt(ctx context.Context, blockHash common.Hash, addr common.Address, withProofs bool) (*AccountProof, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.blocks[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}
	m, ok := c.machines[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}
	base, err := c.accountsDriveBase(ctx, m)
	if err != nil {
		return nil, err
	}
	d, err := drive.Open(&drive.MachineStore{
		Base: base,
		ReadMemory: func(address, length uint64) ([]byte, error) {
			return m.ReadMemory(ctx, address, length)
		},
	})
	if err != nil {
		if errors.Is(err, drive.ErrNotFormatted) {
			return nil, ErrNoAccountsDrive
		}
		return nil, fmt.Errorf("opening the accounts drive: %w", err)
	}
	cfg := d.Config()
	out := &AccountProof{DriveBase: base, Config: cfg}

	// Replay the lookup of spec §6.2 slot by slot, keeping what the library's
	// GetAccount does not expose: which slots the walk examined. The walk is
	// what a verifier re-runs inside the proven pages, so the two must agree —
	// same seeded-keccak home, same displacement rule, same termination.
	seed := cfg.Seed[:]
	capacity, slotSize := cfg.Capacity, uint64(cfg.SlotSize)
	home := func(key []byte) uint64 {
		h := drive.Keccak256(seed, key)
		return binary.LittleEndian.Uint64(h[:8]) % capacity
	}
	out.HomeSlot = home(addr[:])
	for dist := uint64(0); dist < capacity; dist++ {
		i := (out.HomeSlot + dist) % capacity
		slot, err := m.ReadMemory(ctx, base+cfg.TableOffset+slotSize*i, slotSize)
		if err != nil {
			return nil, fmt.Errorf("reading slot %d: %w", i, err)
		}
		out.WalkLength = dist + 1
		key := slot[:20]
		if isZeroBytes(key) {
			break // empty slot: addr is absent, provably (spec §6.6)
		}
		if bytes.Equal(key, addr[:]) {
			out.Found, out.SlotIndex, out.Record = true, i, slot
			out.Nonce, out.Balance = decodeAccountSlot(cfg, slot)
			break
		}
		if (i+capacity-home(key))%capacity < dist {
			break // early termination: addr is absent, provably (spec §6.6)
		}
	}
	if !out.Found {
		out.Balance = new(big.Int)
	}

	// The pages: the header (the walk's constants live there), then every
	// page covering a walked slot. The byte range home…home+len is contiguous
	// unless it wraps the table end, in which case it contributes pages from
	// both ends; pages are deduplicated and served in ascending offset.
	pageOffsets := map[uint64]struct{}{}
	addRange := func(firstSlot, slots uint64) {
		start := cfg.TableOffset + slotSize*firstSlot
		end := start + slotSize*slots
		for p := start &^ ((1 << proofPageLog2) - 1); p < end; p += 1 << proofPageLog2 {
			pageOffsets[p] = struct{}{}
		}
	}
	if wrap := out.HomeSlot + out.WalkLength; wrap > capacity {
		addRange(out.HomeSlot, capacity-out.HomeSlot)
		addRange(0, wrap-capacity)
	} else {
		addRange(out.HomeSlot, out.WalkLength)
	}
	offsets := make([]uint64, 0, len(pageOffsets))
	for off := range pageOffsets {
		offsets = append(offsets, off)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })

	readPage := func(off uint64) (ProvenPage, error) {
		data, err := m.ReadMemory(ctx, base+off, 1<<proofPageLog2)
		if err != nil {
			return ProvenPage{}, fmt.Errorf("reading drive page %#x: %w", off, err)
		}
		return ProvenPage{DriveOffset: off, Address: base + off, Data: data}, nil
	}
	if out.HeaderPage, err = readPage(0); err != nil {
		return nil, err
	}
	for _, off := range offsets {
		page, err := readPage(off)
		if err != nil {
			return nil, err
		}
		out.WalkPages = append(out.WalkPages, page)
	}

	if withProofs {
		prover, ok := m.(proofReader)
		if !ok {
			return nil, machine.ErrProofsUnsupported
		}
		prove := func(p *ProvenPage) error {
			proof, err := prover.GetProof(ctx, p.Address, proofPageLog2)
			if err != nil {
				return fmt.Errorf("proving drive page %#x: %w", p.DriveOffset, err)
			}
			// The machine parked at this block is the block's post-state, so
			// the proof's root must be the header's stateRoot — anything else
			// means the snapshot and the chain disagree, which must not be
			// served as a proof.
			if proof.RootHash != b.Header.Root {
				return fmt.Errorf("proof root %s does not match block state root %s", proof.RootHash, b.Header.Root)
			}
			p.Proof = proof
			return nil
		}
		if err := prove(&out.HeaderPage); err != nil {
			return nil, err
		}
		for i := range out.WalkPages {
			if err := prove(&out.WalkPages[i]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func isZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// decodeAccountSlot reads nonce and balance out of a live slot, for both
// record layouts of spec §7 (the standard 64-byte record, whose prefix a
// wide slot shares, and the compact 32-byte record).
func decodeAccountSlot(cfg drive.Config, slot []byte) (uint64, *big.Int) {
	if cfg.Profile == drive.ProfileSingleAsset && cfg.SlotSize == 32 {
		return uint64(binary.LittleEndian.Uint32(slot[20:24])), new(big.Int).SetBytes(slot[24:32])
	}
	return binary.LittleEndian.Uint64(slot[24:32]), new(big.Int).SetBytes(slot[32:64])
}
