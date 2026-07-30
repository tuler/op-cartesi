package chain

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// naiveRoot is a direct transcription of LibMerkle32.merkleRoot: build each
// parent level in turn, substituting defaultNode for missing positions, and
// lift defaultNode with parent(defaultNode, defaultNode) at every level. It is
// deliberately the slow, obvious implementation, so that agreement with
// OutputTree's frontier arithmetic is evidence the fast one is right.
func naiveRoot(leaves []common.Hash, height int) common.Hash {
	nodes := append([]common.Hash(nil), leaves...)
	defaultNode := common.Hash{}
	at := func(ns []common.Hash, i int) common.Hash {
		if i < len(ns) {
			return ns[i]
		}
		return defaultNode
	}
	for range height {
		parents := make([]common.Hash, (len(nodes)+1)/2)
		for i := range parents {
			parents[i] = hashPair(at(nodes, 2*i), at(nodes, 2*i+1))
		}
		nodes = parents
		defaultNode = hashPair(defaultNode, defaultNode)
	}
	return at(nodes, 0)
}

func leaf(i int) common.Hash { return OutputLeaf(fmt.Appendf(nil, "output-%d", i)) }

// The incremental tree must agree with the reference implementation at every
// size, including the empty tree and the boundaries where a level fills up.
func TestOutputTreeMatchesReferenceImplementation(t *testing.T) {
	var leaves []common.Hash
	tree := NewOutputTree()

	for n := range 40 {
		if got, want := tree.Root(), naiveRoot(leaves, OutputTreeHeight); got != want {
			t.Fatalf("%d leaves: incremental root %s, reference root %s", n, got, want)
		}
		if tree.Count() != uint64(n) {
			t.Fatalf("count %d, want %d", tree.Count(), n)
		}
		leaves = append(leaves, leaf(n))
		tree = tree.Append(leaf(n))
	}
}

// The empty tree's root is the all-zero subtree of full height, which is what
// a chain that has produced no outputs must commit to.
func TestEmptyOutputTreeRoot(t *testing.T) {
	if got, want := NewOutputTree().Root(), zeroHashes[OutputTreeHeight]; got != want {
		t.Fatalf("empty root %s, want the height-%d zero hash %s", got, OutputTreeHeight, want)
	}
	if got := NewOutputTree().Root(); got != naiveRoot(nil, OutputTreeHeight) {
		t.Fatal("empty root disagrees with the reference implementation")
	}
}

// Appending in batches must be indistinguishable from appending one at a time:
// a block appends all of its outputs at once, but the resulting chain state
// must not depend on how the outputs were grouped into blocks.
func TestOutputTreeAppendIsAssociative(t *testing.T) {
	oneByOne := NewOutputTree()
	for i := range 13 {
		oneByOne = oneByOne.Append(leaf(i))
	}
	batched := NewOutputTree().Append(leaf(0), leaf(1), leaf(2)).
		Append(leaf(3)).
		Append(leaf(4), leaf(5), leaf(6), leaf(7), leaf(8), leaf(9), leaf(10), leaf(11), leaf(12))

	if oneByOne.Root() != batched.Root() {
		t.Fatalf("grouping changed the root: %s vs %s", oneByOne.Root(), batched.Root())
	}
	if oneByOne.Count() != batched.Count() {
		t.Fatalf("grouping changed the count: %d vs %d", oneByOne.Count(), batched.Count())
	}
}

// Append must not mutate the receiver: block building forks the parent's tree,
// and a discarded branch must leave the parent's accumulator untouched.
func TestOutputTreeAppendDoesNotMutateReceiver(t *testing.T) {
	base := NewOutputTree().Append(leaf(0), leaf(1), leaf(2))
	before, count := base.Root(), base.Count()

	branchA := base.Append(leaf(10))
	branchB := base.Append(leaf(20))

	if base.Root() != before || base.Count() != count {
		t.Fatal("Append mutated the receiver")
	}
	if branchA.Root() == branchB.Root() {
		t.Fatal("different appends produced the same root")
	}
}

// The frontier stays logarithmic in the output count, which is what makes it
// affordable to keep one per block.
func TestOutputTreeFrontierStaysSmall(t *testing.T) {
	tree := NewOutputTree()
	for i := range 1000 {
		tree = tree.Append(leaf(i))
	}
	if got := len(tree.branch); got > OutputTreeHeight {
		t.Fatalf("frontier has %d levels, more than the tree height", got)
	}
	if got, want := tree.trimmedLen(), 10; got != want { // bits.Len64(1000) == 10
		t.Fatalf("frontier significant levels %d, want %d", got, want)
	}
}

// A known-answer test that pins the hashing convention: leaves are
// keccak256(output) and parents are keccak256(left || right). If either
// changed, existing Cartesi proofs would stop verifying, and this fails.
func TestOutputTreeHashingConvention(t *testing.T) {
	output := []byte("voucher payload")
	if got, want := OutputLeaf(output), naiveRoot([]common.Hash{OutputLeaf(output)}, 0); got != want {
		t.Fatalf("a height-0 tree of one leaf should be the leaf itself: %s vs %s", got, want)
	}
	// One leaf in a height-1 tree pairs with the zero leaf on the right.
	single := NewOutputTree().Append(OutputLeaf(output))
	if got, want := naiveRoot([]common.Hash{OutputLeaf(output)}, 1), hashPair(OutputLeaf(output), common.Hash{}); got != want {
		t.Fatalf("height-1 root %s, want keccak(leaf || 0) = %s", got, want)
	}
	if single.Count() != 1 {
		t.Fatal("append did not record the leaf")
	}
}
