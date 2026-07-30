package machine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	// OutputFn, when set, produces the CMIO emissions for an input. It must be
	// a pure function of the input for the mock to stay deterministic.
	OutputFn func(input []byte) []Output
	// InspectFn, when set, answers read-only queries. Defaults to echoing the
	// query back as a single report.
	InspectFn func(query []byte) [][]byte
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
	var outputs []Output
	if m.OutputFn != nil {
		outputs = m.OutputFn(input)
	}
	// A rejected input still emits whatever it produced before rejecting; it
	// is the caller's job to discard the provable part, as a real machine's
	// rollback would.
	if m.RejectFn != nil && m.RejectFn(input) {
		return &AdvanceResult{Accepted: false, Cycles: cycles, Outputs: outputs}, nil
	}
	m.root = crypto.Keccak256Hash(m.root.Bytes(), input)
	return &AdvanceResult{Accepted: true, Cycles: cycles, Outputs: outputs}, nil
}

// Inspect answers a query. The mock never advances its root, so it cannot
// detect a caller that forgot to fork first — a real machine would run guest
// code here and could write memory, which is why the interface puts that
// obligation on the caller.
func (m *Mock) Inspect(_ context.Context, query []byte, maxCycles uint64) (*InspectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cycles := uint64(500 + 5*len(query))
	if cycles > maxCycles {
		return nil, ErrCycleLimit
	}
	if m.RejectFn != nil && m.RejectFn(query) {
		return &InspectResult{Accepted: false, Cycles: cycles}, nil
	}
	reports := [][]byte{query}
	if m.InspectFn != nil {
		reports = m.InspectFn(query)
	}
	return &InspectResult{Accepted: true, Cycles: cycles, Reports: reports}, nil
}

func (m *Mock) RootHash(context.Context) (common.Hash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root, nil
}

func (m *Mock) Fork(context.Context) (Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &Mock{
		root:      m.root,
		RejectFn:  m.RejectFn,
		CycleCost: m.CycleCost,
		OutputFn:  m.OutputFn,
		InspectFn: m.InspectFn,
	}, nil
}

// Store writes the mock's whole state — its root hash — to a file, which is
// enough for the checkpoint machinery to be exercised without an emulator.
func (m *Mock) Store(_ context.Context, directory string) error {
	m.mu.Lock()
	root := m.root
	m.mu.Unlock()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, mockStateFile), root.Bytes(), 0o644)
}

// LoadMock reads back a machine written by Mock.Store.
func LoadMock(directory string) (*Mock, error) {
	data, err := os.ReadFile(filepath.Join(directory, mockStateFile))
	if err != nil {
		return nil, err
	}
	if len(data) != common.HashLength {
		return nil, fmt.Errorf("%s holds %d bytes, want a %d-byte root", directory, len(data), common.HashLength)
	}
	return &Mock{root: common.BytesToHash(data)}, nil
}

const mockStateFile = "mock-root"

func (m *Mock) Close(context.Context) error { return nil }
