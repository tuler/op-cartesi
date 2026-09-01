package chain

import (
	"math/big"
	"testing"
)

// op-batcher shrinks blocks by lowering these limits when batches back up on
// L1. The sequencer must actually honour them, or the unsafe chain outruns
// what can be posted.
func TestDALimitThrottlesPoolTransactions(t *testing.T) {
	c, pool := newIsthmusChain(t, "seed")

	raw := userTx(t, 0, []byte("a reasonably sized user transaction payload"))
	if _, err := pool.Add(raw); err != nil {
		t.Fatal(err)
	}

	// A block-size budget smaller than the transaction leaves it in the pool.
	c.SetMaxDASize(big.NewInt(0), big.NewInt(1))
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, false))
	if got := len(env.ExecutionPayload.Transactions); got != 1 {
		t.Fatalf("%d transactions, want the deposit only while throttled", got)
	}
	if pool.Len() != 1 {
		t.Error("the throttled transaction was dropped rather than deferred")
	}

	// Lifting the limit lets it in on the next block.
	c.SetMaxDASize(big.NewInt(0), big.NewInt(0))
	env = buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "next")}, false))
	if got := len(env.ExecutionPayload.Transactions); got != 2 {
		t.Fatalf("%d transactions, want deposit + the released transaction", got)
	}
	if pool.Len() != 0 {
		t.Error("the transaction was not taken from the pool once unthrottled")
	}
}

// Deposits are forced into the block by op-node, so throttling must never
// shed them however tight the budget.
func TestDALimitNeverDropsDeposits(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	c.SetMaxDASize(big.NewInt(1), big.NewInt(1))

	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))
	if got := len(env.ExecutionPayload.Transactions); got != 1 {
		t.Fatalf("%d transactions, want the deposit included despite the limit", got)
	}
}

// A per-transaction cap rejects an oversized transaction without blocking
// smaller ones behind it.
func TestDALimitPerTransactionCap(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	c.SetMaxDASize(big.NewInt(10), big.NewInt(0))
	if !c.admits(0, 5) {
		t.Error("a small transaction was rejected under a 10-byte cap")
	}
	if c.admits(0, 50) {
		t.Error("an oversized transaction was admitted")
	}

	// Zero means unlimited, matching op-geth.
	c.SetMaxDASize(big.NewInt(0), big.NewInt(0))
	if !c.admits(1<<30, 1<<30) {
		t.Error("zero limits should mean unlimited")
	}
	maxTx, maxBlock := c.MaxDASize()
	if maxTx != 0 || maxBlock != 0 {
		t.Errorf("limits read back as %d/%d, want 0/0", maxTx, maxBlock)
	}
}
