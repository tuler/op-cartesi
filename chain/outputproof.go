package chain

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// OutputProof is Cartesi's OutputValidityProof: the position of an output in
// the chain-wide outputs tree, and the sibling at each level on the path to
// the root.
//
// It is the proof Application.executeOutput takes, and the reason the tree in
// outputtree.go is shaped the way it is. Nothing here is op-cartesi-specific:
// a proof produced by this package is the same object a Cartesi node would
// produce, and LibOutputValidityProof verifies it the same way.
type OutputProof struct {
	// OutputIndex is the output's chain-wide index — its leaf position.
	OutputIndex uint64
	// Siblings are the co-path nodes, leaf level first, one per level. Always
	// OutputTreeHeight long, because the tree's height is fixed.
	Siblings []common.Hash
}

// ProveOutput builds the proof for one leaf against the tree holding leaves.
//
// This mirrors LibMerkle32.siblings: at each level take the node at index^1,
// defaulting to that level's zero node when the level is short, then lift the
// level and halve the index. The default node is what makes a 63-level proof
// cheap for a tree holding a handful of outputs — every level above the real
// leaves contributes a precomputed zero.
func ProveOutput(leaves []common.Hash, index uint64) (*OutputProof, error) {
	if index >= uint64(len(leaves)) {
		return nil, fmt.Errorf("output %d does not exist; the tree holds %d", index, len(leaves))
	}
	siblings := make([]common.Hash, 0, OutputTreeHeight)
	level, cursor := leaves, index
	for h := 0; h < OutputTreeHeight; h++ {
		siblings = append(siblings, nodeAt(level, cursor^1, zeroHashes[h]))
		level = parentLevel(level, zeroHashes[h])
		cursor >>= 1
	}
	return &OutputProof{OutputIndex: index, Siblings: siblings}, nil
}

// nodeAt is LibMerkle32.at: the node, or the level's default beyond the end.
func nodeAt(nodes []common.Hash, index uint64, defaultNode common.Hash) common.Hash {
	if index < uint64(len(nodes)) {
		return nodes[index]
	}
	return defaultNode
}

// parentLevel is LibMerkle32.parentLevel: pair the level up, padding an odd
// tail with the level's default node.
func parentLevel(nodes []common.Hash, defaultNode common.Hash) []common.Hash {
	if len(nodes) == 0 {
		return nil
	}
	n := (len(nodes) + 1) / 2
	level := make([]common.Hash, n)
	for i := range n {
		level[i] = hashPair(nodes[2*i], nodeAt(nodes, uint64(2*i+1), defaultNode))
	}
	return level
}

// RootAfterReplacement recomputes the tree root from a proof and a leaf, which
// is what LibMerkle32.merkleRootAfterReplacement does on chain. Having it here
// lets the node check a proof it just produced before handing it out.
func RootAfterReplacement(siblings []common.Hash, index uint64, leaf common.Hash) common.Hash {
	for _, sibling := range siblings {
		if index&1 == 0 {
			leaf = hashPair(leaf, sibling)
		} else {
			leaf = hashPair(sibling, leaf)
		}
		index >>= 1
	}
	return leaf
}

// OutputAt names one output of the chain by its chain-wide index.
type OutputAt struct {
	// Output is the raw bytes the machine emitted, which is what
	// executeOutput takes and what the leaf hashes.
	Output []byte
	// BlockHash is the block whose execution produced it.
	BlockHash common.Hash
	// BlockNumber of that block.
	BlockNumber uint64
	// TxHash of the transaction that produced it.
	TxHash common.Hash
}

// OutputProofAt builds the proof for a chain-wide output index against the
// outputs commitment as of a block — which is the root a proposal for that
// block committed to.
//
// The proof is against a *later* state of the tree than the one that existed
// when the output was emitted, and that is the point: a withdrawal has to stay
// provable against the commitment of whatever block was proposed, not only
// against the block that produced it.
func (c *Chain) OutputProofAt(index uint64, blockHash common.Hash) (*OutputAt, *OutputProof, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	target, ok := c.blocks[blockHash]
	if !ok {
		return nil, nil, fmt.Errorf("unknown block %s", blockHash)
	}
	leaves, found, err := c.leavesThrough(target)
	if err != nil {
		return nil, nil, err
	}
	if index >= uint64(len(leaves)) {
		return nil, nil, fmt.Errorf("output %d does not exist as of block %d; %d outputs so far",
			index, target.NumberU64(), len(leaves))
	}
	proof, err := ProveOutput(leaves, index)
	if err != nil {
		return nil, nil, err
	}
	// A proof that does not reproduce the committed root is a bug here, not a
	// caller's problem, so it is caught before it is served.
	if got := RootAfterReplacement(proof.Siblings, index, leaves[index]); target.Header.WithdrawalsHash == nil || got != *target.Header.WithdrawalsHash {
		return nil, nil, fmt.Errorf("built a proof that reproduces %s, but block %d commits to %v",
			got, target.NumberU64(), target.Header.WithdrawalsHash)
	}
	return found[index], proof, nil
}

// leavesThrough collects every output leaf from genesis through a block, in
// index order, together with what produced each one.
//
// It walks the chain rather than keeping a flat list, so the cost is linear in
// the chain's length. That is fine while outputs are rare and proofs are
// occasional; a chain that emits many will want an index from output index to
// block, which is a store change rather than a protocol one.
func (c *Chain) leavesThrough(target *Block) ([]common.Hash, []*OutputAt, error) {
	var chain []*Block
	for b := target; b != nil; {
		chain = append(chain, b)
		if b.NumberU64() == 0 {
			break
		}
		parent, ok := c.blocks[b.Header.ParentHash]
		if !ok {
			if c.store == nil {
				return nil, nil, fmt.Errorf("block %d is not in memory and there is no store to read it from", b.NumberU64()-1)
			}
			parent = c.store.Block(b.Header.ParentHash)
			if parent == nil {
				return nil, nil, fmt.Errorf("block %s is missing", b.Header.ParentHash)
			}
		}
		b = parent
	}

	var leaves []common.Hash
	var found []*OutputAt
	for i := len(chain) - 1; i >= 0; i-- {
		b := chain[i]
		outputs, ok := c.outputs[b.Hash()]
		if !ok && c.store != nil {
			stored, _, _, err := c.store.Records(b.Hash())
			if err == nil {
				outputs = stored
			} else if b.NumberU64() != 0 {
				return nil, nil, fmt.Errorf("no output records for block %d", b.NumberU64())
			}
		}
		for _, tx := range outputs {
			for _, output := range tx.Outputs {
				leaves = append(leaves, OutputLeaf(output))
				found = append(found, &OutputAt{
					Output:      output,
					BlockHash:   b.Hash(),
					BlockNumber: b.NumberU64(),
					TxHash:      tx.TxHash,
				})
			}
		}
	}
	return leaves, found, nil
}
