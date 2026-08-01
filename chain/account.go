package chain

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	drive "github.com/tuler/op-cartesi/accounts-drive/go"
	"github.com/tuler/op-cartesi/machine"
)

// ErrNoAccountsDrive means the guest machine carries no accounts drive —
// either its configuration declares none, or the declared range holds no
// formatted drive. Balances and nonces then simply do not exist as host-
// readable state; callers decide what that means for them (the RPC layer
// serves zeros, since a consumer could not tell an empty ledger from an
// empty account anyway).
var ErrNoAccountsDrive = errors.New("the guest machine has no accounts drive")

const (
	driveUnresolved = iota
	driveFound
	driveAbsent
)

// driveLocator is the optional machine capability of naming where the
// accounts drive starts. Both machine implementations have it: Remote reads
// the label out of machine.get_initial_config, Mock reports its installed
// test image.
type driveLocator interface {
	AccountsDriveStart(ctx context.Context) (start uint64, found bool, err error)
}

// accountsDriveBase resolves — once per process — where the accounts drive
// starts. Presence and absence are both cached: geometry is a deployment
// constant covered by the genesis state root, so neither can change while
// the process runs. Only a transient discovery error is left uncached.
func (c *Chain) accountsDriveBase(ctx context.Context, m machine.Machine) (uint64, error) {
	c.drive.mu.Lock()
	defer c.drive.mu.Unlock()
	switch c.drive.state {
	case driveFound:
		return c.drive.base, nil
	case driveAbsent:
		return 0, ErrNoAccountsDrive
	}
	loc, ok := m.(driveLocator)
	if !ok {
		c.drive.state = driveAbsent
		return 0, ErrNoAccountsDrive
	}
	base, found, err := loc.AccountsDriveStart(ctx)
	if err != nil {
		return 0, fmt.Errorf("discovering the accounts drive: %w", err)
	}
	if !found {
		c.drive.state = driveAbsent
		return 0, ErrNoAccountsDrive
	}
	c.drive.state, c.drive.base = driveFound, base
	return base, nil
}

// AccountAt reads the account record the guest's accounts drive holds for
// addr as of the block: the nonce and the native balance. An absent record
// answers (0, 0, nil) — the standard RPC meaning of an untouched account.
// Blocks outside the snapshot retention window return ErrNoSnapshot, the same
// rule Inspect applies; a guest without a drive returns ErrNoAccountsDrive.
//
// Unlike Inspect this takes no fork: machine.ReadMemory copies bytes out of
// the parked machine without executing it, so the snapshot is left untouched
// by construction rather than by discarding a copy. The read lock is held
// across the reads for the same reason Inspect forks under it — without it,
// pruning could close the machine mid-lookup.
func (c *Chain) AccountAt(ctx context.Context, blockHash common.Hash, addr common.Address) (nonce uint64, balance *big.Int, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.machines[blockHash]
	if !ok {
		return 0, nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}
	base, err := c.accountsDriveBase(ctx, m)
	if err != nil {
		return 0, nil, err
	}
	d, err := drive.Open(&drive.MachineStore{
		Base: base,
		ReadMemory: func(address, length uint64) ([]byte, error) {
			return m.ReadMemory(ctx, address, length)
		},
	})
	if err != nil {
		if errors.Is(err, drive.ErrNotFormatted) {
			// The machine declares a drive but its bytes hold no v1 header —
			// a guest that never formatted it, or a wrong fallback address.
			// Either way there is no ledger to read, which is the same answer
			// as no drive at all.
			return 0, nil, ErrNoAccountsDrive
		}
		return 0, nil, fmt.Errorf("opening the accounts drive: %w", err)
	}
	acct, found, err := d.GetAccount(addr)
	if err != nil {
		return 0, nil, fmt.Errorf("reading account %s: %w", addr, err)
	}
	if !found {
		return 0, new(big.Int), nil
	}
	return acct.Nonce, acct.Balance, nil
}
