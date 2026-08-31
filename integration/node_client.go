package integration

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// nodeClient is the non-engine half of the surface: the eth_* subset and the
// cartesi_* namespace, as an ordinary client sees them
// (docs/ENGINE-RPC-SPEC.md §5, §6).
//
// It exists so this suite can ask the node questions without holding its
// internals. The tests used to reach into chain.Chain and mempool.Pool
// directly — the head's height, the outputs accumulator, whether a
// transaction was still queued — which meant they could only ever run against
// an engine built into the same binary. Everything they needed is on the wire
// already, so asking for it over the wire is both a truer test and what lets
// the same suite run against an engine written in another language.
type nodeClient struct {
	rpc *rpc.Client
}

func newNodeClient(c *rpc.Client) *nodeClient {
	return &nodeClient{rpc: c}
}

// block is the subset of eth_getBlockBy* this suite reads.
type block struct {
	Hash       common.Hash    `json:"hash"`
	Number     hexutil.Uint64 `json:"number"`
	Timestamp  hexutil.Uint64 `json:"timestamp"`
	GasLimit   hexutil.Uint64 `json:"gasLimit"`
	ParentHash common.Hash    `json:"parentHash"`
}

func (n *nodeClient) ChainID(ctx context.Context) (uint64, error) {
	var id hexutil.Uint64
	if err := n.rpc.CallContext(ctx, &id, "eth_chainId"); err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// BlockByTag reads a block by number or by one of the tags of §3. A tag no
// block answers to is an error rather than a nil block, since every caller
// here treats a missing block as a broken node.
func (n *nodeClient) BlockByTag(ctx context.Context, tag string) (*block, error) {
	var b *block
	if err := n.rpc.CallContext(ctx, &b, "eth_getBlockByNumber", tag, false); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("no block at %q", tag)
	}
	return b, nil
}

func (n *nodeClient) SendRawTransaction(ctx context.Context, raw []byte) (common.Hash, error) {
	var hash common.Hash
	err := n.rpc.CallContext(ctx, &hash, "eth_sendRawTransaction", hexutil.Bytes(raw))
	return hash, err
}

// transaction is the subset of eth_getTransactionByHash this suite reads. The
// block coordinates are null for a pooled transaction and set for a mined
// one, which is how a client tells the two apart (§5.2).
type transaction struct {
	Hash        common.Hash     `json:"hash"`
	BlockHash   *common.Hash    `json:"blockHash"`
	BlockNumber *hexutil.Uint64 `json:"blockNumber"`
}

// TransactionByHash returns the transaction, or nil when the node knows
// nothing of it.
func (n *nodeClient) TransactionByHash(ctx context.Context, hash common.Hash) (*transaction, error) {
	var tx *transaction
	if err := n.rpc.CallContext(ctx, &tx, "eth_getTransactionByHash", hash); err != nil {
		return nil, err
	}
	return tx, nil
}

// outputsRoot is cartesi_getOutputsRoot's answer: the chain-wide outputs
// commitment as of a block, and how many outputs the tree holds.
type outputsRoot struct {
	BlockHash   common.Hash    `json:"blockHash"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	Root        common.Hash    `json:"root"`
	Count       hexutil.Uint64 `json:"count"`
}

func (n *nodeClient) OutputsRoot(ctx context.Context, blockHash common.Hash) (*outputsRoot, error) {
	var out *outputsRoot
	if err := n.rpc.CallContext(ctx, &out, "cartesi_getOutputsRoot", blockHash); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("no outputs commitment for block %s", blockHash)
	}
	return out, nil
}
