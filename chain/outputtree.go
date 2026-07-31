package chain

import (
	"math/bits"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// OutputTreeHeight is the height of the outputs Merkle tree. It mirrors
// CanonicalMachine.LOG2_MAX_OUTPUTS in cartesi/rollups-contracts, so the roots
// this package computes are the ones Cartesi's on-chain output validation
// (Application.validateOutput and LibOutputValidityProof) already knows how to
// verify — existing voucher proofs and tooling carry over unchanged.
const OutputTreeHeight = 63

// OutputLeaf hashes an output blob into its tree leaf. Cartesi's
// Application.validateOutput hashes the output bytes with keccak256 before
// verifying the proof, so the leaf is keccak256(output).
func OutputLeaf(output []byte) common.Hash {
	return crypto.Keccak256Hash(output)
}

// hashPair is LibKeccak256.hashPair: keccak256(left || right).
func hashPair(left, right common.Hash) common.Hash {
	return crypto.Keccak256Hash(left.Bytes(), right.Bytes())
}

// zeroHashes[h] is the root of an all-zero subtree of height h — the default
// node of level h, lifted level by level: the level-0 default is the zero hash,
// and each level up is hashPair(default, default).
var zeroHashes = func() [OutputTreeHeight + 1]common.Hash {
	var z [OutputTreeHeight + 1]common.Hash
	for h := 1; h <= OutputTreeHeight; h++ {
		z[h] = hashPair(z[h-1], z[h-1])
	}
	return z
}()

// OutputTree is an append-only Merkle tree of output hashes, matching the
// fixed-height zero-padded tree Cartesi's contracts verify against.
//
// The tree is cumulative over the whole chain, not per block. That is required
// on both sides: Cartesi indexes outputs globally, and the OP Stack's
// messagePasserStorageRoot is the state of all withdrawals up to a block, so a
// withdrawal must stay provable against the output root of any later block.
//
// Only the tree's frontier is kept — the pending left-hand node at each level —
// which is what makes carrying the accumulator from block to block cheap. The
// zero value is a valid empty tree.
type OutputTree struct {
	count uint64
	// branch[h] is the pending node at level h. It is meaningful exactly when
	// bit h of count is set, so the slice is trimmed to the significant bits
	// of count rather than always being OutputTreeHeight long.
	branch []common.Hash
}

// NewOutputTree returns an empty tree.
func NewOutputTree() OutputTree { return OutputTree{} }

// Count returns how many outputs have been appended.
func (t OutputTree) Count() uint64 { return t.count }

// Append adds output hashes to the tree, returning the extended tree. The
// receiver is not modified, so a tree can be carried across a fork safely.
func (t OutputTree) Append(leaves ...common.Hash) OutputTree {
	if len(leaves) == 0 {
		return t
	}
	next := OutputTree{count: t.count, branch: slices.Clone(t.branch)}
	for _, leaf := range leaves {
		next.insert(leaf)
	}
	return next
}

// insert carries one leaf up the frontier: while the level below is already
// full (its bit in the current count is set), combine and carry; otherwise
// park the node at this level.
func (t *OutputTree) insert(leaf common.Hash) {
	node := leaf
	size := t.count
	for h := 0; h < OutputTreeHeight; h++ {
		if size&1 == 0 {
			for len(t.branch) <= h {
				t.branch = append(t.branch, common.Hash{})
			}
			t.branch[h] = node
			t.count++
			return
		}
		node = hashPair(t.branch[h], node)
		size >>= 1
	}
	// Unreachable for any realistic output count: it would require 2^63
	// outputs, which exceeds the tree's capacity.
	panic("output tree is full")
}

// Root returns the root of the fixed-height tree, padding the unfilled right
// side with zero subtrees. TestOutputTreeMatchesSolidity pins the result
// against Cartesi's on-chain verifier.
func (t OutputTree) Root() common.Hash {
	node := common.Hash{}
	size := t.count
	for h := 0; h < OutputTreeHeight; h++ {
		if size&1 == 1 {
			node = hashPair(t.branch[h], node)
		} else {
			node = hashPair(node, zeroHashes[h])
		}
		size >>= 1
	}
	return node
}

// trimmedLen is the number of frontier levels that Root can read, given the
// count. Kept for tests that assert the frontier stays small.
func (t OutputTree) trimmedLen() int { return bits.Len64(t.count) }
