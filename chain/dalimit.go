package chain

import (
	"math/big"
	"sync/atomic"
)

// daLimits is the sequencer's data-availability backpressure, set by
// op-batcher through miner_setMaxDASize.
//
// When batches are backing up on L1, the batcher asks the sequencer to shrink
// the blocks it produces. Honouring that is what keeps the unsafe chain from
// outrunning what can actually be posted; a sequencer that ignored it would
// keep building blocks the batcher can never catch up with.
//
// The limits are advisory in one direction only. Deposits are forced into the
// block by op-node and are never dropped — throttling may only shed
// transactions from the mempool, which is the sequencer's own discretionary
// content.
type daLimits struct {
	// maxTxSize is the largest single transaction admitted, in bytes.
	maxTxSize atomic.Uint64
	// maxBlockSize is the largest total transaction payload per block.
	maxBlockSize atomic.Uint64
}

// SetMaxDASize records new limits. Zero means unlimited, matching op-geth.
func (c *Chain) SetMaxDASize(maxTxSize, maxBlockSize *big.Int) {
	c.da.maxTxSize.Store(clampToUint64(maxTxSize))
	c.da.maxBlockSize.Store(clampToUint64(maxBlockSize))
}

// MaxDASize reports the current limits, for tests and diagnostics.
func (c *Chain) MaxDASize() (maxTxSize, maxBlockSize uint64) {
	return c.da.maxTxSize.Load(), c.da.maxBlockSize.Load()
}

func clampToUint64(v *big.Int) uint64 {
	if v == nil || v.Sign() <= 0 {
		return 0
	}
	if !v.IsUint64() {
		return 0 // beyond any real limit, so treat as unlimited
	}
	return v.Uint64()
}

// admits reports whether a pool transaction of the given size still fits,
// given how much of the block's budget is already used.
func (c *Chain) admits(used, size uint64) bool {
	if max := c.da.maxTxSize.Load(); max != 0 && size > max {
		return false
	}
	if max := c.da.maxBlockSize.Load(); max != 0 && used+size > max {
		return false
	}
	return true
}
