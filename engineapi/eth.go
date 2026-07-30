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
	var b *chain.Block
	switch number {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		b = e.chain.HeadBlock()
	case rpc.SafeBlockNumber:
		b = e.chain.SafeBlock()
	case rpc.FinalizedBlockNumber:
		b = e.chain.FinalizedBlock()
	case rpc.EarliestBlockNumber:
		b = e.chain.BlockByNumber(0)
	default:
		if number < 0 {
			return nil, fmt.Errorf("unsupported block number %d", number)
		}
		b = e.chain.BlockByNumber(uint64(number))
	}
	return marshalBlock(b, fullTx)
}

func (e *EthAPI) SendRawTransaction(_ context.Context, raw hexutil.Bytes) (common.Hash, error) {
	if e.pool == nil {
		return common.Hash{}, fmt.Errorf("transaction submission disabled on this node")
	}
	return e.pool.Add(raw)
}

// GetTransactionReceipt returns null for now: the machine does not produce
// EVM receipts, and none of the OP services on the critical path require
// them. Synthetic receipts are future work.
func (e *EthAPI) GetTransactionReceipt(_ context.Context, _ common.Hash) (map[string]any, error) {
	return nil, nil
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
