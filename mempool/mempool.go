// Package mempool is the sequencer's transaction ingress. The OP Stack has no
// public L2 mempool: transactions reach the sequencer through its RPC only,
// so a simple bounded FIFO is sufficient.
package mempool

import (
	"errors"
	"fmt"
	"math/big"
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
	// ErrInvalidSender means the gated pool could not recover a signer with
	// the chain's signer — an unsigned transaction, or one signed for another
	// chain. The guest would reject it for the same reason.
	ErrInvalidSender = errors.New("transaction sender cannot be recovered")
	// ErrNonceTooLow means the transaction's nonce is already consumed on the
	// sender's account record at the chain head.
	ErrNonceTooLow = errors.New("transaction nonce below the sender's account nonce")
	// ErrKnownNonce means a different transaction from the same sender with
	// the same nonce is already queued; only one of them could ever execute.
	ErrKnownNonce = errors.New("a transaction with this sender and nonce is already queued")
)

// senderNonce identifies the one slot a transaction competes for: an
// account's next nonce.
type senderNonce struct {
	sender common.Address
	nonce  uint64
}

type entry struct {
	raw  []byte
	hash common.Hash
	// sn is meaningful only when gated: the recovered sender and nonce this
	// entry occupies in byNonce.
	sn    senderNonce
	gated bool
}

// Pool is a FIFO of raw, RLP-valid transactions awaiting inclusion.
type Pool struct {
	mu    sync.Mutex
	txs   []entry
	known map[common.Hash]struct{}
	// byNonce holds the (sender, nonce) pairs of gated entries, so two
	// transactions competing for the same account slot cannot both queue.
	byNonce map[senderNonce]struct{}
	max     int
	// signer and nonce arm the nonce gate (SetNonceGate); both nil means no
	// gating, which is the pre-accounts-drive behavior.
	signer types.Signer
	nonce  func(common.Address) (uint64, error)
}

func New(max int) *Pool {
	if max <= 0 {
		max = 4096
	}
	return &Pool{
		known:   make(map[common.Hash]struct{}),
		byNonce: make(map[senderNonce]struct{}),
		max:     max,
	}
}

// SetNonceGate arms nonce-based admission: transactions must recover a
// sender under the chain's signer, must not carry a nonce the sender's
// account has already consumed (read through the hook — the caller wires it
// to the chain's account state at head, i.e. the guest's accounts drive),
// and must not duplicate a (sender, nonce) already queued.
//
// This is a courtesy filter and a DoS bound at the RPC ingress, NOT the
// enforcement: the authoritative nonce check lives in the guest, where it is
// part of the proven state transition (docs/ACCOUNTS.md §6.2). A transaction
// that slips past the gate — a nonce gap, a reorg, a race — is still
// rejected by the machine at inclusion and dropped from the pool then.
//
// A nil hook leaves the pool ungated. Call before serving requests; the
// gate is not designed to be flipped concurrently with Add.
func (p *Pool) SetNonceGate(chainID *big.Int, nonce func(common.Address) (uint64, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signer = types.LatestSignerForChainID(chainID)
	p.nonce = nonce
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
	signer, nonceAt := p.signer, p.nonce
	p.mu.Unlock()

	// The gate's reads run outside p.mu on purpose: the nonce hook reaches
	// into the chain (machine memory at head), and the chain calls Forget
	// while holding its own lock — reading under p.mu would be a lock-order
	// inversion away from a deadlock.
	var sn senderNonce
	gated := nonceAt != nil
	if gated {
		from, err := types.Sender(signer, tx)
		if err != nil {
			return common.Hash{}, fmt.Errorf("%w: %v", ErrInvalidSender, err)
		}
		current, err := nonceAt(from)
		if err != nil {
			return common.Hash{}, fmt.Errorf("reading account nonce of %s: %w", from, err)
		}
		if tx.Nonce() < current {
			return common.Hash{}, fmt.Errorf("%w: tx nonce %d, account nonce %d", ErrNonceTooLow, tx.Nonce(), current)
		}
		sn = senderNonce{sender: from, nonce: tx.Nonce()}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.known[hash]; ok {
		return common.Hash{}, ErrKnownTx
	}
	if gated {
		if _, ok := p.byNonce[sn]; ok {
			return common.Hash{}, fmt.Errorf("%w: sender %s, nonce %d", ErrKnownNonce, sn.sender, sn.nonce)
		}
	}
	if len(p.txs) >= p.max {
		return common.Hash{}, ErrPoolFull
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.txs = append(p.txs, entry{raw: cp, hash: hash, sn: sn, gated: gated})
	p.known[hash] = struct{}{}
	if gated {
		p.byNonce[sn] = struct{}{}
	}
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
			if e.gated {
				delete(p.byNonce, e.sn)
			}
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
