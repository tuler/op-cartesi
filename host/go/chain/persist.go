package chain

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

// Restore rebuilds a chain from a store, or reports that the store is empty so
// the caller can start a fresh one.
//
// Restoring is checkpoint plus replay. The newest machine checkpoint at or
// below the persisted head is loaded, and every block after it is re-executed
// in order — which is the same work a verifier does, so the state root each
// block reaches is checked against the one it was stored with. A checkpoint
// that has drifted from the blocks recorded alongside it therefore fails
// loudly here rather than serving a wrong chain.
//
// loadMachine turns a checkpoint directory into a machine. It is a parameter
// because only the caller knows how to reach an emulator server.
func Restore(
	ctx context.Context,
	cfg Config,
	store *Store,
	loadMachine func(ctx context.Context, dir string) (machine.Machine, error),
	pool *mempool.Pool,
) (*Chain, bool, error) {
	// The same normalisation New does. Without it a restored node would run on
	// a different config from the one that wrote the store — and since the
	// input envelope carries chain id and app contract, that shows up as a
	// state root mismatch during replay rather than as anything legible.
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}
	head, safe, finalized := store.Heads()
	if head == (common.Hash{}) {
		return nil, false, nil
	}
	headBlock := store.Block(head)
	if headBlock == nil {
		return nil, false, fmt.Errorf("store records head %s but does not hold it", head)
	}

	cp, ok := store.LatestCheckpoint(headBlock.NumberU64())
	if !ok {
		return nil, false, fmt.Errorf("store holds blocks up to %d but no machine checkpoint; delete the store to start over", headBlock.NumberU64())
	}
	base := store.Block(cp.Hash)
	if base == nil {
		return nil, false, fmt.Errorf("checkpoint at height %d has no canonical block", cp.Number)
	}

	m, err := loadMachine(ctx, cp.Dir)
	if err != nil {
		return nil, false, fmt.Errorf("loading checkpoint %s: %w", cp.Dir, err)
	}
	root, err := m.RootHash(ctx)
	if err != nil {
		return nil, false, err
	}
	if root != base.Header.Root {
		return nil, false, fmt.Errorf("checkpoint %s holds state root %s, but block %d records %s",
			cp.Dir, root, base.NumberU64(), base.Header.Root)
	}

	outputs, tree, inputs, err := store.Records(base.Hash())
	if base.NumberU64() == 0 {
		outputs, tree, inputs, err = nil, NewOutputTree(), 0, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("records for the checkpoint block: %w", err)
	}
	// The withdrawal commitment is not stored — it is a pure function of the
	// outputs already persisted per block, so it is rebuilt by walking the
	// chain, and checked against the root the checkpoint block committed.
	passer, err := rebuildPasser(store, base, tree)
	if err != nil {
		return nil, false, fmt.Errorf("rebuilding the withdrawal commitment: %w", err)
	}
	c := adopt(cfg, store, pool, base, m, outputs, tree, passer, inputs)

	slog.Info("restoring chain from store",
		"checkpoint", cp.Number, "head", headBlock.NumberU64(),
		"replay", headBlock.NumberU64()-base.NumberU64())

	for n := base.NumberU64() + 1; n <= headBlock.NumberU64(); n++ {
		hash := store.CanonicalHash(n)
		if hash == (common.Hash{}) {
			return nil, false, fmt.Errorf("no canonical block at height %d, but the head is %d", n, headBlock.NumberU64())
		}
		b := store.Block(hash)
		if b == nil {
			return nil, false, fmt.Errorf("canonical block %d (%s) is missing from the store", n, hash)
		}
		if err := c.replayStored(ctx, b); err != nil {
			return nil, false, fmt.Errorf("replaying block %d: %w", n, err)
		}
	}

	// Safe and finalized fall back to the checkpoint block rather than to
	// genesis: a restored chain starts at the checkpoint, and genesis is only
	// in the store. Pointing them at a block this node does not hold would
	// make the next forkchoice call fail as an invalid state.
	c.safe, c.finalized = base.Hash(), base.Hash()
	if c.blocks[safe] != nil {
		c.safe = safe
	}
	if c.blocks[finalized] != nil {
		c.finalized = finalized
	}
	c.prune(ctx)
	return c, true, nil
}

// replayStored re-executes one stored block on top of the current head and
// checks it reaches the state root and outputs commitment it was stored with.
func (c *Chain) replayStored(ctx context.Context, b *Block) error {
	parentMachine, ok := c.machines[b.Header.ParentHash]
	if !ok {
		return errUnknownParentState
	}
	work, err := parentMachine.Fork(ctx)
	if err != nil {
		return err
	}
	firstInput := c.inputs[b.Header.ParentHash]
	ic := c.cfg.inputContextOf(b.Header, firstInput)
	// replayImport forks per transaction and closes the machine it was handed,
	// so the block's machine is the one it returns — not the one passed in.
	res, err := c.replayImport(ctx, work, ic, b.Txs)
	if err != nil {
		return err
	}
	root, err := res.machine.RootHash(ctx)
	if err != nil {
		res.machine.Close(ctx)
		return err
	}
	if root != b.Header.Root {
		res.machine.Close(ctx)
		return fmt.Errorf("re-execution reaches state root %s, stored block says %s", root, b.Header.Root)
	}
	tree := c.trees[b.Header.ParentHash].Append(outputLeaves(res.outputs)...)
	passer, err := c.extendPasser(b.Header.ParentHash, res.outputs, tree)
	if err != nil {
		res.machine.Close(ctx)
		return err
	}
	if b.Header.WithdrawalsHash == nil || passer.Root() != *b.Header.WithdrawalsHash {
		res.machine.Close(ctx)
		return fmt.Errorf("re-execution reaches withdrawals root %s, stored block says %v", passer.Root(), b.Header.WithdrawalsHash)
	}
	c.commit(b, res.machine, res.outputs, tree, passer, firstInput)
	c.canonical[b.NumberU64()] = b.Hash()
	c.head = b.Hash()
	return nil
}

// persist writes a committed block through to the store, and takes a machine
// checkpoint when one is due. Callers hold c.mu.
//
// A failure to persist is logged rather than returned: the chain in memory is
// correct and op-node is waiting on an answer, so refusing the block would
// stall a chain that is otherwise fine. What a failure costs is a slower
// restart, not a wrong one.
func (c *Chain) persist(ctx context.Context, b *Block, outputs []TxOutputs, tree OutputTree, firstInput uint64) {
	if c.store == nil {
		return
	}
	if err := c.store.PutBlock(b, outputs, tree, firstInput+uint64(len(b.Txs))); err != nil {
		slog.Error("persisting block", "number", b.NumberU64(), "hash", b.Hash(), "err", err)
		return
	}
	if c.cfg.CheckpointInterval == 0 || b.NumberU64() == 0 {
		return
	}
	if b.NumberU64()%c.cfg.CheckpointInterval != 0 {
		return
	}
	c.checkpoint(ctx, b)
}

// checkpoint stores the machine as of a block. It runs on a fork so the
// chain's own snapshot is not tied up for the duration of the write, which is
// on the order of seconds for a real machine.
func (c *Chain) checkpoint(ctx context.Context, b *Block) {
	m, ok := c.machines[b.Hash()]
	if !ok {
		return
	}
	dir := c.store.CheckpointDir(b.NumberU64(), b.Hash())
	if _, err := os.Stat(dir); err == nil {
		return
	}
	forked, err := m.Fork(ctx)
	if err != nil {
		slog.Error("forking for a checkpoint", "block", b.NumberU64(), "err", err)
		return
	}
	defer forked.Close(ctx)
	if err := forked.Store(ctx, dir); err != nil {
		slog.Error("writing a checkpoint", "block", b.NumberU64(), "dir", dir, "err", err)
		os.RemoveAll(dir)
		return
	}
	slog.Info("wrote machine checkpoint", "block", b.NumberU64(), "dir", dir)
	c.store.PruneCheckpoints(c.cfg.CheckpointRetention)
}

// adopt builds a chain whose earliest known block is an arbitrary one rather
// than genesis. Restoring starts here: the checkpoint block takes the place
// genesis holds in a fresh chain, and everything before it lives only in the
// store.
func adopt(cfg Config, store *Store, pool *mempool.Pool, base *Block, m machine.Machine,
	outputs []TxOutputs, tree OutputTree, passer *PasserTrie, inputs uint64) *Chain {
	c := &Chain{
		cfg:       cfg,
		pool:      pool,
		store:     store,
		blocks:    map[common.Hash]*Block{base.Hash(): base},
		canonical: map[uint64]common.Hash{base.NumberU64(): base.Hash()},
		machines:  map[common.Hash]machine.Machine{base.Hash(): m},
		outputs:   map[common.Hash][]TxOutputs{base.Hash(): outputs},
		trees:     map[common.Hash]OutputTree{base.Hash(): tree},
		passers:   map[common.Hash]*PasserTrie{base.Hash(): passer},
		inputs:    map[common.Hash]uint64{base.Hash(): inputs},
		txBlocks:  map[common.Hash][]common.Hash{},
		pending:   map[engine.PayloadID]*pendingPayload{},
		// The genesis hash stays what the store says it is: op-node checks it
		// against the rollup config, and a restored node must answer the same
		// way as the one that wrote the store.
		genesisHash: store.CanonicalHash(0),
		head:        base.Hash(),
		safe:        base.Hash(),
		finalized:   base.Hash(),
	}
	if c.genesisHash == (common.Hash{}) {
		c.genesisHash = base.Hash()
	}
	for _, txo := range outputs {
		if txo.TxHash != (common.Hash{}) {
			c.txBlocks[txo.TxHash] = append(c.txBlocks[txo.TxHash], base.Hash())
		}
	}
	return c
}

// checkpointIfFinalized takes a checkpoint at the finalized head. A finalized
// block can never be reorged away, so a checkpoint there never has to be
// discarded — which is the ideal trigger. It is not the only one, because
// nothing finalizes on a devnet L1 and a chain that never finalizes would
// never checkpoint at all. Callers hold c.mu.
func (c *Chain) checkpointIfFinalized(ctx context.Context) {
	if c.cfg.CheckpointInterval == 0 || c.finalized == (common.Hash{}) {
		return
	}
	b, ok := c.blocks[c.finalized]
	if !ok || b.NumberU64() == 0 {
		return
	}
	if cp, ok := c.store.LatestCheckpoint(b.NumberU64()); ok && cp.Number >= b.NumberU64() {
		return
	}
	c.checkpoint(ctx, b)
}

// checkpointGenesis writes the checkpoint the first restart replays from.
// Every other checkpoint is triggered by height or finality, neither of which
// block 0 satisfies.
func (c *Chain) checkpointGenesis(ctx context.Context) {
	if c.store == nil {
		return
	}
	b := c.blocks[c.genesisHash]
	if b == nil {
		return
	}
	m, ok := c.machines[b.Hash()]
	if !ok {
		return
	}
	dir := c.store.CheckpointDir(0, b.Hash())
	if _, err := os.Stat(dir); err == nil {
		return
	}
	if err := m.Store(ctx, dir); err != nil {
		slog.Error("writing the genesis checkpoint", "err", err)
		os.RemoveAll(dir)
	}
}

// isCanonical reports whether a hash names a block on this chain, consulting
// the store for blocks older than the ones held in memory.
//
// A restored node starts at a checkpoint, so everything before it lives only
// on disk — but op-node goes on referring to those blocks as safe or finalized
// for as long as its own view lags. Rejecting them because they are not in
// memory would make a restored node fail its first forkchoice call.
// Callers hold c.mu.
func (c *Chain) isCanonical(hash common.Hash) bool {
	if b, ok := c.blocks[hash]; ok {
		return c.canonical[b.NumberU64()] == hash
	}
	if c.store == nil {
		return false
	}
	b := c.store.Block(hash)
	if b == nil {
		return false
	}
	return c.store.CanonicalHash(b.NumberU64()) == hash
}

// Adopt attaches a store to a chain that started fresh, writing genesis and
// its checkpoint so the next start has somewhere to restore from.
func (c *Chain) Adopt(ctx context.Context, store *Store) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = store
	genesis := c.blocks[c.genesisHash]
	if err := store.PutBlock(genesis, nil, NewOutputTree(), 0); err != nil {
		return err
	}
	if err := store.SetCanonical(genesis, func(h common.Hash) *Block { return c.blocks[h] }); err != nil {
		return err
	}
	c.checkpointGenesis(ctx)
	return nil
}

// Close releases the store. The machine is owned by the caller.
func (c *Chain) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return nil
	}
	if err := c.store.SetHeads(c.head, c.safe, c.finalized); err != nil {
		slog.Error("recording forkchoice on shutdown", "err", err)
	}
	return c.store.Close()
}
