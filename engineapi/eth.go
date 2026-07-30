package engineapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/mempool"
)

type EthAPI struct {
	chain *chain.Chain
	pool  *mempool.Pool
}

func NewEthAPI(c *chain.Chain, pool *mempool.Pool) *EthAPI {
	return &EthAPI{chain: c, pool: pool}
}

func (e *EthAPI) ChainId() *hexutil.Big {
	return (*hexutil.Big)(new(big.Int).SetUint64(e.chain.Config().ChainID))
}

func (e *EthAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(e.chain.HeadBlock().NumberU64())
}

func (e *EthAPI) Syncing() (any, error) {
	return false, nil
}

func (e *EthAPI) GetBlockByHash(_ context.Context, hash common.Hash, fullTx bool) (map[string]any, error) {
	return marshalBlock(e.chain.BlockByHash(hash), fullTx)
}

func (e *EthAPI) GetBlockByNumber(_ context.Context, number rpc.BlockNumber, fullTx bool) (map[string]any, error) {
	b, err := e.blockFrom(rpc.BlockNumberOrHashWithNumber(number))
	if err != nil {
		return nil, err
	}
	return marshalBlock(b, fullTx)
}

func (e *EthAPI) SendRawTransaction(_ context.Context, raw hexutil.Bytes) (common.Hash, error) {
	if e.pool == nil {
		return common.Hash{}, fmt.Errorf("transaction submission disabled on this node")
	}
	return e.pool.Add(raw)
}

// GetTransactionReceipt returns a receipt synthesized from what the machine
// emitted while processing the transaction: provable outputs become logs,
// acceptance becomes status, and consumed mcycles become gas.
//
// Nothing on the OP Stack's critical path reads L2 receipts — derivation
// fetches L1 receipts, and the batcher reads blocks — so these serve users
// rather than the protocol. They are deliberately not committed to: the header
// keeps an empty receipts root and bloom, so the encoding stays changeable.
func (e *EthAPI) GetTransactionReceipt(_ context.Context, hash common.Hash) (map[string]any, error) {
	receipt := e.chain.ReceiptByTxHash(hash)
	if receipt == nil {
		return nil, nil
	}
	return marshalReceipt(receipt), nil
}

// GetBlockReceipts returns every receipt in a block.
func (e *EthAPI) GetBlockReceipts(_ context.Context, id rpc.BlockNumberOrHash) ([]map[string]any, error) {
	b, err := e.blockFrom(id)
	if err != nil || b == nil {
		return nil, err
	}
	receipts := e.chain.BlockReceipts(b.Hash())
	out := make([]map[string]any, 0, len(receipts))
	for _, r := range receipts {
		out = append(out, marshalReceipt(r))
	}
	return out, nil
}

// CallArgs is the subset of eth_call's argument object this chain can act on.
// The machine has no notion of caller, value or gas, so only the payload is
// meaningful; the other fields are accepted and ignored so that standard
// tooling can call without special-casing.
type CallArgs struct {
	To    *common.Address `json:"to"`
	Data  *hexutil.Bytes  `json:"data"`
	Input *hexutil.Bytes  `json:"input"`
}

func (a CallArgs) payload() []byte {
	if a.Input != nil {
		return *a.Input
	}
	if a.Data != nil {
		return *a.Data
	}
	return nil
}

// Call answers a read-only query by running the machine's inspect protocol
// against the state at the requested block, on a fork that is then discarded.
//
// This is the natural counterpart of eth_call: it reads state without changing
// it. The reply is the concatenation of the reports the guest emitted, since
// eth_call has a single return value; cartesi_inspect returns them
// individually for callers that emit more than one.
func (e *EthAPI) Call(ctx context.Context, args CallArgs, id *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	b, err := e.blockFromOptional(id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	res, err := e.chain.Inspect(ctx, b.Hash(), args.payload())
	if err != nil {
		return nil, err
	}
	if !res.Accepted {
		return nil, fmt.Errorf("inspect rejected the query")
	}
	var out []byte
	for _, report := range res.Reports {
		out = append(out, report...)
	}
	return out, nil
}

// MinerAPI serves the miner namespace op-batcher requires.
type MinerAPI struct {
	chain *chain.Chain
}

func NewMinerAPI(c *chain.Chain) *MinerAPI {
	return &MinerAPI{chain: c}
}

// SetMaxDASize is op-batcher's backpressure: when batches back up on L1 it
// asks the sequencer to build smaller blocks. The limits are honoured for
// mempool transactions; deposits are forced by op-node and cannot be shed.
//
// op-batcher treats an engine that does not serve this method as a fatal
// error and shuts down, so serving it is not optional for a chain that wants
// to be batched.
func (m *MinerAPI) SetMaxDASize(maxTxSize, maxBlockSize hexutil.Big) bool {
	m.chain.SetMaxDASize((*big.Int)(&maxTxSize), (*big.Int)(&maxBlockSize))
	return true
}

func marshalReceipt(r *chain.Receipt) map[string]any {
	logs := make([]map[string]any, 0, len(r.Logs))
	for _, l := range r.Logs {
		logs = append(logs, map[string]any{
			"address":          l.Address,
			"topics":           l.Topics,
			"data":             hexutil.Bytes(l.Data),
			"blockNumber":      hexutil.Uint64(l.BlockNumber),
			"transactionHash":  l.TxHash,
			"transactionIndex": hexutil.Uint(l.TxIndex),
			"blockHash":        l.BlockHash,
			"logIndex":         hexutil.Uint(l.Index),
			"removed":          false,
		})
	}
	m := map[string]any{
		"transactionHash":   r.TxHash,
		"transactionIndex":  hexutil.Uint(r.TxIndex),
		"blockHash":         r.BlockHash,
		"blockNumber":       hexutil.Uint64(r.BlockNumber),
		"from":              r.From,
		"to":                r.To,
		"type":              hexutil.Uint(r.Type),
		"status":            hexutil.Uint64(r.Status),
		"gasUsed":           hexutil.Uint64(r.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(r.CumulativeGasUsed),
		"contractAddress":   nil,
		"logs":              logs,
		"logsBloom":         r.Bloom,
	}
	if r.EffectiveGasPrice != nil {
		m["effectiveGasPrice"] = (*hexutil.Big)(r.EffectiveGasPrice)
	}
	return m
}

func (e *EthAPI) blockFrom(id rpc.BlockNumberOrHash) (*chain.Block, error) {
	return blockFromChain(e.chain, id)
}

func (e *EthAPI) blockFromOptional(id *rpc.BlockNumberOrHash) (*chain.Block, error) {
	return blockFromChainOptional(e.chain, id)
}

// blockFromChain resolves a block number-or-hash to a block.
func blockFromChain(c *chain.Chain, id rpc.BlockNumberOrHash) (*chain.Block, error) {
	if hash, ok := id.Hash(); ok {
		return c.BlockByHash(hash), nil
	}
	number, ok := id.Number()
	if !ok {
		return nil, fmt.Errorf("block identifier has neither number nor hash")
	}
	switch number {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		return c.HeadBlock(), nil
	case rpc.SafeBlockNumber:
		return c.SafeBlock(), nil
	case rpc.FinalizedBlockNumber:
		return c.FinalizedBlock(), nil
	case rpc.EarliestBlockNumber:
		return c.BlockByNumber(0), nil
	default:
		if number < 0 {
			return nil, fmt.Errorf("unsupported block number %d", number)
		}
		return c.BlockByNumber(uint64(number)), nil
	}
}

// blockFromChainOptional defaults to the head when no block is given.
func blockFromChainOptional(c *chain.Chain, id *rpc.BlockNumberOrHash) (*chain.Block, error) {
	if id == nil {
		return c.HeadBlock(), nil
	}
	return blockFromChain(c, *id)
}

func marshalBlock(b *chain.Block, fullTx bool) (map[string]any, error) {
	if b == nil {
		return nil, nil
	}
	h := b.Header
	m := map[string]any{
		"hash":             b.Hash(),
		"parentHash":       h.ParentHash,
		"sha3Uncles":       h.UncleHash,
		"miner":            h.Coinbase,
		"stateRoot":        h.Root,
		"transactionsRoot": h.TxHash,
		"receiptsRoot":     h.ReceiptHash,
		"logsBloom":        h.Bloom,
		"difficulty":       (*hexutil.Big)(h.Difficulty),
		"number":           (*hexutil.Big)(h.Number),
		"gasLimit":         hexutil.Uint64(h.GasLimit),
		"gasUsed":          hexutil.Uint64(h.GasUsed),
		"timestamp":        hexutil.Uint64(h.Time),
		"extraData":        hexutil.Bytes(h.Extra),
		"mixHash":          h.MixDigest,
		"nonce":            h.Nonce,
		"uncles":           []common.Hash{},
		"withdrawals":      []*types.Withdrawal{},
	}
	if h.BaseFee != nil {
		m["baseFeePerGas"] = (*hexutil.Big)(h.BaseFee)
	}
	if h.WithdrawalsHash != nil {
		m["withdrawalsRoot"] = *h.WithdrawalsHash
	}
	if h.BlobGasUsed != nil {
		m["blobGasUsed"] = hexutil.Uint64(*h.BlobGasUsed)
	}
	if h.ExcessBlobGas != nil {
		m["excessBlobGas"] = hexutil.Uint64(*h.ExcessBlobGas)
	}
	if h.ParentBeaconRoot != nil {
		m["parentBeaconBlockRoot"] = *h.ParentBeaconRoot
	}
	// requestsHash is part of the header from Isthmus onward, so it is part of
	// the block hash. Omitting it makes every client that reconstructs a block
	// from this JSON — op-batcher does, to chain blocks together — compute a
	// different hash and reject the chain.
	if h.RequestsHash != nil {
		m["requestsHash"] = *h.RequestsHash
	}
	txs := make([]any, 0, len(b.Txs))
	for i, raw := range b.Txs {
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(raw); err != nil {
			return nil, fmt.Errorf("block %s tx %d does not decode: %w", b.Hash(), i, err)
		}
		if fullTx {
			encoded, err := tx.MarshalJSON()
			if err != nil {
				return nil, err
			}
			txs = append(txs, json.RawMessage(encoded))
		} else {
			txs = append(txs, tx.Hash())
		}
	}
	m["transactions"] = txs
	return m, nil
}
