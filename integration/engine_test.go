package integration

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// buildAttributes constructs the payload attributes op-node would send for the
// block after parent, including the L1-attributes deposit that op-node always
// forces in as the first transaction.
func (h *harness) buildAttributes(parent eth.L2BlockRef, extraTxs ...[]byte) *eth.PayloadAttributes {
	h.t.Helper()
	gasLimit := eth.Uint64Quantity(30_000_000)
	beaconRoot := common.HexToHash("0xbeac0")

	txs := []eth.Data{depositTx(h.t, parent.Number+1)}
	for _, tx := range extraTxs {
		txs = append(txs, tx)
	}
	attrs := &eth.PayloadAttributes{
		Timestamp:             eth.Uint64Quantity(parent.Time + blockTime),
		PrevRandao:            eth.Bytes32(common.HexToHash("0x0badc0de")),
		SuggestedFeeRecipient: common.HexToAddress("0x4200000000000000000000000000000000000011"),
		Withdrawals:           &types.Withdrawals{},
		ParentBeaconBlockRoot: &beaconRoot,
		Transactions:          txs,
		NoTxPool:              false,
		GasLimit:              &gasLimit,
	}
	// Holocene is always active, so op-node always supplies these.
	params := eth.Bytes8{}
	attrs.EIP1559Params = &params
	return attrs
}

// sequenceBlock drives one full block through op-node's engine client, exactly
// as op-node's sequencer does: start building, fetch the payload, submit it
// back for execution, then advance the forkchoice.
func (h *harness) sequenceBlock(ctx context.Context, parent eth.L2BlockRef, extraTxs ...[]byte) *eth.ExecutionPayloadEnvelope {
	h.t.Helper()

	fc := eth.ForkchoiceState{
		HeadBlockHash:      parent.Hash,
		SafeBlockHash:      h.base.Hash,
		FinalizedBlockHash: h.base.Hash,
	}
	attrs := h.buildAttributes(parent, extraTxs...)
	result, err := h.engine.ForkchoiceUpdate(ctx, &fc, attrs)
	if err != nil {
		h.t.Fatalf("forkchoiceUpdate: %v", err)
	}
	if result.PayloadStatus.Status != eth.ExecutionValid {
		h.t.Fatalf("forkchoiceUpdate status %s: %s", result.PayloadStatus.Status, errText(result.PayloadStatus.ValidationError))
	}
	if result.PayloadID == nil {
		h.t.Fatal("forkchoiceUpdate with attributes returned no payload id")
	}

	envelope, err := h.engine.GetPayload(ctx, eth.PayloadInfo{
		ID:        *result.PayloadID,
		Timestamp: uint64(attrs.Timestamp),
	})
	if err != nil {
		h.t.Fatalf("getPayload: %v", err)
	}

	// The load-bearing assertion: op-node recomputes the block hash from the
	// payload fields using its own header assembly. Any divergence in how the
	// shim fills uncles, withdrawals, blob gas, extraData or the beacon root
	// shows up here as a mismatch.
	actual, ok := envelope.CheckBlockHash()
	if !ok {
		h.t.Fatalf("op-node recomputed block hash %s, payload claims %s", actual, envelope.ExecutionPayload.BlockHash)
	}

	status, err := h.engine.NewPayload(ctx, envelope.ExecutionPayload, envelope.ParentBeaconBlockRoot)
	if err != nil {
		h.t.Fatalf("newPayload: %v", err)
	}
	if status.Status != eth.ExecutionValid {
		h.t.Fatalf("newPayload status %s: %s", status.Status, errText(status.ValidationError))
	}

	fc.HeadBlockHash = envelope.ExecutionPayload.BlockHash
	result, err = h.engine.ForkchoiceUpdate(ctx, &fc, nil)
	if err != nil {
		h.t.Fatalf("forkchoiceUpdate (advance): %v", err)
	}
	if result.PayloadStatus.Status != eth.ExecutionValid {
		h.t.Fatalf("forkchoiceUpdate (advance) status %s", result.PayloadStatus.Status)
	}
	return envelope
}

func errText(s *string) string {
	if s == nil {
		return "<no validation error>"
	}
	return *s
}

func payloadRef(env *eth.ExecutionPayloadEnvelope) eth.L2BlockRef {
	p := env.ExecutionPayload
	return eth.L2BlockRef{
		Hash:       p.BlockHash,
		Number:     uint64(p.BlockNumber),
		ParentHash: p.ParentHash,
		Time:       uint64(p.Timestamp),
	}
}

func depositTx(t *testing.T, seq uint64) []byte {
	t.Helper()
	to := common.HexToAddress("0x4200000000000000000000000000000000000015")
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte{byte(seq)}),
		From:       common.HexToAddress("0xdeaddeaddeaddeaddeaddeaddeaddeaddead0001"),
		To:         &to,
		Gas:        1_000_000,
		Value:      new(big.Int),
		Mint:       new(big.Int),
		Data:       []byte("l1 attributes"),
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSequenceBlocksWithOpNodeClient is the core compatibility test: several
// blocks produced end-to-end through op-node's engine client, each one's hash
// independently verified by op-node's own header reconstruction.
func TestSequenceBlocksWithOpNodeClient(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	parent := h.base
	for i := range 3 {
		env := h.sequenceBlock(ctx, parent)
		payload := env.ExecutionPayload

		if uint64(payload.BlockNumber) != parent.Number+1 {
			t.Fatalf("block %d: number %d, want %d", i, payload.BlockNumber, parent.Number+1)
		}
		if payload.ParentHash != parent.Hash {
			t.Fatalf("block %d: parent %s, want %s", i, payload.ParentHash, parent.Hash)
		}
		if uint64(payload.Timestamp) != parent.Time+blockTime {
			t.Fatalf("block %d: timestamp %d, want %d", i, payload.Timestamp, parent.Time+blockTime)
		}
		if len(payload.Transactions) != 1 {
			t.Fatalf("block %d: %d transactions, want the forced deposit only", i, len(payload.Transactions))
		}
		if payload.StateRoot == (eth.Bytes32{}) {
			t.Fatalf("block %d: empty state root", i)
		}
		// The EIP-1559 parameters are recorded in extraData.
		if len(payload.ExtraData) != 9 {
			t.Fatalf("block %d: extraData %x, want 9 bytes", i, payload.ExtraData)
		}
		parent = payloadRef(env)
	}

	if head := h.head(ctx); head != 3 {
		t.Fatalf("head at %d, want 3", head)
	}
}

// The machine's state root must advance as inputs are consumed, and identical
// input sequences must produce identical roots. This is the property the whole
// design rests on, checked here through the real client path.
func TestStateRootAdvancesDeterministically(t *testing.T) {
	ctx := context.Background()

	run := func() []eth.Bytes32 {
		h := newExclusiveHarness(t)
		parent := h.base
		var roots []eth.Bytes32
		for range 3 {
			env := h.sequenceBlock(ctx, parent)
			roots = append(roots, env.ExecutionPayload.StateRoot)
			parent = payloadRef(env)
		}
		return roots
	}

	first, second := run(), run()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("block %d: state root %s on first run, %s on second", i, first[i], second[i])
		}
		if i > 0 && first[i] == first[i-1] {
			t.Fatalf("block %d: state root did not advance after consuming inputs", i)
		}
	}
}

// A transaction submitted through eth_sendRawTransaction must be picked up by
// the next block, and dropped from the pool once included.
func TestMempoolTransactionIsSequenced(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("integration user key")))
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x1234")
	// The chain's own id, read from the engine, so a remote one with a
	// different id still gets a transaction it will admit.
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(new(big.Int).SetUint64(h.chainID)), &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(h.chainID),
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       1_000_000,
		To:        &to,
		Data:      []byte("hello from the mempool"),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.node.SendRawTransaction(ctx, raw); err != nil {
		t.Fatal(err)
	}
	// Before the block, the node reports it with null block coordinates:
	// known, not mined (ENGINE-RPC-SPEC §5.2).
	pending, err := h.node.TransactionByHash(ctx, tx.Hash())
	if err != nil || pending == nil {
		t.Fatalf("the node does not know the transaction it accepted: %v", err)
	}
	if pending.BlockHash != nil || pending.BlockNumber != nil {
		t.Fatalf("a queued transaction reports block coordinates %v/%v", pending.BlockHash, pending.BlockNumber)
	}

	env := h.sequenceBlock(ctx, h.base)
	txs := env.ExecutionPayload.Transactions
	if len(txs) != 2 {
		t.Fatalf("expected deposit + pool transaction, got %d", len(txs))
	}
	// Deposits must come first, exactly as the OP Stack requires.
	var first types.Transaction
	if err := first.UnmarshalBinary(txs[0]); err != nil {
		t.Fatal(err)
	}
	if first.Type() != types.DepositTxType {
		t.Fatalf("first transaction is type %d, want deposit", first.Type())
	}
	var second types.Transaction
	if err := second.UnmarshalBinary(txs[1]); err != nil {
		t.Fatal(err)
	}
	if second.Hash() != tx.Hash() {
		t.Fatalf("sequenced tx %s, want %s", second.Hash(), tx.Hash())
	}
	// And after it, with the block it landed in — which is how a client sees
	// the queue drain, and all a client can see.
	mined, err := h.node.TransactionByHash(ctx, tx.Hash())
	if err != nil || mined == nil {
		t.Fatalf("the sequenced transaction is unknown to the node: %v", err)
	}
	if mined.BlockHash == nil || *mined.BlockHash != common.Hash(env.ExecutionPayload.BlockHash) {
		t.Fatalf("transaction reports block %v, want %s", mined.BlockHash, env.ExecutionPayload.BlockHash)
	}
}

// op-node reacts to an unknown head by treating the engine as syncing rather
// than erroring, so the shim must answer SYNCING (not an RPC error).
func TestUnknownHeadReportsSyncing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	result, err := h.engine.ForkchoiceUpdate(ctx, &eth.ForkchoiceState{
		HeadBlockHash:      common.HexToHash("0xdeadbeef"),
		SafeBlockHash:      h.base.Hash,
		FinalizedBlockHash: h.base.Hash,
	}, nil)
	if err != nil {
		t.Fatalf("forkchoiceUpdate returned a transport error: %v", err)
	}
	if result.PayloadStatus.Status != eth.ExecutionSyncing {
		t.Fatalf("status %s, want SYNCING", result.PayloadStatus.Status)
	}
}

// A verifier node holds no pending payload, so it must reach the same block by
// re-executing the payload's transactions in its own machine. This is the
// derivation-from-L1 path, exercised through op-node's client.
func TestVerifierRebuildsBlockFromPayload(t *testing.T) {
	ctx := context.Background()
	sequencer := newHarness(t)
	verifier := newExclusiveHarness(t)

	if sequencer.base.Hash != verifier.base.Hash {
		t.Fatal("independent nodes disagree on genesis")
	}

	env := sequencer.sequenceBlock(ctx, sequencer.base)
	payload := env.ExecutionPayload

	status, err := verifier.engine.NewPayload(ctx, payload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatalf("verifier newPayload: %v", err)
	}
	if status.Status != eth.ExecutionValid {
		t.Fatalf("verifier rejected the sequencer's block: %s (%s)", status.Status, errText(status.ValidationError))
	}
	if status.LatestValidHash == nil || *status.LatestValidHash != payload.BlockHash {
		t.Fatalf("latestValidHash %v, want %s", status.LatestValidHash, payload.BlockHash)
	}

	result, err := verifier.engine.ForkchoiceUpdate(ctx, &eth.ForkchoiceState{
		HeadBlockHash:      payload.BlockHash,
		SafeBlockHash:      payload.BlockHash,
		FinalizedBlockHash: verifier.base.Hash,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.PayloadStatus.Status != eth.ExecutionValid {
		t.Fatalf("verifier forkchoice status %s", result.PayloadStatus.Status)
	}
	if got := verifier.headHash(ctx); got != common.Hash(payload.BlockHash) {
		t.Fatalf("verifier head %s, want %s", got, payload.BlockHash)
	}
}
