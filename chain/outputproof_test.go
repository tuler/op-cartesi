package chain

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A proof must reproduce exactly the root the accumulator committed to. The
// two are computed by different code — the accumulator carries only a frontier
// and never sees an old leaf again, while the proof is built from the full
// leaf list — so agreement is a real check on both.
func TestOutputProofReproducesTheAccumulatorRoot(t *testing.T) {
	for _, count := range []int{1, 2, 3, 5, 8, 17, 64, 100} {
		t.Run(fmt.Sprintf("%d-outputs", count), func(t *testing.T) {
			var leaves []common.Hash
			tree := NewOutputTree()
			for i := range count {
				leaf := OutputLeaf(fmt.Appendf(nil, "output-%d", i))
				leaves = append(leaves, leaf)
				tree = tree.Append(leaf)
			}
			root := tree.Root()
			for i := range count {
				proof, err := ProveOutput(leaves, uint64(i))
				if err != nil {
					t.Fatal(err)
				}
				if len(proof.Siblings) != OutputTreeHeight {
					t.Fatalf("proof has %d siblings, want %d", len(proof.Siblings), OutputTreeHeight)
				}
				if proof.OutputIndex != uint64(i) {
					t.Fatalf("proof indexes output %d, want %d", proof.OutputIndex, i)
				}
				if got := RootAfterReplacement(proof.Siblings, uint64(i), leaves[i]); got != root {
					t.Fatalf("proof for output %d of %d reproduces %s, accumulator says %s", i, count, got, root)
				}
			}
		})
	}
}

// A proof only proves the leaf it was built for.
func TestOutputProofRejectsTheWrongLeaf(t *testing.T) {
	var leaves []common.Hash
	tree := NewOutputTree()
	for i := range 6 {
		leaf := OutputLeaf(fmt.Appendf(nil, "output-%d", i))
		leaves = append(leaves, leaf)
		tree = tree.Append(leaf)
	}
	proof, err := ProveOutput(leaves, 2)
	if err != nil {
		t.Fatal(err)
	}
	forged := OutputLeaf([]byte("an output the machine never emitted"))
	if got := RootAfterReplacement(proof.Siblings, 2, forged); got == tree.Root() {
		t.Fatal("a forged leaf reproduced the committed root")
	}
}

func TestProveOutputOutOfRange(t *testing.T) {
	leaves := []common.Hash{OutputLeaf([]byte("only one"))}
	if _, err := ProveOutput(leaves, 1); err == nil {
		t.Fatal("proved an output that does not exist")
	}
}

// TestOutputTreeMatchesSolidity pins this package's tree to Cartesi's on-chain
// verifier.
//
// The two implementations share no code: the tree is built here in Go and
// checked on L1 by LibOutputValidityProof over LibBinaryMerkleTree, and a
// disagreement would not show up as a failing test on either side — it would
// show up as a withdrawal that cannot be executed. So both sides pin the same
// number over the same outputs. The Solidity half is
// contracts/test/OutputTree.t.sol; change either builder and one of the two
// fails.
func TestOutputTreeMatchesSolidity(t *testing.T) {
	outputs := [][]byte{
		[]byte("output zero"),
		[]byte("output one"),
		[]byte("output two"),
		[]byte("output three"),
		[]byte("output four"),
	}
	const want = "0x49afe364b0c4fe906304bbfc9d6273a94df905335403d8e65eeda219ea7fb717"

	var leaves []common.Hash
	tree := NewOutputTree()
	for _, output := range outputs {
		leaf := OutputLeaf(output)
		leaves = append(leaves, leaf)
		tree = tree.Append(leaf)
	}
	if got := tree.Root().Hex(); got != want {
		t.Fatalf("outputs root is %s, but the Solidity side pins %s", got, want)
	}
	// Every proof against that root has to reproduce it, which is what the
	// contract checks before executing an output.
	for i := range leaves {
		proof, err := ProveOutput(leaves, uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		if got := RootAfterReplacement(proof.Siblings, uint64(i), leaves[i]); got != tree.Root() {
			t.Fatalf("proof for output %d reproduces %s, not %s", i, got, tree.Root())
		}
	}
}
