// Package machine abstracts the Cartesi Machine as the chain's execution
// backend: a deterministic state machine that consumes opaque inputs via the
// CMIO protocol, emits outputs, and commits its full state to a Merkle root
// hash. Implementations must be deterministic: the same sequence of inputs
// applied to the same state must produce the same root hash everywhere.
package machine

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

// CMIO constants mirroring cm.h in cartesi/machine-emulator. The values must
// match the emulator version in use.
const (
	CmioYieldCommandAutomatic uint8 = 0
	CmioYieldCommandManual    uint8 = 1

	CmioYieldAutomaticReasonProgress uint16 = 1
	CmioYieldAutomaticReasonTxOutput uint16 = 2
	CmioYieldAutomaticReasonTxReport uint16 = 4

	CmioYieldManualReasonRxAccepted  uint16 = 1
	CmioYieldManualReasonRxRejected  uint16 = 2
	CmioYieldManualReasonTxException uint16 = 4

	CmioRxRequestAdvanceState uint16 = 0
	CmioRxRequestInspectState uint16 = 1
)

var (
	// ErrHalted means the machine halted instead of yielding for more input;
	// the instance is no longer usable for advancing state.
	ErrHalted = errors.New("machine halted")
	// ErrCycleLimit means the input did not complete within the cycle budget.
	// The implementation guarantees the reported machine state is unchanged
	// only when the caller discards the instance (callers fork before each
	// input and drop the fork on error).
	ErrCycleLimit = errors.New("cycle limit exceeded")
)

// Output is a CMIO emission produced while processing an input (an automatic
// yield with a tx-output or tx-report reason).
//
// The two kinds are not interchangeable. An *output* (voucher or notice) is
// part of the machine's provable result: it is committed to and can be proven
// against the state on L1. A *report* is diagnostic only — explicitly not
// provable — so it must never enter a commitment.
type Output struct {
	Reason uint16
	Data   []byte
}

// IsProvable reports whether this emission is a voucher or notice, i.e. part
// of the committed output set.
func (o Output) IsProvable() bool { return o.Reason == CmioYieldAutomaticReasonTxOutput }

// IsReport reports whether this emission is diagnostic output, which must be
// kept out of every commitment.
func (o Output) IsReport() bool { return o.Reason == CmioYieldAutomaticReasonTxReport }

// AdvanceResult describes the machine's reaction to one input.
type AdvanceResult struct {
	// Accepted is true when the machine ended with an rx-accepted manual
	// yield, false for rx-rejected or tx-exception.
	Accepted bool
	// Cycles is the number of mcycles consumed processing the input. This is
	// the chain's native gas unit.
	Cycles  uint64
	Outputs []Output
}

// Machine is a single machine instance parked at an input-wait yield.
type Machine interface {
	// AdvanceInput feeds one input and runs the machine until it yields
	// waiting for the next input, collecting any outputs emitted along the
	// way. maxCycles bounds execution; exceeding it returns ErrCycleLimit.
	AdvanceInput(ctx context.Context, input []byte, maxCycles uint64) (*AdvanceResult, error)

	// RootHash returns the Merkle root of the machine's entire state.
	RootHash(ctx context.Context) (common.Hash, error)

	// Fork snapshots the machine into an independent copy that shares no
	// mutable state with the receiver. Both remain usable.
	Fork(ctx context.Context) (Machine, error)

	// Close releases the instance. The machine must not be used afterwards.
	Close(ctx context.Context) error
}
