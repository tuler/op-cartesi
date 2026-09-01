package chain

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// A receipt reports what the machine did: outputs become logs, acceptance
// becomes status, cycles become gas.
func TestReceiptReflectsMachineExecution(t *testing.T) {
	c, pool := newIsthmusChain(t, "seed")
	raw := userTx(t, 0, []byte("user input"))
	if _, err := pool.Add(raw); err != nil {
		t.Fatal(err)
	}
	deposit := depositTx(t, "l1-info")
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{deposit}, false))
	payload := env.ExecutionPayload

	userHash, err := txHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	receipt := c.ReceiptByTxHash(userHash)
	if receipt == nil {
		t.Fatal("no receipt for the sequenced transaction")
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		t.Errorf("status %d, want success", receipt.Status)
	}
	if receipt.BlockHash != payload.BlockHash || receipt.BlockNumber != payload.Number {
		t.Errorf("receipt points at %s/%d, want %s/%d",
			receipt.BlockHash, receipt.BlockNumber, payload.BlockHash, payload.Number)
	}
	if receipt.GasUsed == 0 {
		t.Error("no gas recorded despite cycles being consumed")
	}
	// The mock emits one provable output per input, so exactly one log.
	if len(receipt.Logs) != 1 {
		t.Fatalf("%d logs, want 1", len(receipt.Logs))
	}
	log := receipt.Logs[0]
	if log.Address != OutputsEmitterAddress {
		t.Errorf("log address %s, want %s", log.Address, OutputsEmitterAddress)
	}
	if len(log.Topics) != 2 || log.Topics[0] != OutputEventTopic {
		t.Fatalf("unexpected topics %v", log.Topics)
	}
	if receipt.Bloom == (types.Bloom{}) {
		t.Error("bloom is empty despite the receipt having a log")
	}

	// Cumulative gas must accumulate across the block, not restart per tx.
	receipts := c.BlockReceipts(payload.BlockHash)
	if len(receipts) != 2 {
		t.Fatalf("%d receipts, want 2", len(receipts))
	}
	if receipts[1].CumulativeGasUsed <= receipts[0].CumulativeGasUsed {
		t.Errorf("cumulative gas did not accumulate: %d then %d",
			receipts[0].CumulativeGasUsed, receipts[1].CumulativeGasUsed)
	}
	if want := receipts[0].GasUsed + receipts[1].GasUsed; receipts[1].CumulativeGasUsed != want {
		t.Errorf("cumulative gas %d, want %d", receipts[1].CumulativeGasUsed, want)
	}
}

// A log's index topic must be the output's position in the chain-wide tree,
// since that is exactly the outputIndex an on-chain output proof takes.
func TestReceiptLogsCarryChainWideOutputIndex(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")

	var seen []uint64
	for i := range 3 {
		env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i))+"-input")}, true))
		receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
		if len(receipts) != 1 || len(receipts[0].Logs) != 1 {
			t.Fatalf("block %d: unexpected receipts/logs", i)
		}
		seen = append(seen, receipts[0].Logs[0].Topics[1].Big().Uint64())
	}

	for i, index := range seen {
		if index != uint64(i) {
			t.Errorf("block %d: output index %d, want %d", i, index, i)
		}
	}
}

// A rejected input produced no provable outputs, so its receipt must show
// failure and carry no logs — the reports stay available separately.
func TestReceiptForRejectedInput(t *testing.T) {
	reject := func(input []byte) bool { return bytes.Contains(input, []byte("l1-info")) }
	c, _ := newEmittingChain(t, "seed", reject)

	deposit := depositTx(t, "l1-info")
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{deposit}, true))
	receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
	if len(receipts) != 1 {
		t.Fatalf("%d receipts, want 1", len(receipts))
	}
	if receipts[0].Status != types.ReceiptStatusFailed {
		t.Error("rejected input reported as successful")
	}
	if len(receipts[0].Logs) != 0 {
		t.Errorf("rejected input produced %d logs, want 0", len(receipts[0].Logs))
	}

	depositHash, err := txHash(deposit)
	if err != nil {
		t.Fatal(err)
	}
	emissions, ok := c.TxEmissions(depositHash)
	if !ok {
		t.Fatal("no emissions recorded for the rejected input")
	}
	if len(emissions.Outputs.Reports) != 1 {
		t.Errorf("%d reports, want the rejection explained", len(emissions.Outputs.Reports))
	}
}

// Receipts follow the canonical chain: a transaction reorged out has no
// receipt, and one reorged in does.
func TestReceiptsFollowReorgs(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	genesis := c.HeadBlock()

	depositA := depositTx(t, "branch-a")
	envA := buildBlock(c, t, attrsOn(genesis, [][]byte{depositA}, true))
	hashA, err := txHash(depositA)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReceiptByTxHash(hashA) == nil {
		t.Fatal("no receipt on the canonical branch")
	}

	// Build a competing block at the same height and make it canonical.
	fcu(c, t, genesis.Hash(), nil)
	depositB := depositTx(t, "branch-b")
	attrsB := attrsOn(genesis, [][]byte{depositB}, true)
	attrsB.Timestamp = genesis.Time() + 4
	envB := buildBlock(c, t, attrsB)
	if envA.ExecutionPayload.BlockHash == envB.ExecutionPayload.BlockHash {
		t.Fatal("branches did not diverge")
	}

	if c.ReceiptByTxHash(hashA) != nil {
		t.Error("reorged-out transaction still has a receipt")
	}
	hashB, err := txHash(depositB)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReceiptByTxHash(hashB) == nil {
		t.Error("transaction on the new canonical branch has no receipt")
	}

	// Reorg back; the original transaction's receipt must return.
	fcu(c, t, envA.ExecutionPayload.BlockHash, nil)
	if c.ReceiptByTxHash(hashA) == nil {
		t.Error("receipt did not return after reorging back")
	}
	if c.ReceiptByTxHash(hashB) != nil {
		t.Error("receipt survived on the now-orphaned branch")
	}
}

func TestReceiptUnknownTransaction(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	if r := c.ReceiptByTxHash(common.HexToHash("0xdeadbeef")); r != nil {
		t.Fatal("unknown transaction returned a receipt")
	}
	if _, ok := c.TxEmissions(common.HexToHash("0xdeadbeef")); ok {
		t.Fatal("unknown transaction returned emissions")
	}
}

// Receipts are deliberately not committed to, so that the encoding can still
// change without a fork. The header must keep an empty receipts root and bloom
// even when the block's receipts carry logs.
func TestReceiptsAreNotCommitted(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))

	receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
	if len(receipts) == 0 || len(receipts[0].Logs) == 0 {
		t.Fatal("expected a receipt with logs, so the check is meaningful")
	}
	header := c.BlockByHash(env.ExecutionPayload.BlockHash).Header
	if header.ReceiptHash != types.EmptyReceiptsHash {
		t.Errorf("receipts root %s, want the empty root while the encoding is unstable", header.ReceiptHash)
	}
	if header.Bloom != (types.Bloom{}) {
		t.Error("header bloom is non-empty, which would commit to the log encoding")
	}
}

// Inspect must not disturb the chain: it runs on a fork that is discarded.
func TestInspectLeavesChainUntouched(t *testing.T) {
	ctx := context.Background()
	c, _ := newIsthmusChain(t, "seed")
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))

	head := c.HeadBlock()
	stateRootBefore := head.Header.Root
	treeBefore, _ := c.OutputTreeAt(head.Hash())

	res, err := c.Inspect(ctx, head.Hash(), []byte("are you there?"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted {
		t.Fatal("inspect was rejected")
	}
	if len(res.Reports) != 1 || !bytes.Equal(res.Reports[0], []byte("are you there?")) {
		t.Fatalf("unexpected reports %q", res.Reports)
	}

	if c.HeadBlock().Hash() != head.Hash() {
		t.Error("inspect moved the head")
	}
	if c.HeadBlock().Header.Root != stateRootBefore {
		t.Error("inspect changed the state root")
	}
	treeAfter, _ := c.OutputTreeAt(head.Hash())
	if treeAfter.Root() != treeBefore.Root() || treeAfter.Count() != treeBefore.Count() {
		t.Error("inspect changed the outputs commitment")
	}
	// And the next block must build exactly as it would have.
	next := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "next")}, true))
	if next.ExecutionPayload.ParentHash != env.ExecutionPayload.BlockHash {
		t.Error("chain did not continue from the inspected head")
	}
}

func TestInspectUnknownBlock(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")
	if _, err := c.Inspect(context.Background(), common.HexToHash("0xdeadbeef"), []byte("q")); err == nil {
		t.Fatal("inspect against an unknown block succeeded")
	}
}
