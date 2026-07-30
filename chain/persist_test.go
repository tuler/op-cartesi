package chain

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// persistConfig checkpoints often enough that a short test crosses the
// trigger, which is the part worth exercising.
func persistConfig() Config {
	cfg := testConfig()
	cfg.CheckpointInterval = 2
	cfg.CheckpointRetention = 3
	return cfg
}

func newPersistentChain(t *testing.T, dir, seed string) (*Chain, *Store) {
	t.Helper()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), persistConfig(), machine.NewMock([]byte(seed)), mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	c.store = store
	// Genesis is written explicitly: a restart needs the block the chain
	// starts from, and nothing commits it.
	if err := store.PutBlock(c.HeadBlock(), nil, NewOutputTree(), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCanonical(c.HeadBlock(), func(h common.Hash) *Block { return c.blocks[h] }); err != nil {
		t.Fatal(err)
	}
	// Genesis needs a checkpoint too, or the first restart has nothing to
	// start replaying from.
	c.checkpointGenesis(context.Background())
	return c, store
}

// The property persistence exists for: a node that stops and starts serves the
// same chain, without replaying it from genesis.
func TestRestoreRebuildsTheChain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	c, store := newPersistentChain(t, dir, "restore")
	var built []*engine.ExecutionPayloadEnvelope
	for i := range 5 {
		env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i)))}, true))
		built = append(built, env)
	}
	head := c.HeadBlock()
	wantRoot := head.Header.Root
	wantOutputs := *head.Header.WithdrawalsHash
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process: new store handle, and a machine that can only come from
	// a checkpoint.
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loads := 0
	restored, ok, err := Restore(ctx, persistConfig(), reopened,
		func(_ context.Context, dir string) (machine.Machine, error) {
			loads++
			return machine.LoadMock(dir)
		}, mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the store reported itself empty after five blocks")
	}
	if loads != 1 {
		t.Fatalf("loaded %d checkpoints, want exactly 1", loads)
	}

	if got := restored.HeadBlock(); got.Hash() != head.Hash() {
		t.Fatalf("restored head %s (%d), want %s (%d)",
			got.Hash(), got.NumberU64(), head.Hash(), head.NumberU64())
	}
	if got := restored.HeadBlock().Header.Root; got != wantRoot {
		t.Errorf("restored state root %s, want %s", got, wantRoot)
	}
	if got := *restored.HeadBlock().Header.WithdrawalsHash; got != wantOutputs {
		t.Errorf("restored outputs root %s, want %s", got, wantOutputs)
	}
	if got := restored.GenesisHash(); got != c.GenesisHash() {
		t.Errorf("restored genesis %s, want %s — op-node would reject this node", got, c.GenesisHash())
	}

	// And it must be able to keep going, which needs a live machine at the head.
	env := buildBlock(restored, t, attrsOn(restored.HeadBlock(), [][]byte{depositTx(t, "after-restart")}, true))
	if env.ExecutionPayload.Number != head.NumberU64()+1 {
		t.Fatalf("built block %d after restoring at %d", env.ExecutionPayload.Number, head.NumberU64())
	}
}

// Restoring re-executes, so a checkpoint that does not match the blocks stored
// beside it has to be caught rather than served.
func TestRestoreRejectsAMismatchedCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, store := newPersistentChain(t, dir, "mismatch")
	for i := range 3 {
		buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i)))}, true))
	}
	store.Close()

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, _, err = Restore(ctx, persistConfig(), reopened,
		func(context.Context, string) (machine.Machine, error) {
			// A machine that is not the one the checkpoint holds.
			return machine.NewMock([]byte("wrong machine")), nil
		}, mempool.New(16))
	if err == nil {
		t.Fatal("restored a chain from a checkpoint that does not match its blocks")
	}
	t.Logf("rejected as it should: %v", err)
}

// An empty store is not an error: it is how a node starts for the first time.
func TestRestoreOnAnEmptyStore(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, ok, err := Restore(context.Background(), persistConfig(), store,
		func(context.Context, string) (machine.Machine, error) {
			t.Fatal("an empty store should not load a machine")
			return nil, nil
		}, mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	if ok || c != nil {
		t.Fatal("an empty store reported a chain")
	}
}

// Retention is the whole disk budget, since checkpoints do not deduplicate.
func TestCheckpointRetention(t *testing.T) {
	dir := t.TempDir()
	c, store := newPersistentChain(t, dir, "retention")
	defer store.Close()
	for i := range 9 {
		buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i)))}, true))
	}
	kept := store.Checkpoints()
	if len(kept) > persistConfig().CheckpointRetention {
		t.Fatalf("kept %d checkpoints, retention is %d", len(kept), persistConfig().CheckpointRetention)
	}
	if len(kept) == 0 {
		t.Fatal("no checkpoints were taken at all")
	}
	t.Logf("kept %d checkpoints, newest at block %d", len(kept), kept[len(kept)-1].Number)
}
