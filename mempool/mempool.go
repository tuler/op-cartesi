// Package mempool is the sequencer's transaction ingress. The OP Stack has no
// public L2 mempool: transactions reach the sequencer through its RPC only,
// so a simple bounded FIFO is sufficient.
package mempool

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const maxTxSize = 128 * 1024

var (
	ErrPoolFull    = errors.New("mempool full")
	ErrKnownTx     = errors.New("transaction already in mempool")
	ErrOversized   = fmt.Errorf("transaction exceeds %d bytes", maxTxSize)
	ErrDepositType = errors.New("deposit transactions cannot be submitted via RPC")
)

type entry struct {
	raw  []byte
	hash common.Hash
}

// Pool is a FIFO of raw, RLP-valid transactions awaiting inclusion.
type Pool struct {
	mu    sync.Mutex
	txs   []entry
	known map[common.Hash]struct{}
	max   int
}

func New(max int) *Pool {
	if max <= 0 {
		max = 4096
	}
	return &Pool{known: make(map[common.Hash]struct{}), max: max}
}

// Add validates and enqueues a raw transaction, returning its hash. The
// transaction must decode as an Ethereum transaction (this is what keeps L2
// blocks parseable by stock op-node and op-batcher); its payload for the
// machine is the raw bytes themselves.
func (p *Pool) Add(raw []byte) (common.Hash, error) {
	if len(raw) > maxTxSize {
		return common.Hash{}, ErrOversized
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, fmt.Errorf("invalid transaction: %w", err)
	}
	if tx.Type() == types.DepositTxType {
		return common.Hash{}, ErrDepositType
	}
	hash := tx.Hash()

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.known[hash]; ok {
		return common.Hash{}, ErrKnownTx
	}
	if len(p.txs) >= p.max {
		return common.Hash{}, ErrPoolFull
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.txs = append(p.txs, entry{raw: cp, hash: hash})
	p.known[hash] = struct{}{}
	return hash, nil
}

// Pending returns the queued transactions in FIFO order.
func (p *Pool) Pending() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.txs))
	for i, e := range p.txs {
		out[i] = e.raw
	}
	return out
}

// Forget drops the given transactions (by hash) from the pool. Called when
// transactions are included in a committed block, or found invalid.
func (p *Pool) Forget(hashes []common.Hash) {
	if len(hashes) == 0 {
		return
	}
	drop := make(map[common.Hash]struct{}, len(hashes))
	for _, h := range hashes {
		drop[h] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.txs[:0]
	for _, e := range p.txs {
		if _, ok := drop[e.hash]; ok {
			delete(p.known, e.hash)
			continue
		}
		kept = append(kept, e)
	}
	p.txs = kept
}

func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.txs)
}
