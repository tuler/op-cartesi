package machine

import (
	"context"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Mock is a deterministic in-memory stand-in for a Cartesi Machine, used for
// development and tests. Its "state" is a single 32-byte hash advanced as
// root' = keccak256(root || input), which preserves the property the chain
// relies on: replaying the same inputs from the same state yields the same
// root hash.
type Mock struct {
	mu   sync.Mutex
	root common.Hash

	// RejectFn, when set, marks inputs the mock refuses (rx-rejected).
	// Rejected inputs leave the state untouched.
	RejectFn func(input []byte) bool
	// CycleCost computes the cycle charge of an input. Defaults to
	// 1000 + 10*len(input).
	CycleCost func(input []byte) uint64
}

var _ Machine = (*Mock)(nil)

// NewMock creates a mock machine whose initial root is keccak256(seed).
func NewMock(seed []byte) *Mock {
	return &Mock{root: crypto.Keccak256Hash(seed)}
}

func (m *Mock) AdvanceInput(_ context.Context, input []byte, maxCycles uint64) (*AdvanceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cycles := uint64(1000 + 10*len(input))
	if m.CycleCost != nil {
		cycles = m.CycleCost(input)
	}
	if cycles > maxCycles {
		return nil, ErrCycleLimit
	}
	if m.RejectFn != nil && m.RejectFn(input) {
		return &AdvanceResult{Accepted: false, Cycles: cycles}, nil
	}
	m.root = crypto.Keccak256Hash(m.root.Bytes(), input)
	return &AdvanceResult{Accepted: true, Cycles: cycles}, nil
}

func (m *Mock) RootHash(context.Context) (common.Hash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root, nil
}

func (m *Mock) Fork(context.Context) (Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &Mock{root: m.root, RejectFn: m.RejectFn, CycleCost: m.CycleCost}, nil
}

func (m *Mock) Close(context.Context) error { return nil }
