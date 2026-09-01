package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/host/go/machine"
)

// ErrNoSnapshot means the machine state for a block is no longer available,
// because the block fell outside the snapshot retention window.
var ErrNoSnapshot = errors.New("no machine snapshot for block")

// Inspect answers a read-only query against the machine state as of a block.
//
// The query runs on a fork that is discarded afterwards, so whatever the guest
// does while answering — including writing memory — cannot affect the chain.
// That is what makes this safe to expose publicly and what distinguishes it
// from an input: nothing it emits is provable, and nothing it does is
// committed to.
func (c *Chain) Inspect(ctx context.Context, blockHash common.Hash, query []byte) (*machine.InspectResult, error) {
	// The fork is taken under the lock: without it, pruning could close the
	// snapshot between the lookup and the fork.
	c.mu.RLock()
	m, ok := c.machines[blockHash]
	if !ok {
		c.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrNoSnapshot, blockHash)
	}
	work, err := m.Fork(ctx)
	c.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("forking machine for inspect: %w", err)
	}
	defer work.Close(ctx)

	return work.Inspect(ctx, query, c.cfg.MaxCyclesPerInput)
}

// InspectHead answers a query against the current unsafe head.
func (c *Chain) InspectHead(ctx context.Context, query []byte) (*machine.InspectResult, error) {
	c.mu.RLock()
	head := c.head
	c.mu.RUnlock()
	return c.Inspect(ctx, head, query)
}
