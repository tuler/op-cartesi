package chain

import (
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/machine"
)

// TxOutputs records what the machine emitted while processing one
// transaction of a block.
//
// The split between Outputs and Reports is the Cartesi provability boundary,
// and it decides what may ever be committed to:
//
//   - Outputs (vouchers and notices) are provable. They are the raw material
//     for the block's outputs commitment, which is the analogue of the OP
//     Stack's messagePasserStorageRoot, and hence for withdrawals.
//   - Reports are diagnostic and explicitly not provable. They are served to
//     users for debugging and may inform a synthesized receipt's failure
//     detail, but they must never enter a Merkle root or a header field.
//
// Outputs of a rejected input are dropped: a rejection rolls the machine back,
// so the emissions never became part of the state. Reports are kept even for
// rejected inputs, because a rejected input's report is usually the only
// explanation of why it failed.
type TxOutputs struct {
	// TxIndex is the transaction's position within the block.
	TxIndex int
	// TxHash is the transaction hash, or the zero hash if the transaction did
	// not decode.
	TxHash common.Hash
	// Accepted is false when the machine rejected the input, meaning it had no
	// effect on state.
	Accepted bool
	// Cycles consumed processing this input.
	Cycles uint64
	// Outputs are the provable emissions (vouchers, notices), in emission
	// order. Empty when the input was rejected.
	Outputs [][]byte
	// Reports are the non-provable diagnostic emissions, in emission order.
	Reports [][]byte
}

// collectOutputs splits one advance result into its provable and diagnostic
// parts, applying the rejection rule described on TxOutputs.
func collectOutputs(index int, tx []byte, res *machine.AdvanceResult) TxOutputs {
	out := TxOutputs{
		TxIndex:  index,
		Accepted: res.Accepted,
		Cycles:   res.Cycles,
	}
	if h, err := txHash(tx); err == nil {
		out.TxHash = h
	}
	for _, emission := range res.Outputs {
		switch {
		case emission.IsProvable():
			if res.Accepted {
				out.Outputs = append(out.Outputs, emission.Data)
			}
		case emission.IsReport():
			out.Reports = append(out.Reports, emission.Data)
		}
	}
	return out
}

// outputLeaves flattens a block's provable outputs into tree leaves, in the
// order the machine emitted them. Reports are excluded by construction: they
// never reach TxOutputs.Outputs, so they cannot influence the commitment.
func outputLeaves(txs []TxOutputs) []common.Hash {
	var leaves []common.Hash
	for _, tx := range txs {
		for _, output := range tx.Outputs {
			leaves = append(leaves, OutputLeaf(output))
		}
	}
	return leaves
}

// BlockOutputs returns the per-transaction machine emissions recorded for a
// block, or nil if the block is unknown or its outputs were pruned.
func (c *Chain) BlockOutputs(hash common.Hash) []TxOutputs {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.outputs[hash]
}

// OutputTreeAt returns the cumulative outputs accumulator as of a block. The
// second result is false if the block is unknown. Its root is what the block
// commits to in the header's withdrawals root under Isthmus, and what an
// output proof for any output produced up to this block verifies against.
func (c *Chain) OutputTreeAt(hash common.Hash) (OutputTree, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tree, ok := c.trees[hash]
	return tree, ok
}
