// Package chain maintains the L2 block chain whose execution layer is a
// Cartesi Machine. Blocks are Ethereum-shaped headers over an ordered list of
// raw transactions; executing a block means feeding those transactions, in
// order, to the machine as CMIO inputs and committing the machine's Merkle
// root hash as the block's stateRoot. Recent blocks keep a live machine
// snapshot (a fork of the emulator) so the chain can build on any of them and
// re-execute payloads for verification.
package chain

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

var errUnknownParentState = errors.New("no machine snapshot for parent block")

type pendingPayload struct {
	data       *engine.ExecutableData
	beaconRoot *common.Hash
	machine    machine.Machine
	outputs    []TxOutputs
	tree       OutputTree
}

type Chain struct {
	mu   sync.RWMutex
	cfg  Config
	pool *mempool.Pool

	blocks    map[common.Hash]*Block
	canonical map[uint64]common.Hash
	machines  map[common.Hash]machine.Machine
	// outputs are retained for as long as their block is, rather than for the
	// machine-snapshot window: they are small, and serving receipts means
	// answering for old blocks. Like blocks, they are in-memory only and will
	// need persistence and a retention policy before this is long-lived.
	outputs map[common.Hash][]TxOutputs
	// trees carries the cumulative outputs accumulator per block, so a child
	// block can extend its parent's tree across forks and reorgs.
	trees   map[common.Hash]OutputTree
	pending map[engine.PayloadID]*pendingPayload

	genesisHash common.Hash
	head        common.Hash
	safe        common.Hash
	finalized   common.Hash
}

// New creates a chain from a machine parked at its first input-wait yield.
// The machine's current root hash becomes the genesis stateRoot; the chain
// takes ownership of the machine.
func New(ctx context.Context, cfg Config, m machine.Machine, pool *mempool.Pool) (*Chain, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	root, err := m.RootHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading genesis root hash: %w", err)
	}
	genesisExtra := cfg.genesisExtraData()
	withdrawalsHash := types.EmptyWithdrawalsHash
	var requestsHash *common.Hash
	if cfg.IsIsthmus(cfg.GenesisTimestamp) {
		// Genesis has produced no outputs, so it commits to the empty tree.
		withdrawalsHash = NewOutputTree().Root()
		requestsHash = &types.EmptyRequestsHash
	}
	blobGasUsed := uint64(0)
	excessBlobGas := uint64(0)
	header := &types.Header{
		RequestsHash:     requestsHash,
		ParentHash:       common.Hash{},
		UncleHash:        types.EmptyUncleHash,
		Coinbase:         common.Address{},
		Root:             root,
		TxHash:           types.EmptyTxsHash,
		ReceiptHash:      types.EmptyReceiptsHash,
		Bloom:            types.Bloom{},
		Difficulty:       new(big.Int),
		Number:           new(big.Int),
		GasLimit:         cfg.GasLimit,
		GasUsed:          0,
		Time:             cfg.GenesisTimestamp,
		Extra:            genesisExtra,
		MixDigest:        common.Hash{},
		Nonce:            types.BlockNonce{},
		BaseFee:          cfg.BaseFee,
		WithdrawalsHash:  &withdrawalsHash,
		BlobGasUsed:      &blobGasUsed,
		ExcessBlobGas:    &excessBlobGas,
		ParentBeaconRoot: &common.Hash{},
	}
	genesis := newBlock(header, nil)
	c := &Chain{
		cfg:         cfg,
		pool:        pool,
		blocks:      map[common.Hash]*Block{genesis.Hash(): genesis},
		canonical:   map[uint64]common.Hash{0: genesis.Hash()},
		machines:    map[common.Hash]machine.Machine{genesis.Hash(): m},
		outputs:     map[common.Hash][]TxOutputs{},
		trees:       map[common.Hash]OutputTree{genesis.Hash(): NewOutputTree()},
		pending:     map[engine.PayloadID]*pendingPayload{},
		genesisHash: genesis.Hash(),
		head:        genesis.Hash(),
		safe:        genesis.Hash(),
		finalized:   genesis.Hash(),
	}
	return c, nil
}

func (c *Chain) Config() Config           { return c.cfg }
func (c *Chain) GenesisHash() common.Hash { return c.genesisHash }

func (c *Chain) HeadBlock() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[c.head]
}

func (c *Chain) SafeBlock() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[c.safe]
}

func (c *Chain) FinalizedBlock() *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[c.finalized]
}

func (c *Chain) BlockByHash(h common.Hash) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blocks[h]
}

// BlockByNumber returns the canonical block at the given height.
func (c *Chain) BlockByNumber(n uint64) *Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.canonical[n]
	if !ok {
		return nil
	}
	return c.blocks[h]
}

// ForkchoiceUpdated implements the state transition behind
// engine_forkchoiceUpdatedV3: reorg to the given head, record safe/finalized,
// and optionally build a payload on top of the head.
func (c *Chain) ForkchoiceUpdated(ctx context.Context, fc engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes) (*engine.ForkChoiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	head, ok := c.blocks[fc.HeadBlockHash]
	if !ok {
		return &engine.ForkChoiceResponse{
			PayloadStatus: engine.PayloadStatusV1{Status: engine.SYNCING},
		}, nil
	}
	c.reorgTo(head)
	for _, ref := range []struct {
		target *common.Hash
		hash   common.Hash
	}{{&c.safe, fc.SafeBlockHash}, {&c.finalized, fc.FinalizedBlockHash}} {
		if ref.hash == (common.Hash{}) {
			continue
		}
		b, ok := c.blocks[ref.hash]
		if !ok || c.canonical[b.NumberU64()] != ref.hash {
			return nil, engine.InvalidForkChoiceState
		}
		*ref.target = ref.hash
	}
	c.head = head.Hash()
	c.prune(ctx)

	valid := engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &head.hash}
	if attrs == nil {
		return &engine.ForkChoiceResponse{PayloadStatus: valid}, nil
	}
	if attrs.Timestamp <= head.Time() {
		return nil, engine.InvalidPayloadAttributes
	}
	id, err := c.buildPayload(ctx, head, attrs)
	if err != nil {
		return nil, fmt.Errorf("building payload: %w", err)
	}
	return &engine.ForkChoiceResponse{PayloadStatus: valid, PayloadID: &id}, nil
}

// reorgTo makes the chain ending at head canonical. Callers hold c.mu.
func (c *Chain) reorgTo(head *Block) {
	for n := range c.canonical {
		if n > head.NumberU64() {
			delete(c.canonical, n)
		}
	}
	for cur := head; ; {
		if c.canonical[cur.NumberU64()] == cur.Hash() {
			break
		}
		c.canonical[cur.NumberU64()] = cur.Hash()
		if cur.NumberU64() == 0 {
			break
		}
		parent, ok := c.blocks[cur.Header.ParentHash]
		if !ok {
			break
		}
		cur = parent
	}
}

// prune closes machine snapshots that fell out of the retention window and
// drops pending payloads that can no longer become the next block. Callers
// hold c.mu.
func (c *Chain) prune(ctx context.Context) {
	headNum := c.blocks[c.head].NumberU64()
	var cutoff uint64
	if headNum > uint64(c.cfg.MaxSnapshots) {
		cutoff = headNum - uint64(c.cfg.MaxSnapshots)
	}
	protected := map[common.Hash]struct{}{c.head: {}, c.safe: {}, c.finalized: {}}
	for h, m := range c.machines {
		if _, ok := protected[h]; ok {
			continue
		}
		if c.blocks[h].NumberU64() >= cutoff {
			continue
		}
		if err := m.Close(ctx); err != nil {
			slog.Warn("closing pruned machine snapshot", "block", h, "err", err)
		}
		delete(c.machines, h)
	}
	for id, p := range c.pending {
		if p.data.Number <= headNum {
			if err := p.machine.Close(ctx); err != nil {
				slog.Warn("closing stale pending payload", "id", id, "err", err)
			}
			delete(c.pending, id)
		}
	}
}

func payloadID(parent common.Hash, attrs *engine.PayloadAttributes) (engine.PayloadID, error) {
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return engine.PayloadID{}, err
	}
	sum := sha256.Sum256(append(parent.Bytes(), encoded...))
	var id engine.PayloadID
	copy(id[:], sum[:8])
	return id, nil
}

// buildPayload executes deposits from the attributes plus pending mempool
// transactions on a fork of the parent's machine and stores the resulting
// payload for engine_getPayload. Callers hold c.mu.
func (c *Chain) buildPayload(ctx context.Context, parent *Block, attrs *engine.PayloadAttributes) (engine.PayloadID, error) {
	parentMachine, ok := c.machines[parent.Hash()]
	if !ok {
		return engine.PayloadID{}, fmt.Errorf("%w: %s", errUnknownParentState, parent.Hash())
	}
	id, err := payloadID(parent.Hash(), attrs)
	if err != nil {
		return engine.PayloadID{}, err
	}
	if _, ok := c.pending[id]; ok {
		return id, nil
	}

	gasLimit := parent.Header.GasLimit
	if attrs.GasLimit != nil {
		gasLimit = *attrs.GasLimit
	}
	var poolTxs [][]byte
	if !attrs.NoTxPool && c.pool != nil {
		poolTxs = c.pool.Pending()
	}

	work, err := parentMachine.Fork(ctx)
	if err != nil {
		return engine.PayloadID{}, fmt.Errorf("forking parent machine: %w", err)
	}
	exec, err := c.applyBuild(ctx, work, attrs.Transactions, poolTxs, gasLimit)
	if err != nil {
		return engine.PayloadID{}, err
	}
	root, err := exec.machine.RootHash(ctx)
	if err != nil {
		exec.machine.Close(ctx)
		return engine.PayloadID{}, fmt.Errorf("reading root hash: %w", err)
	}

	extra, err := c.cfg.extraData(attrs.Timestamp, attrs.EIP1559Params)
	if err != nil {
		exec.machine.Close(ctx)
		return engine.PayloadID{}, err
	}
	tree, ok := c.trees[parent.Hash()]
	if !ok {
		exec.machine.Close(ctx)
		return engine.PayloadID{}, fmt.Errorf("no outputs accumulator for parent %s", parent.Hash())
	}
	tree = tree.Append(outputLeaves(exec.outputs)...)

	header := buildHeader(c.cfg, parent.Header, attrs, root, exec.included, min(exec.gasUsed, gasLimit), gasLimit, c.cfg.BaseFee, extra, tree.Root())
	beaconRoot := header.ParentBeaconRoot
	c.pending[id] = &pendingPayload{
		data:       executableData(header, exec.included),
		beaconRoot: beaconRoot,
		machine:    exec.machine,
		outputs:    exec.outputs,
		tree:       tree,
	}
	return id, nil
}

// execResult is the outcome of running a block's inputs through the machine.
type execResult struct {
	included [][]byte
	gasUsed  uint64
	outputs  []TxOutputs
	machine  machine.Machine
}

// applyBuild is the sequencer-side execution loop. Deposits are mandatory:
// one failing hard (cycle limit, halt) aborts the build. Pool transactions
// that reject or fail are dropped from the pool and excluded from the block.
// Each input runs on a fork so a failed input leaves no state behind; the
// returned machine holds the post-block state and the caller owns it.
func (c *Chain) applyBuild(ctx context.Context, work machine.Machine, deposits, poolTxs [][]byte, gasLimit uint64) (*execResult, error) {
	cur := work
	out := &execResult{}
	fail := func(e error) (*execResult, error) {
		cur.Close(ctx)
		return nil, e
	}
	record := func(tx []byte, res *machine.AdvanceResult) {
		out.outputs = append(out.outputs, collectOutputs(len(out.included), tx, res))
		out.included = append(out.included, tx)
		out.gasUsed += res.Cycles / CyclesPerGas
	}
	for _, tx := range deposits {
		cand, err := cur.Fork(ctx)
		if err != nil {
			return fail(err)
		}
		res, err := cand.AdvanceInput(ctx, tx, c.cfg.MaxCyclesPerInput)
		if err != nil {
			cand.Close(ctx)
			return fail(fmt.Errorf("deposit transaction failed: %w", err))
		}
		record(tx, res)
		if res.Accepted {
			cur.Close(ctx)
			cur = cand
		} else {
			cand.Close(ctx)
		}
	}
	var dropped []common.Hash
	for _, tx := range poolTxs {
		if out.gasUsed >= gasLimit {
			break
		}
		cand, err := cur.Fork(ctx)
		if err != nil {
			return fail(err)
		}
		res, err := cand.AdvanceInput(ctx, tx, c.cfg.MaxCyclesPerInput)
		if err != nil || !res.Accepted {
			cand.Close(ctx)
			if h, herr := txHash(tx); herr == nil {
				dropped = append(dropped, h)
			}
			continue
		}
		record(tx, res)
		cur.Close(ctx)
		cur = cand
	}
	if c.pool != nil {
		c.pool.Forget(dropped)
	}
	out.machine = cur
	return out, nil
}

// replayImport is the verifier-side execution loop: every transaction in the
// block is applied; one that rejects or fails contributes no state change
// (and, on hard failure, no gas). The rules must mirror applyBuild exactly so
// builder and verifiers agree on stateRoot, gasUsed and the recorded outputs.
func (c *Chain) replayImport(ctx context.Context, work machine.Machine, txs [][]byte) (*execResult, error) {
	cur := work
	out := &execResult{included: txs}
	for i, tx := range txs {
		cand, err := cur.Fork(ctx)
		if err != nil {
			cur.Close(ctx)
			return nil, err
		}
		res, err := cand.AdvanceInput(ctx, tx, c.cfg.MaxCyclesPerInput)
		if err != nil {
			cand.Close(ctx)
			continue
		}
		out.outputs = append(out.outputs, collectOutputs(i, tx, res))
		out.gasUsed += res.Cycles / CyclesPerGas
		if res.Accepted {
			cur.Close(ctx)
			cur = cand
		} else {
			cand.Close(ctx)
		}
	}
	out.machine = cur
	return out, nil
}

// Payload returns a previously built payload by ID.
func (c *Chain) Payload(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.pending[id]
	if !ok {
		return nil, false
	}
	return &engine.ExecutionPayloadEnvelope{
		ExecutionPayload: p.data,
		BlockValue:       new(big.Int),
		BlobsBundle: &engine.BlobsBundle{
			Commitments: []hexutil.Bytes{},
			Proofs:      []hexutil.Bytes{},
			Blobs:       []hexutil.Bytes{},
		},
		Override:              false,
		ParentBeaconBlockRoot: p.beaconRoot,
	}, true
}

// ImportPayload implements engine_newPayloadV3: validate the payload's block
// hash, then either adopt the locally built block or re-execute the payload's
// transactions and check the resulting state root.
func (c *Chain) ImportPayload(ctx context.Context, data *engine.ExecutableData, beaconRoot *common.Hash) (engine.PayloadStatusV1, error) {
	header, err := c.cfg.headerFromPayload(data, beaconRoot)
	if err != nil {
		return invalidStatus(nil, err.Error()), nil
	}
	hash := header.Hash()
	if hash != data.BlockHash {
		return invalidStatus(nil, fmt.Sprintf("block hash mismatch: computed %s, payload claims %s", hash, data.BlockHash)), nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.blocks[hash]; ok {
		return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &hash}, nil
	}
	parent, ok := c.blocks[data.ParentHash]
	if !ok {
		return engine.PayloadStatusV1{Status: engine.SYNCING}, nil
	}
	parentHash := parent.Hash()
	if data.Number != parent.NumberU64()+1 {
		return invalidStatus(&parentHash, "block number is not parent number + 1"), nil
	}
	if data.Timestamp <= parent.Time() {
		return invalidStatus(&parentHash, "timestamp not after parent"), nil
	}

	// Adopt the machine of a locally built payload instead of re-executing.
	for id, p := range c.pending {
		if p.data.BlockHash == hash {
			c.commit(newBlock(header, data.Transactions), p.machine, p.outputs, p.tree)
			delete(c.pending, id)
			return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &hash}, nil
		}
	}

	parentMachine, ok := c.machines[data.ParentHash]
	if !ok {
		slog.Warn("cannot validate payload: parent machine snapshot pruned", "block", hash, "parent", data.ParentHash)
		return engine.PayloadStatusV1{Status: engine.SYNCING}, nil
	}
	work, err := parentMachine.Fork(ctx)
	if err != nil {
		return engine.PayloadStatusV1{}, fmt.Errorf("forking parent machine: %w", err)
	}
	exec, err := c.replayImport(ctx, work, data.Transactions)
	if err != nil {
		return engine.PayloadStatusV1{}, err
	}
	root, err := exec.machine.RootHash(ctx)
	if err != nil {
		exec.machine.Close(ctx)
		return engine.PayloadStatusV1{}, fmt.Errorf("reading root hash: %w", err)
	}
	if root != data.StateRoot {
		exec.machine.Close(ctx)
		return invalidStatus(&parentHash, fmt.Sprintf("state root mismatch: computed %s, payload claims %s", root, data.StateRoot)), nil
	}
	if capped := min(exec.gasUsed, data.GasLimit); capped != data.GasUsed {
		exec.machine.Close(ctx)
		return invalidStatus(&parentHash, fmt.Sprintf("gas used mismatch: computed %d, payload claims %d", capped, data.GasUsed)), nil
	}
	// Re-derive the outputs commitment from the outputs this node just
	// observed. A payload claiming a withdrawal set the machine did not
	// produce is rejected here, which is what keeps the output root — and so
	// the withdrawals proven against it — honest.
	tree, ok := c.trees[data.ParentHash]
	if !ok {
		exec.machine.Close(ctx)
		slog.Warn("cannot validate payload: parent outputs accumulator missing", "block", hash, "parent", data.ParentHash)
		return engine.PayloadStatusV1{Status: engine.SYNCING}, nil
	}
	tree = tree.Append(outputLeaves(exec.outputs)...)
	if data.WithdrawalsRoot != nil && tree.Root() != *data.WithdrawalsRoot {
		exec.machine.Close(ctx)
		return invalidStatus(&parentHash, fmt.Sprintf("outputs root mismatch: computed %s, payload claims %s", tree.Root(), *data.WithdrawalsRoot)), nil
	}
	c.commit(newBlock(header, data.Transactions), exec.machine, exec.outputs, tree)
	return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &hash}, nil
}

// commit stores a validated block, its post-state machine and the machine
// emissions recorded while executing it. Canonicality is decided separately by
// ForkchoiceUpdated. Callers hold c.mu.
func (c *Chain) commit(b *Block, m machine.Machine, outputs []TxOutputs, tree OutputTree) {
	c.blocks[b.Hash()] = b
	c.machines[b.Hash()] = m
	c.outputs[b.Hash()] = outputs
	c.trees[b.Hash()] = tree
	if c.pool != nil {
		var hashes []common.Hash
		for _, tx := range b.Txs {
			if h, err := txHash(tx); err == nil {
				hashes = append(hashes, h)
			}
		}
		c.pool.Forget(hashes)
	}
}

func txHash(raw []byte) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	return tx.Hash(), nil
}

func invalidStatus(latestValid *common.Hash, msg string) engine.PayloadStatusV1 {
	return engine.PayloadStatusV1{Status: engine.INVALID, LatestValidHash: latestValid, ValidationError: &msg}
}
