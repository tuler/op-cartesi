package chain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
)

// Store is the chain's durable half: blocks and the per-block records derived
// from executing them, in a key-value database, plus machine checkpoints on
// disk.
//
// Blocks go through go-ethereum's own rawdb rather than a schema of our own.
// An op-cartesi block is a types.Header over a list of transactions, which is
// exactly what rawdb stores, so headers, bodies, the canonical number→hash
// mapping and the head pointers are all reuse — including the canonical
// rewriting that a reorg needs.
//
// What rawdb has no notion of is the machine: there is no analogue in an
// op-geth chain, where the state simply *is* the database. So the machine is
// checkpointed whole, at intervals, and the blocks after the newest checkpoint
// are re-executed on startup. See DESIGN §7b for why that shape rather than a
// per-block snapshot.
type Store struct {
	db  ethdb.Database
	dir string
}

// Table prefixes for the records rawdb does not model. They are single bytes
// chosen not to collide with rawdb's own schema, which uses "h", "b", "n",
// "H", "r", "l" and a set of longer word keys.
var (
	outputsPrefix = []byte("O") // block hash -> the machine's emissions per transaction
	treePrefix    = []byte("T") // block hash -> cumulative outputs accumulator
	inputsPrefix  = []byte("I") // block hash -> chain-wide inputs consumed through this block
	// rawdb models head and finalized but not the safe head, which is an OP
	// notion rather than an Ethereum one, so it gets a key of its own.
	safeHeadKey = []byte("SafeBlockHash")
)

// OpenStore opens (or creates) a store rooted at dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	kv, err := pebble.New(filepath.Join(dir, "chaindata"), 128, 256, "", false)
	if err != nil {
		// The database is locked by whoever holds it, and the underlying error
		// says only "resource temporarily unavailable" — which is not a hint
		// that a second node is pointed at the same directory.
		if errors.Is(err, syscall.EAGAIN) || strings.Contains(err.Error(), "resource temporarily unavailable") {
			return nil, fmt.Errorf("%s is already open; two nodes cannot share one store", dir)
		}
		return nil, fmt.Errorf("opening %s: %w", dir, err)
	}
	checkpoints := filepath.Join(dir, "checkpoints")
	if err := os.MkdirAll(checkpoints, 0o755); err != nil {
		kv.Close()
		return nil, err
	}
	return &Store{db: rawdb.NewDatabase(kv), dir: checkpoints}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// PutBlock writes a block and everything the chain derived from executing it.
// The write is batched so a crash cannot leave a block visible without its
// outputs.
func (s *Store) PutBlock(b *Block, outputs []TxOutputs, tree OutputTree, inputsConsumed uint64) error {
	txs := make([]*types.Transaction, 0, len(b.Txs))
	for i, raw := range b.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(raw); err != nil {
			return fmt.Errorf("block %s tx %d does not decode: %w", b.Hash(), i, err)
		}
		txs = append(txs, tx)
	}
	batch := s.db.NewBatch()
	rawdb.WriteHeader(batch, b.Header)
	rawdb.WriteBody(batch, b.Hash(), b.NumberU64(), &types.Body{Transactions: txs})

	encodedOutputs, err := rlp.EncodeToBytes(encodeOutputs(outputs))
	if err != nil {
		return err
	}
	encodedTree, err := rlp.EncodeToBytes(tree.encode())
	if err != nil {
		return err
	}
	if err := batch.Put(key(outputsPrefix, b.Hash()), encodedOutputs); err != nil {
		return err
	}
	if err := batch.Put(key(treePrefix, b.Hash()), encodedTree); err != nil {
		return err
	}
	if err := batch.Put(key(inputsPrefix, b.Hash()), binary.BigEndian.AppendUint64(nil, inputsConsumed)); err != nil {
		return err
	}
	return batch.Write()
}

// SetCanonical records the canonical chain up to head, rewriting any numbers
// a reorg has invalidated. walkParent supplies each block's parent.
func (s *Store) SetCanonical(head *Block, walkParent func(common.Hash) *Block) error {
	batch := s.db.NewBatch()
	// Numbers past the new head belong to a chain that lost; leaving them
	// would make a restart read blocks that are no longer part of the chain.
	for n := head.NumberU64() + 1; ; n++ {
		h := rawdb.ReadCanonicalHash(s.db, n)
		if h == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(batch, n)
	}
	for cur := head; cur != nil; {
		if rawdb.ReadCanonicalHash(s.db, cur.NumberU64()) == cur.Hash() {
			break
		}
		rawdb.WriteCanonicalHash(batch, cur.Hash(), cur.NumberU64())
		if cur.NumberU64() == 0 {
			break
		}
		cur = walkParent(cur.Header.ParentHash)
	}
	rawdb.WriteHeadBlockHash(batch, head.Hash())
	rawdb.WriteHeadHeaderHash(batch, head.Hash())
	return batch.Write()
}

// SetHeads records the three forkchoice pointers.
func (s *Store) SetHeads(head, safe, finalized common.Hash) error {
	batch := s.db.NewBatch()
	rawdb.WriteHeadBlockHash(batch, head)
	rawdb.WriteHeadHeaderHash(batch, head)
	if safe != (common.Hash{}) {
		if err := batch.Put(safeHeadKey, safe.Bytes()); err != nil {
			return err
		}
	}
	if finalized != (common.Hash{}) {
		rawdb.WriteFinalizedBlockHash(batch, finalized)
	}
	return batch.Write()
}

// Heads reads back the forkchoice pointers. A zero head means an empty store.
func (s *Store) Heads() (head, safe, finalized common.Hash) {
	if raw, err := s.db.Get(safeHeadKey); err == nil {
		safe = common.BytesToHash(raw)
	}
	return rawdb.ReadHeadBlockHash(s.db), safe, rawdb.ReadFinalizedBlockHash(s.db)
}

// Block reads a block back. It returns nil when the store does not have it.
func (s *Store) Block(hash common.Hash) *Block {
	number, ok := rawdb.ReadHeaderNumber(s.db, hash)
	if !ok {
		return nil
	}
	header := rawdb.ReadHeader(s.db, hash, number)
	if header == nil {
		return nil
	}
	body := rawdb.ReadBody(s.db, hash, number)
	if body == nil {
		return nil
	}
	txs := make([][]byte, 0, len(body.Transactions))
	for _, tx := range body.Transactions {
		raw, err := tx.MarshalBinary()
		if err != nil {
			return nil
		}
		txs = append(txs, raw)
	}
	// newBlock rather than a struct literal: Block caches its hash, and a
	// literal leaves that zero, which makes every lookup keyed by hash miss.
	return newBlock(header, txs)
}

// CanonicalHash returns the block hash recorded at a height.
func (s *Store) CanonicalHash(number uint64) common.Hash {
	return rawdb.ReadCanonicalHash(s.db, number)
}

// Records reads back what executing a block produced.
func (s *Store) Records(hash common.Hash) (outputs []TxOutputs, tree OutputTree, inputsConsumed uint64, err error) {
	encodedOutputs, err := s.db.Get(key(outputsPrefix, hash))
	if err != nil {
		return nil, OutputTree{}, 0, fmt.Errorf("no records for block %s: %w", hash, err)
	}
	var storedOuts []storedTxOutputs
	if err := rlp.DecodeBytes(encodedOutputs, &storedOuts); err != nil {
		return nil, OutputTree{}, 0, err
	}
	outputs = decodeOutputs(storedOuts)
	encodedTree, err := s.db.Get(key(treePrefix, hash))
	if err != nil {
		return nil, OutputTree{}, 0, err
	}
	var stored storedTree
	if err := rlp.DecodeBytes(encodedTree, &stored); err != nil {
		return nil, OutputTree{}, 0, err
	}
	encodedInputs, err := s.db.Get(key(inputsPrefix, hash))
	if err != nil {
		return nil, OutputTree{}, 0, err
	}
	return outputs, stored.decode(), binary.BigEndian.Uint64(encodedInputs), nil
}

// --- machine checkpoints ---------------------------------------------------

// Checkpoint identifies a stored machine.
type Checkpoint struct {
	Number uint64
	Hash   common.Hash
	Dir    string
}

// CheckpointDir is where a checkpoint for a block would be written. The block
// number leads so the directory sorts by height, and the hash disambiguates
// two blocks at the same height across a reorg.
func (s *Store) CheckpointDir(number uint64, hash common.Hash) string {
	return filepath.Join(s.dir, fmt.Sprintf("%012d-%s", number, hash.Hex()[2:14]))
}

// Checkpoints lists what is on disk, oldest first. A directory that does not
// parse is ignored rather than fatal: the machine's own files live inside
// these directories and a partially written one should not stop a restart.
func (s *Store) Checkpoints() []Checkpoint {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Checkpoint
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, _, ok := strings.Cut(e.Name(), "-")
		if !ok {
			continue
		}
		number, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			continue
		}
		dir := filepath.Join(s.dir, e.Name())
		hash := s.CanonicalHash(number)
		out = append(out, Checkpoint{Number: number, Hash: hash, Dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// LatestCheckpoint returns the newest checkpoint at or below number, if any.
func (s *Store) LatestCheckpoint(number uint64) (Checkpoint, bool) {
	list := s.Checkpoints()
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Number <= number {
			return list[i], true
		}
	}
	return Checkpoint{}, false
}

// PruneCheckpoints keeps the newest keep checkpoints and removes the rest. A
// stored machine is several hundred megabytes and nothing deduplicates them,
// so retention is the whole of the disk budget.
func (s *Store) PruneCheckpoints(keep int) {
	list := s.Checkpoints()
	if len(list) <= keep {
		return
	}
	for _, cp := range list[:len(list)-keep] {
		os.RemoveAll(cp.Dir)
	}
}

func key(prefix []byte, hash common.Hash) []byte {
	return append(append([]byte{}, prefix...), hash.Bytes()...)
}

// storedTxOutputs is the on-disk shape of TxOutputs. It exists partly because
// RLP cannot encode a plain int, and partly for the same reason storedTree
// does: the in-memory type is free to change without silently changing what
// previously written stores mean.
type storedTxOutputs struct {
	TxIndex  uint64
	TxHash   common.Hash
	Accepted bool
	Cycles   uint64
	Outputs  [][]byte
	Reports  [][]byte
}

func encodeOutputs(outputs []TxOutputs) []storedTxOutputs {
	out := make([]storedTxOutputs, 0, len(outputs))
	for _, o := range outputs {
		out = append(out, storedTxOutputs{
			TxIndex:  uint64(o.TxIndex),
			TxHash:   o.TxHash,
			Accepted: o.Accepted,
			Cycles:   o.Cycles,
			Outputs:  o.Outputs,
			Reports:  o.Reports,
		})
	}
	return out
}

func decodeOutputs(stored []storedTxOutputs) []TxOutputs {
	if len(stored) == 0 {
		return nil
	}
	out := make([]TxOutputs, 0, len(stored))
	for _, o := range stored {
		out = append(out, TxOutputs{
			TxIndex:  int(o.TxIndex),
			TxHash:   o.TxHash,
			Accepted: o.Accepted,
			Cycles:   o.Cycles,
			Outputs:  o.Outputs,
			Reports:  o.Reports,
		})
	}
	return out
}

// storedTree is the accumulator's RLP shape. OutputTree's fields are
// unexported, so the encoding is written out rather than derived — which also
// means a change to the in-memory representation has to be a deliberate change
// to what is on disk.
type storedTree struct {
	Count  uint64
	Branch []common.Hash
}

func (t OutputTree) encode() storedTree {
	return storedTree{Count: t.count, Branch: t.branch}
}

func (s storedTree) decode() OutputTree {
	return OutputTree{count: s.Count, branch: s.Branch}
}
