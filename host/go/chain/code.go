package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/tuler/op-cartesi/host/go/machine"
	abidrive "github.com/tuler/op-cartesi/lib/abi-drive/go"
	drive "github.com/tuler/op-cartesi/lib/accounts-drive/go"
)

// The guest routes addresses to native code, so there is no EVM bytecode to
// serve — but eth_getCode's real job for tooling is the contract/EOA
// distinction, and wallets and SDKs check exactly that before calling. A
// routed address therefore answers a four-byte marker instead of emptiness:
//
//	0xEF 0xC7 0x51 <kind>
//
// 0xEF because EIP-3541 forbids deployed code starting with it — no real
// EVM contract can collide with the marker — then the 0xC751 brand, then
// the kind: 0x00 system built-in, 0x01 application (both straight from the
// ABI drive, docs/ABI-DRIVE-SPEC.md), 0x02 a registered token's façade
// (derived from the accounts drive's token registry, EVM-COMPAT §6).
const (
	CodeKindSystem = 0x00
	CodeKindApp    = 0x01
	CodeKindToken  = 0x02
)

// CodeMarker is the eth_getCode answer for a routed address of the given
// kind.
func CodeMarker(kind byte) []byte {
	return []byte{0xEF, 0xC7, 0x51, kind}
}

// facadeDomain is the token-façade derivation tag (EVM-COMPAT §6).
const facadeDomain = "ctsi.erc20.v1"

// L2TokenAddress derives a registered token's L2 façade address, exactly as
// the guest does: last20(keccak256("ctsi.erc20.v1" ‖ l1Token)).
func L2TokenAddress(l1Token common.Address) common.Address {
	hash := crypto.Keccak256(append([]byte(facadeDomain), l1Token.Bytes()...))
	return common.BytesToAddress(hash[12:])
}

// abiDriveLocator is the optional machine capability of naming where the
// ABI drive starts; both machine implementations have it.
type abiDriveLocator interface {
	AbiDriveStart(ctx context.Context) (start uint64, found bool, err error)
}

// errNoAbiDrive is internal: CodeAt treats a machine without an ABI drive as
// "no recorded contracts", not as a failure — a consumer could not tell an
// empty registry from a missing drive anyway.
var errNoAbiDrive = errors.New("the guest machine has no abi drive")

// abiDriveBase resolves — once per process, like the accounts drive's — where
// the ABI drive starts.
func (c *Chain) abiDriveBase(ctx context.Context, m machine.Machine) (uint64, error) {
	c.abiDrive.mu.Lock()
	defer c.abiDrive.mu.Unlock()
	switch c.abiDrive.state {
	case driveFound:
		return c.abiDrive.base, nil
	case driveAbsent:
		return 0, errNoAbiDrive
	}
	loc, ok := m.(abiDriveLocator)
	if !ok {
		c.abiDrive.state = driveAbsent
		return 0, errNoAbiDrive
	}
	base, found, err := loc.AbiDriveStart(ctx)
	if err != nil {
		return 0, fmt.Errorf("discovering the abi drive: %w", err)
	}
	if !found {
		c.abiDrive.state = driveAbsent
		return 0, errNoAbiDrive
	}
	c.abiDrive.state, c.abiDrive.base = driveFound, base
	return base, nil
}

// Contract is one routed address the guest serves, as discovery reports it.
type Contract struct {
	Address common.Address
	Kind    byte // CodeKindSystem, CodeKindApp or CodeKindToken
	// Abi is the recorded standard JSON ABI text, straight from the ABI
	// drive. Nil for token façades: their shared ABI is fixed by the
	// standard (EVM-COMPAT §9), not recorded per token.
	Abi []byte
	// L1Token names the registered L1 token a façade serves; nil otherwise.
	L1Token *common.Address
}

// ContractsAt lists everything the guest routes as of the block, from the
// same two sources CodeAt reads: the ABI drive's recorded contracts
// (registration order), then the token façades derived from the accounts
// drive's registry (id order). A machine missing one drive or the other
// simply contributes nothing from it.
func (c *Chain) ContractsAt(ctx context.Context, blockHash common.Hash) ([]Contract, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.machines[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}
	read := func(address, length uint64) ([]byte, error) {
		return m.ReadMemory(ctx, address, length)
	}

	var out []Contract
	if base, err := c.abiDriveBase(ctx, m); err == nil {
		d, err := abidrive.Open(&drive.MachineStore{Base: base, ReadMemory: read})
		if err != nil && !errors.Is(err, abidrive.ErrNotFormatted) {
			return nil, fmt.Errorf("opening the abi drive: %w", err)
		}
		if err == nil {
			entries, err := d.Entries()
			if err != nil {
				return nil, fmt.Errorf("reading the abi drive: %w", err)
			}
			for _, e := range entries {
				out = append(out, Contract{
					Address: common.BytesToAddress(e.Address[:]),
					Kind:    e.Kind,
					Abi:     e.Abi,
				})
			}
		}
	} else if !errors.Is(err, errNoAbiDrive) {
		return nil, err
	}

	base, err := c.accountsDriveBase(ctx, m)
	if err != nil {
		if errors.Is(err, ErrNoAccountsDrive) {
			return out, nil
		}
		return nil, err
	}
	d, err := drive.Open(&drive.MachineStore{Base: base, ReadMemory: read})
	if err != nil {
		if errors.Is(err, drive.ErrNotFormatted) {
			return out, nil
		}
		return nil, fmt.Errorf("opening the accounts drive: %w", err)
	}
	for _, t := range d.Tokens() {
		l1 := common.BytesToAddress(t.Address[:])
		out = append(out, Contract{
			Address: L2TokenAddress(l1),
			Kind:    CodeKindToken,
			L1Token: &l1,
		})
	}
	return out, nil
}

// CodeAt answers eth_getCode for the block: a marker for every address the
// guest routes — recorded contracts out of the ABI drive, token façades
// derived from the accounts drive's registry — and nil for everything else,
// the standard answer for an EOA. Like AccountAt this reads the parked
// machine's memory without executing it; blocks outside the retention
// window return ErrNoSnapshot. A machine without one drive or the other
// simply contributes no markers from it.
func (c *Chain) CodeAt(ctx context.Context, blockHash common.Hash, addr common.Address) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.machines[blockHash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}

	if base, err := c.abiDriveBase(ctx, m); err == nil {
		d, err := abidrive.Open(&drive.MachineStore{
			Base: base,
			ReadMemory: func(address, length uint64) ([]byte, error) {
				return m.ReadMemory(ctx, address, length)
			},
		})
		if err != nil && !errors.Is(err, abidrive.ErrNotFormatted) {
			return nil, fmt.Errorf("opening the abi drive: %w", err)
		}
		if err == nil {
			entry, found, err := d.Lookup(addr)
			if err != nil {
				return nil, fmt.Errorf("reading the abi drive: %w", err)
			}
			if found {
				return CodeMarker(entry.Kind), nil
			}
		}
	} else if !errors.Is(err, errNoAbiDrive) {
		return nil, err
	}

	// Not a recorded contract; a registered token's derived façade answers
	// the token marker (the façades are deliberately absent from the ABI
	// drive — ABI-DRIVE-SPEC §4).
	base, err := c.accountsDriveBase(ctx, m)
	if err != nil {
		if errors.Is(err, ErrNoAccountsDrive) {
			return nil, nil
		}
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
			return nil, nil
		}
		return nil, fmt.Errorf("opening the accounts drive: %w", err)
	}
	for _, t := range d.Tokens() {
		if L2TokenAddress(common.BytesToAddress(t.Address[:])) == addr {
			return CodeMarker(CodeKindToken), nil
		}
	}
	return nil, nil
}
