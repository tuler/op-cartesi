package integration

import (
	"context"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
)

// TestIsthmusOutputRootThroughOpNode is the point of Isthmus support: op-node
// must be able to build an L2 output root for this chain.
//
// Pre-Isthmus it cannot. op-node's L2Client.outputV0 obtains the
// messagePasserStorageRoot by calling eth_getProof for the L2ToL1MessagePasser
// account and verifying the result against the block's state root — which is
// impossible here, because the state root is a Cartesi hash tree and there is
// no Ethereum MPT to prove against. From Isthmus it reads the value straight
// out of the header instead, so this chain can publish its outputs Merkle root
// there and op-proposer works unmodified.
//
// This test reproduces that computation with op-node's own OutputV0 type and
// hashing, over payloads the shim produced.
func TestIsthmusOutputRootThroughOpNode(t *testing.T) {
	ctx := context.Background()
	h := newEmittingHarness(t)

	parent := h.genesisRef
	var previous eth.Bytes32
	for i := range 3 {
		env := h.sequenceBlock(ctx, parent)
		payload := env.ExecutionPayload

		if payload.WithdrawalsRoot == nil {
			t.Fatalf("block %d: payload has no withdrawalsRoot; op-node could not compute an output root", i)
		}

		// Exactly what op-node does from Isthmus onward.
		output := &eth.OutputV0{
			StateRoot:                payload.StateRoot,
			MessagePasserStorageRoot: eth.Bytes32(*payload.WithdrawalsRoot),
			BlockHash:                payload.BlockHash,
		}
		root := eth.OutputRoot(output)
		if root == (eth.Bytes32{}) {
			t.Fatalf("block %d: empty output root", i)
		}
		if eth.Bytes32(root) == previous {
			t.Fatalf("block %d: output root did not change as outputs accumulated", i)
		}
		previous = eth.Bytes32(root)

		// The commitment must be the chain's own accumulator root, and it must
		// grow by one output per block for this machine.
		tree, ok := h.chain.OutputTreeAt(payload.BlockHash)
		if !ok {
			t.Fatalf("block %d: no outputs accumulator", i)
		}
		if common.Hash(*payload.WithdrawalsRoot) != tree.Root() {
			t.Fatalf("block %d: header commits %s, accumulator root is %s", i, *payload.WithdrawalsRoot, tree.Root())
		}
		if want := uint64(i + 1); tree.Count() != want {
			t.Fatalf("block %d: accumulator holds %d outputs, want %d", i, tree.Count(), want)
		}
		parent = payloadRef(env)
	}
}

// Under Isthmus, op-node reconstructs the header from the withdrawalsRoot
// field and pairs it with an empty requests hash. sequenceBlock already runs
// CheckBlockHash on every block, so reaching the end proves the shim's header
// matches; this test pins the Isthmus-specific fields explicitly.
func TestIsthmusHeaderShape(t *testing.T) {
	ctx := context.Background()
	h := newEmittingHarness(t)

	env := h.sequenceBlock(ctx, h.genesisRef)
	payload := env.ExecutionPayload

	if payload.Withdrawals == nil || len(*payload.Withdrawals) != 0 {
		t.Fatalf("withdrawals list is %v, want present and empty under Isthmus", payload.Withdrawals)
	}
	if payload.WithdrawalsRoot == nil {
		t.Fatal("withdrawalsRoot missing under Isthmus")
	}
	if actual, ok := env.CheckBlockHash(); !ok {
		t.Fatalf("op-node recomputed %s, payload claims %s", actual, payload.BlockHash)
	}
}
