package chain

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// emitOn builds an OutputFn that emits one voucher/notice and one report for
// every input, derived from the input bytes so the mock stays deterministic.
func emitOn(input []byte) []machine.Output {
	return []machine.Output{
		{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: append([]byte("output:"), input[:4]...)},
		{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: append([]byte("report:"), input[:4]...)},
	}
}

func newEmittingChain(t *testing.T, seed string, reject func([]byte) bool) (*Chain, *mempool.Pool) {
	t.Helper()
	m := machine.NewMock([]byte(seed))
	m.OutputFn = emitOn
	m.RejectFn = reject
	pool := mempool.New(64)
	c, err := New(context.Background(), testConfig(), m, pool)
	if err != nil {
		t.Fatal(err)
	}
	return c, pool
}

// Outputs must be recorded identically by the node that built the block and by
// a node that only re-executes it. Everything downstream — the outputs
// commitment, and therefore withdrawals — depends on this agreement.
func TestOutputsAgreeBetweenBuilderAndVerifier(t *testing.T) {
	sequencer, pool := newEmittingChain(t, "seed", nil)
	if _, err := pool.Add(userTx(t, 0, []byte("user input"))); err != nil {
		t.Fatal(err)
	}
	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, false))
	payload := env.ExecutionPayload

	built := sequencer.BlockOutputs(payload.BlockHash)
	if len(built) != 2 {
		t.Fatalf("recorded outputs for %d transactions, want 2", len(built))
	}

	// An independent node running the same machine, which must reach the same
	// outputs by re-executing the payload rather than by having built it.
	verifier, _ := newEmittingChain(t, "seed", nil)
	st, err := verifier.ImportPayload(context.Background(), payload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("verifier rejected the block: %s", st.Status)
	}
	replayed := verifier.BlockOutputs(payload.BlockHash)

	if len(replayed) != len(built) {
		t.Fatalf("verifier recorded %d transactions' outputs, builder recorded %d", len(replayed), len(built))
	}
	for i := range built {
		b, r := built[i], replayed[i]
		if b.TxIndex != r.TxIndex || b.TxHash != r.TxHash || b.Accepted != r.Accepted {
			t.Errorf("tx %d: metadata differs: builder %+v, verifier %+v", i, b, r)
		}
		if len(b.Outputs) != len(r.Outputs) {
			t.Fatalf("tx %d: builder recorded %d outputs, verifier %d", i, len(b.Outputs), len(r.Outputs))
		}
		for j := range b.Outputs {
			if !bytes.Equal(b.Outputs[j], r.Outputs[j]) {
				t.Errorf("tx %d output %d: builder %x, verifier %x", i, j, b.Outputs[j], r.Outputs[j])
			}
		}
	}
}

// A rejected input is rolled back, so its provable outputs must not be
// recorded. Its report must survive: it is usually the only explanation of the
// failure.
func TestRejectedInputDropsOutputsButKeepsReports(t *testing.T) {
	reject := func(input []byte) bool { return bytes.Contains(input, []byte("l1-info")) }
	c, _ := newEmittingChain(t, "seed", reject)

	// A deposit is included in the block even when rejected, which makes it
	// the case that distinguishes the two rules.
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))
	recorded := c.BlockOutputs(env.ExecutionPayload.BlockHash)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d transactions, want 1", len(recorded))
	}
	tx := recorded[0]
	if tx.Accepted {
		t.Fatal("expected the deposit to be rejected by the mock")
	}
	if len(tx.Outputs) != 0 {
		t.Errorf("rejected input contributed %d provable outputs, want 0", len(tx.Outputs))
	}
	if len(tx.Reports) != 1 {
		t.Errorf("rejected input contributed %d reports, want 1", len(tx.Reports))
	}
}

// Reports are not provable and must stay out of every commitment. Until the
// outputs commitment lands, the observable form of that rule is that emitting
// reports never changes the block hash or the state root.
func TestReportsDoNotAffectTheBlockCommitment(t *testing.T) {
	withReports := machine.NewMock([]byte("seed"))
	withReports.OutputFn = func(input []byte) []machine.Output {
		return []machine.Output{{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: []byte("noisy diagnostics")}}
	}
	silent := machine.NewMock([]byte("seed"))

	build := func(m *machine.Mock) *engine.ExecutableData {
		c, err := New(context.Background(), testConfig(), m, mempool.New(8))
		if err != nil {
			t.Fatal(err)
		}
		return buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true)).ExecutionPayload
	}

	noisy, quiet := build(withReports), build(silent)
	if noisy.StateRoot != quiet.StateRoot {
		t.Errorf("reports changed the state root: %s vs %s", noisy.StateRoot, quiet.StateRoot)
	}
	if noisy.BlockHash != quiet.BlockHash {
		t.Errorf("reports changed the block hash: %s vs %s", noisy.BlockHash, quiet.BlockHash)
	}
}

// Outputs are attributed to the transaction that produced them, by index and
// by hash, so a receipt can later be assembled per transaction.
func TestOutputsAreAttributedToTransactions(t *testing.T) {
	c, pool := newEmittingChain(t, "seed", nil)
	raw := userTx(t, 0, []byte("user input"))
	if _, err := pool.Add(raw); err != nil {
		t.Fatal(err)
	}
	deposit := depositTx(t, "l1-info")
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{deposit}, false))

	recorded := c.BlockOutputs(env.ExecutionPayload.BlockHash)
	if len(recorded) != 2 {
		t.Fatalf("recorded %d transactions, want 2", len(recorded))
	}
	depositHash, err := txHash(deposit)
	if err != nil {
		t.Fatal(err)
	}
	userHash, err := txHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	if recorded[0].TxIndex != 0 || recorded[0].TxHash != depositHash {
		t.Errorf("first record is %d/%s, want 0/%s", recorded[0].TxIndex, recorded[0].TxHash, depositHash)
	}
	if recorded[1].TxIndex != 1 || recorded[1].TxHash != userHash {
		t.Errorf("second record is %d/%s, want 1/%s", recorded[1].TxIndex, recorded[1].TxHash, userHash)
	}
	for i, r := range recorded {
		if len(r.Outputs) != 1 || len(r.Reports) != 1 {
			t.Errorf("tx %d: %d outputs / %d reports, want 1/1", i, len(r.Outputs), len(r.Reports))
		}
		if r.Cycles == 0 {
			t.Errorf("tx %d: no cycles recorded", i)
		}
	}
}

func TestBlockOutputsUnknownBlock(t *testing.T) {
	c, _ := newEmittingChain(t, "seed", nil)
	if got := c.BlockOutputs(common.HexToHash("0xdeadbeef")); got != nil {
		t.Fatalf("unknown block returned %d records, want nil", len(got))
	}
}
