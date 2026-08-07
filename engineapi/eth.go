package engineapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
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

// CallArgs is the subset of the eth_call / eth_estimateGas argument object
// this chain can act on: the caller, the target, the value and the calldata
// all travel to the guest inside the EvmCall envelope. Gas fields are
// accepted and ignored so that standard tooling can call without
// special-casing — there is no fee market to price a read.
type CallArgs struct {
	From  *common.Address `json:"from"`
	To    *common.Address `json:"to"`
	Value *hexutil.Big    `json:"value"`
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

// revertError is the standard Ethereum JSON-RPC shape for a reverted call:
// code 3, "execution reverted" (with the decoded Error(string) reason when
// there is one), and the raw revert bytes in the data field — which is what
// lets viem, ethers and cast surface require-style messages verbatim.
type revertError struct {
	reason string
	data   hexutil.Bytes
}

func (e *revertError) Error() string  { return e.reason }
func (e *revertError) ErrorCode() int { return 3 }
func (e *revertError) ErrorData() any { return e.data }

// errorStringSelector is keccak("Error(string)")[:4], the require-style
// revert payload.
var errorStringSelector = []byte{0x08, 0xc3, 0x79, 0xa0}

func newRevertError(data []byte) *revertError {
	reason := "execution reverted"
	if len(data) > 4 && bytes.Equal(data[:4], errorStringSelector) {
		stringTy, err := abi.NewType("string", "", nil)
		if err == nil {
			if vals, err := (abi.Arguments{{Type: stringTy}}).Unpack(data[4:]); err == nil && len(vals) == 1 {
				if s, ok := vals[0].(string); ok {
					reason = "execution reverted: " + s
				}
			}
		}
	}
	return &revertError{reason: reason, data: data}
}

// Call answers a read-only query by running the machine's inspect protocol
// against the state at the requested block, on a fork that is then discarded.
//
// This is the shim half of EVM-COMPAT §7: the call travels to the guest as
// the EvmCall envelope — EvmCall(chainId, from, to, value, data) — and the
// guest answers with tagged reports. An accepted inspect's return-tagged
// report is eth_call's return value; a rejected inspect's revert-tagged
// report becomes the standard revert error, code 3 with the revert bytes in
// the data field. Application dialects that predate the envelope keep the
// raw path: cartesi_inspect passes payloads through untouched, both ways.
func (e *EthAPI) Call(ctx context.Context, args CallArgs, id *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	b, err := e.blockFromOptional(id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	if args.To == nil {
		return nil, fmt.Errorf("eth_call requires 'to': there is no contract creation to simulate")
	}
	var from common.Address
	if args.From != nil {
		from = *args.From
	}
	envelope, err := chain.EncodeEvmCall(e.chain.Config().ChainID, from, *args.To, (*big.Int)(args.Value), args.payload())
	if err != nil {
		return nil, err
	}
	res, err := e.chain.Inspect(ctx, b.Hash(), envelope)
	if err != nil {
		return nil, err
	}
	if !res.Accepted {
		// The error data rides the last error-tagged report. A simulation
		// that FAILs rather than REVERTs still reverts the eth_call — the
		// answer to "would this succeed" is no either way — but it is tagged
		// apart on the wire so a caller that cares can tell them apart.
		for i := len(res.Reports) - 1; i >= 0; i-- {
			if r := res.Reports[i]; len(r) >= 1 && chain.IsErrorTag(r[0]) {
				return nil, newRevertError(r[1:])
			}
		}
		return nil, fmt.Errorf("inspect rejected the query")
	}
	// Return data is the return-tagged report (the guest emits exactly one;
	// concatenation degenerates to it). App-tagged diagnostics are not
	// eth_call's to return.
	var out []byte
	for _, report := range res.Reports {
		if len(report) >= 1 && report[0] == chain.ReportTagReturn {
			out = append(out, report[1:]...)
		}
	}
	return out, nil
}

// GetBalance serves the account's native balance out of the guest's accounts
// drive: resolve the block tag to a parked machine, read the record straight
// from machine memory — no fork, no execution (docs/ACCOUNTS.md §6.2).
func (e *EthAPI) GetBalance(ctx context.Context, addr common.Address, id *rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	_, balance, err := e.accountAt(ctx, addr, id)
	if err != nil {
		return nil, err
	}
	return (*hexutil.Big)(balance), nil
}

// GetTransactionCount serves the account's nonce from the same record. Since
// ACCOUNTS.md roadmap v1 the guest enforces and bumps this nonce for every
// ordinary L2 transaction, so what the drive holds is the next nonce a
// wallet must sign with — exactly what this method exists to answer.
func (e *EthAPI) GetTransactionCount(ctx context.Context, addr common.Address, id *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	nonce, _, err := e.accountAt(ctx, addr, id)
	if err != nil {
		return 0, err
	}
	return hexutil.Uint64(nonce), nil
}

// accountAt is the shared block-resolution-plus-drive-read behind the two
// account methods.
func (e *EthAPI) accountAt(ctx context.Context, addr common.Address, id *rpc.BlockNumberOrHash) (uint64, *big.Int, error) {
	b, err := e.blockFromOptional(id)
	if err != nil {
		return 0, nil, err
	}
	if b == nil {
		return 0, nil, fmt.Errorf("unknown block")
	}
	nonce, balance, err := e.chain.AccountAt(ctx, b.Hash(), addr)
	if errors.Is(err, chain.ErrNoAccountsDrive) {
		// A machine without an accounts drive — the in-memory mock, or a guest
		// predating the drive — is answered with zeros rather than an error.
		// An RPC consumer cannot distinguish an empty account from a missing
		// ledger anyway, and erroring would break every wallet pointed at a
		// mock-machine dev node (and the existing tests that run one).
		return 0, new(big.Int), nil
	}
	if err != nil {
		return 0, nil, err
	}
	return nonce, balance, nil
}

// GetCode answers the contract/EOA question for a routed address: a
// four-byte marker (chain.CodeMarker) for every address the guest routes —
// recorded contracts out of the ABI drive, token façades derived from the
// accounts drive's registry — and "0x" for everything else. There is no EVM
// bytecode to serve; the marker (0xEF-prefixed, so no real EVM code can
// collide with it) is what lets wallets and SDKs treat routed contracts as
// contracts before calling them (EVM-COMPAT §10a).
func (e *EthAPI) GetCode(ctx context.Context, addr common.Address, id *rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	b, err := e.blockFromOptional(id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	return e.chain.CodeAt(ctx, b.Hash(), addr)
}

// GasPrice suggests what a transaction should offer per gas: the head block's
// base fee plus a zero tip. There is no fee market — every header carries the
// constant base fee chain.Config.BaseFee stamps on it, and nobody is paid a
// priority fee — so the head's base fee is the honest suggestion, and serving
// it is what lets `cast send` run without a --gas-price flag. The value is
// read from the header rather than the config so the answer always matches
// what the chain actually committed to.
func (e *EthAPI) GasPrice() *hexutil.Big {
	return headerBaseFee(e.chain.HeadBlock().Header)
}

// MaxPriorityFeePerGas suggests a tip of zero, honestly: with no fee market
// nothing charges or pays priority fees, so any nonzero tip would be burned
// for no ordering benefit.
func (e *EthAPI) MaxPriorityFeePerGas() *hexutil.Big {
	return (*hexutil.Big)(new(big.Int))
}

// maxFeeHistoryBlocks caps one eth_feeHistory answer, matching geth's default
// fee-history depth.
const maxFeeHistoryBlocks = 1024

// feeHistoryResult is go-ethereum's eth_feeHistory wire shape, field for
// field — cast and wallets parse it strictly. The blob-fee fields geth may
// append are omitted (they are omitempty there too): this chain accepts no
// blob transactions, so there is no blob fee to describe.
type feeHistoryResult struct {
	OldestBlock  *hexutil.Big     `json:"oldestBlock"`
	Reward       [][]*hexutil.Big `json:"reward,omitempty"`
	BaseFee      []*hexutil.Big   `json:"baseFeePerGas,omitempty"`
	GasUsedRatio []float64        `json:"gasUsedRatio"`
}

// FeeHistory synthesizes the fee history from headers, which hold everything
// there is to know here: the base fee is the constant every header carries,
// gasUsedRatio is the header's metered mcycles over its limit, and priority
// fees do not exist. Shape and clamping mirror geth's oracle — blockCount
// entries ending at lastBlock, clamped to the blocks this node still holds;
// baseFeePerGas carries one extra entry for the block after the window; and
// reward, present only when percentiles are requested, is a matrix of zeros,
// which with no fee market is the true tip distribution rather than a stub.
func (e *EthAPI) FeeHistory(_ context.Context, blockCount math.HexOrDecimal64, lastBlock rpc.BlockNumber, rewardPercentiles []float64) (*feeHistoryResult, error) {
	// Zero blocks requested means no retrievable data, answered with an empty
	// result rather than an error, exactly as geth answers it.
	if blockCount < 1 {
		return &feeHistoryResult{OldestBlock: (*hexutil.Big)(new(big.Int))}, nil
	}
	for i, p := range rewardPercentiles {
		if p < 0 || p > 100 {
			return nil, fmt.Errorf("invalid reward percentile: %f", p)
		}
		if i > 0 && p <= rewardPercentiles[i-1] {
			return nil, fmt.Errorf("invalid reward percentile: #%d:%f >= #%d:%f", i-1, rewardPercentiles[i-1], i, p)
		}
	}
	last, err := e.blockFrom(rpc.BlockNumberOrHashWithNumber(lastBlock))
	if err != nil {
		return nil, err
	}
	if last == nil {
		return nil, fmt.Errorf("request beyond head block: requested %d, head %d", lastBlock, e.chain.HeadBlock().NumberU64())
	}
	lastNum := last.NumberU64()
	count := min(uint64(blockCount), maxFeeHistoryBlocks, lastNum+1)
	oldest := lastNum + 1 - count
	// Clamp to what this node still holds: after a restart only the blocks
	// from the newest checkpoint onward are in memory, so a window reaching
	// below that serves its available suffix, the way geth serves the
	// retrievable part of a range instead of erroring.
	for oldest < lastNum && e.chain.BlockByNumber(oldest) == nil {
		oldest++
	}
	count = lastNum + 1 - oldest

	res := &feeHistoryResult{
		OldestBlock:  (*hexutil.Big)(new(big.Int).SetUint64(oldest)),
		BaseFee:      make([]*hexutil.Big, 0, count+1),
		GasUsedRatio: make([]float64, 0, count),
	}
	for n := oldest; n <= lastNum; n++ {
		b := e.chain.BlockByNumber(n)
		if b == nil {
			return nil, fmt.Errorf("no canonical block at height %d", n)
		}
		res.BaseFee = append(res.BaseFee, headerBaseFee(b.Header))
		res.GasUsedRatio = append(res.GasUsedRatio, gasUsedRatio(b.Header))
		if len(rewardPercentiles) > 0 {
			row := make([]*hexutil.Big, len(rewardPercentiles))
			for j := range row {
				row[j] = (*hexutil.Big)(new(big.Int))
			}
			res.Reward = append(res.Reward, row)
		}
	}
	// The extra baseFeePerGas entry describes the block after the window.
	// When that block already exists its header is served; when the window
	// ends at the head, the next block will be stamped with the configured
	// constant, so that is the truthful projection.
	if next := e.chain.BlockByNumber(lastNum + 1); next != nil {
		res.BaseFee = append(res.BaseFee, headerBaseFee(next.Header))
	} else {
		res.BaseFee = append(res.BaseFee, (*hexutil.Big)(new(big.Int).Set(e.chain.Config().BaseFee)))
	}
	return res, nil
}

func headerBaseFee(h *types.Header) *hexutil.Big {
	if h.BaseFee == nil {
		return (*hexutil.Big)(new(big.Int))
	}
	return (*hexutil.Big)(new(big.Int).Set(h.BaseFee))
}

func gasUsedRatio(h *types.Header) float64 {
	if h.GasLimit == 0 {
		return 0 // avoid NaN, which cannot be JSON-encoded
	}
	return float64(h.GasUsed) / float64(h.GasLimit)
}

// EstimateGas returns the chain's per-input execution budget expressed as
// gas — an upper bound the chain will accept, which is what a gas limit means
// here, rather than a measurement of this particular call.
//
// A true estimate would run the payload on a discarded fork and report its
// cycle count, the way Call runs inspect. But an estimate arrives unsigned —
// CallArgs carry no signature — and since ACCOUNTS.md roadmap v1 the guest
// recovers the sender and enforces the nonce on every ordinary transaction,
// so an unsigned replay cannot follow the enforced path: wrapping the args in
// a synthetic EvmAdvance would be rejected at the signature check and the
// cycles measured would be the rejection's. Until the guest offers a
// signature-less simulation entry point (or estimation folds into the inspect
// protocol), the defensible answer is MaxCyclesPerInput converted at
// CyclesPerGas — with the defaults, 1,000,000 gas. An input never gets more
// than that budget, so a wallet sending the bound can never be cut short.
//
// The bound is clamped to the block gas limit, which tools refuse to exceed,
// and floored at the intrinsic-gas minimum (21000), which tools refuse to go
// below; the chain itself reads neither from the transaction. The args and
// block tag are accepted for wire compatibility; the block is resolved (so an
// unknown tag errors like everywhere else) and the args are ignored.
func (e *EthAPI) EstimateGas(_ context.Context, args CallArgs, id *rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	b, err := e.blockFromOptional(id)
	if err != nil {
		return 0, err
	}
	if b == nil {
		return 0, fmt.Errorf("unknown block")
	}
	bound := e.chain.Config().MaxCyclesPerInput / chain.CyclesPerGas
	if limit := b.Header.GasLimit; bound > limit {
		bound = limit
	}
	if bound < params.TxGas {
		bound = params.TxGas
	}
	return hexutil.Uint64(bound), nil
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
