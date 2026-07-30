package chain

import (
	"context"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

func isthmusConfig() Config {
	at := uint64(0)
	cfg := testConfig()
	cfg.HoloceneTime = &at
	cfg.IsthmusTime = &at
	return cfg
}

// isthmusAttrs are payload attributes for an Isthmus chain: Holocene is
// necessarily active too, so op-node would always supply EIP-1559 parameters.
func isthmusAttrs(parent *Block, deposits [][]byte) *engine.PayloadAttributes {
	attrs := attrsOn(parent, deposits, true)
	attrs.EIP1559Params = make([]byte, 8)
	return attrs
}

func newIsthmusChain(t *testing.T, seed string) (*Chain, *mempool.Pool) {
	t.Helper()
	m := machine.NewMock([]byte(seed))
	m.OutputFn = emitOn
	pool := mempool.New(64)
	c, err := New(context.Background(), isthmusConfig(), m, pool)
	if err != nil {
		t.Fatal(err)
	}
	return c, pool
}

// Under Isthmus the header carries the outputs Merkle root in withdrawalsRoot,
// paired with an empty requests hash. This is what makes op-node able to
// compute an output root at all for a chain with no Ethereum state trie.
func TestIsthmusPublishesOutputsRootInHeader(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")

	genesis := c.HeadBlock()
	if got, want := *genesis.Header.WithdrawalsHash, NewOutputTree().Root(); got != want {
		t.Fatalf("genesis withdrawalsRoot %s, want the empty outputs tree root %s", got, want)
	}
	if genesis.Header.RequestsHash == nil || *genesis.Header.RequestsHash != types.EmptyRequestsHash {
		t.Fatalf("genesis requestsHash %v, want the empty requests hash", genesis.Header.RequestsHash)
	}

	env := buildBlock(c, t, isthmusAttrs(genesis, [][]byte{depositTx(t, "l1-info")}))
	payload := env.ExecutionPayload
	if payload.WithdrawalsRoot == nil {
		t.Fatal("payload carries no withdrawalsRoot under Isthmus")
	}

	// One deposit, and the mock emits one provable output per input, so the
	// accumulator must hold exactly one leaf.
	tree, ok := c.OutputTreeAt(payload.BlockHash)
	if !ok {
		t.Fatal("no outputs accumulator recorded for the block")
	}
	if tree.Count() != 1 {
		t.Fatalf("accumulator holds %d outputs, want 1", tree.Count())
	}
	if *payload.WithdrawalsRoot != tree.Root() {
		t.Fatalf("header commits to %s, accumulator root is %s", *payload.WithdrawalsRoot, tree.Root())
	}
	if *payload.WithdrawalsRoot == NewOutputTree().Root() {
		t.Fatal("header still commits to the empty tree after an output was produced")
	}
}

// The commitment is cumulative: an output produced in an early block stays
// covered by the root of every later block, which is what lets a withdrawal be
// proven against any output root published after it.
func TestOutputsRootIsCumulative(t *testing.T) {
	c, _ := newIsthmusChain(t, "seed")

	var roots []common.Hash
	var counts []uint64
	for i := range 3 {
		env := buildBlock(c, t, isthmusAttrs(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i))+"-input")}))
		tree, ok := c.OutputTreeAt(env.ExecutionPayload.BlockHash)
		if !ok {
			t.Fatal("missing accumulator")
		}
		roots = append(roots, tree.Root())
		counts = append(counts, tree.Count())
	}

	for i, want := range []uint64{1, 2, 3} {
		if counts[i] != want {
			t.Fatalf("block %d: accumulator holds %d outputs, want %d", i+1, counts[i], want)
		}
	}
	for i := 1; i < len(roots); i++ {
		if roots[i] == roots[i-1] {
			t.Fatalf("block %d: root did not change after new outputs", i+1)
		}
	}
}

// A payload claiming an outputs root the machine did not produce must be
// rejected. This is what keeps withdrawals honest: the root is re-derived from
// re-execution, not trusted from the payload.
func TestImportRejectsForgedOutputsRoot(t *testing.T) {
	sequencer, _ := newIsthmusChain(t, "seed")
	env := buildBlock(sequencer, t, isthmusAttrs(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))

	forged := *env.ExecutionPayload
	bogus := common.HexToHash("0xf0f0f0")
	forged.WithdrawalsRoot = &bogus
	header, err := sequencer.Config().headerFromPayload(&forged, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	forged.BlockHash = header.Hash() // make the forgery internally consistent

	verifier, _ := newIsthmusChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), &forged, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.INVALID {
		t.Fatalf("forged outputs root accepted with status %s", st.Status)
	}
	if st.ValidationError == nil || !strings.Contains(*st.ValidationError, "outputs root mismatch") {
		t.Fatalf("unexpected validation error: %v", st.ValidationError)
	}
}

// Builder and verifier must agree on the commitment, through the two different
// code paths (payload adoption vs re-execution).
func TestOutputsRootAgreesBetweenBuilderAndVerifier(t *testing.T) {
	sequencer, _ := newIsthmusChain(t, "seed")
	env := buildBlock(sequencer, t, isthmusAttrs(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))

	verifier, _ := newIsthmusChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), env.ExecutionPayload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("verifier rejected the block: %s (%v)", st.Status, st.ValidationError)
	}
	built, _ := sequencer.OutputTreeAt(env.ExecutionPayload.BlockHash)
	replayed, ok := verifier.OutputTreeAt(env.ExecutionPayload.BlockHash)
	if !ok {
		t.Fatal("verifier recorded no accumulator")
	}
	if built.Root() != replayed.Root() || built.Count() != replayed.Count() {
		t.Fatalf("builder committed %s/%d, verifier computed %s/%d",
			built.Root(), built.Count(), replayed.Root(), replayed.Count())
	}
}

// Reports are not provable, so they must not move the outputs commitment even
// though they are recorded and served.
func TestReportsDoNotAffectOutputsRoot(t *testing.T) {
	build := func(fn func([]byte) []machine.Output) common.Hash {
		m := machine.NewMock([]byte("seed"))
		m.OutputFn = fn
		c, err := New(context.Background(), isthmusConfig(), m, mempool.New(8))
		if err != nil {
			t.Fatal(err)
		}
		env := buildBlock(c, t, isthmusAttrs(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))
		return *env.ExecutionPayload.WithdrawalsRoot
	}

	reportsOnly := build(func([]byte) []machine.Output {
		return []machine.Output{{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: []byte("chatty")}}
	})
	silent := build(func([]byte) []machine.Output { return nil })
	if reportsOnly != silent {
		t.Fatalf("reports changed the outputs root: %s vs %s", reportsOnly, silent)
	}
	if silent != NewOutputTree().Root() {
		t.Fatalf("a block with no provable outputs committed %s, want the empty tree root", silent)
	}
}

// Pre-Isthmus chains must not carry a withdrawals root, and post-Isthmus ones
// must; a payload that disagrees with the chain's fork schedule is invalid.
func TestWithdrawalsRootMustMatchForkSchedule(t *testing.T) {
	isthmus, _ := newIsthmusChain(t, "seed")
	env := buildBlock(isthmus, t, isthmusAttrs(isthmus.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))

	preIsthmus := testConfig()
	at := uint64(0)
	preIsthmus.HoloceneTime = &at
	if _, err := preIsthmus.headerFromPayload(env.ExecutionPayload, env.ParentBeaconBlockRoot); err == nil {
		t.Fatal("a pre-Isthmus chain accepted a payload carrying a withdrawalsRoot")
	}

	stripped := *env.ExecutionPayload
	stripped.WithdrawalsRoot = nil
	if _, err := isthmus.Config().headerFromPayload(&stripped, env.ParentBeaconBlockRoot); err == nil {
		t.Fatal("an Isthmus chain accepted a payload with no withdrawalsRoot")
	}
}

// Isthmus reshapes the header, so it cannot be enabled without Holocene.
func TestIsthmusRequiresHolocene(t *testing.T) {
	at := uint64(0)
	cfg := testConfig()
	cfg.IsthmusTime = &at
	if _, err := New(context.Background(), cfg, machine.NewMock([]byte("seed")), nil); err == nil {
		t.Fatal("accepted an Isthmus chain without Holocene")
	}
}
