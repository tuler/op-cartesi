package chain

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

// Block is an L2 block: an Ethereum-shaped header (so stock op-node and
// op-batcher can hash and parse it) whose stateRoot is the Cartesi Machine
// root hash and whose body is the ordered list of raw transactions fed to the
// machine as CMIO inputs.
type Block struct {
	Header *types.Header
	Txs    [][]byte

	hash common.Hash
}

func newBlock(header *types.Header, txs [][]byte) *Block {
	return &Block{Header: header, Txs: txs, hash: header.Hash()}
}

func (b *Block) Hash() common.Hash { return b.hash }
func (b *Block) NumberU64() uint64 { return b.Header.Number.Uint64() }
func (b *Block) Time() uint64      { return b.Header.Time }

// rawTxs lets already-encoded transactions be hashed into a transactions root
// exactly the way op-node does it (DeriveSha over opaque bytes).
type rawTxs [][]byte

func (r rawTxs) Len() int { return len(r) }
func (r rawTxs) EncodeIndex(i int, w *bytes.Buffer) {
	w.Write(r[i])
}

func txsRoot(txs [][]byte) common.Hash {
	if len(txs) == 0 {
		return types.EmptyTxsHash
	}
	return types.DeriveSha(rawTxs(txs), trie.NewStackTrie(nil))
}

// extraData computes the header extraData for a block built from payload
// attributes. Holocene is always active, so op-node is required to supply the
// EIP-1559 parameters, and zeroed parameters mean "use the chain defaults".
// op-geth's own encoder produces the bytes, so they match what an op-geth
// engine would have committed to.
func (c Config) extraData(timestamp uint64, params []byte) ([]byte, error) {
	if err := eip1559.ValidateHolocene1559Params(params); err != nil {
		return nil, err
	}
	denominator, elasticity := eip1559.DecodeHolocene1559Params(params)
	if denominator == 0 {
		denominator, elasticity = c.EIP1559Denominator, c.EIP1559Elasticity
	}
	return c.encodeExtraData(timestamp, denominator, elasticity), nil
}

// genesisExtraData encodes the chain's default EIP-1559 parameters for the
// genesis header, which is built without payload attributes.
func (c Config) genesisExtraData() []byte {
	return c.encodeExtraData(c.GenesisTimestamp, c.EIP1559Denominator, c.EIP1559Elasticity)
}

func (c Config) encodeExtraData(timestamp, denominator, elasticity uint64) []byte {
	return eip1559.EncodeOptimismExtraData(c, timestamp, denominator, elasticity, nil)
}

// buildHeader assembles the header for a new block. Field choices follow the
// OP Stack Isthmus shape: no uncles, empty withdrawals list, zero blob gas,
// parentBeaconRoot from the payload attributes, and the withdrawals root set
// directly to the withdrawal trie's root (passertrie.go — the message passer
// storage trie, which carries the Cartesi outputs root at its reserved slot)
// rather than derived from a withdrawals list. They must match op-node's
// ExecutionPayloadEnvelope.CheckBlockHash exactly, since op-node recomputes
// the block hash from the payload it receives — including the empty requests
// hash it pairs with a directly-set withdrawals root.
//
// The field-by-field rules are docs/BLOCKS-SPEC.md §12.
func buildHeader(parent *types.Header, attrs *engine.PayloadAttributes, stateRoot common.Hash, txs [][]byte, gasUsed, gasLimit uint64, baseFee *big.Int, extra []byte, passerRoot common.Hash) *types.Header {
	withdrawalsHash := passerRoot
	requestsHash := types.EmptyRequestsHash
	blobGasUsed := uint64(0)
	excessBlobGas := uint64(0)
	beaconRoot := attrs.BeaconRoot
	if beaconRoot == nil {
		beaconRoot = &common.Hash{}
	}
	return &types.Header{
		RequestsHash:     &requestsHash,
		ParentHash:       parent.Hash(),
		UncleHash:        types.EmptyUncleHash,
		Coinbase:         attrs.SuggestedFeeRecipient,
		Root:             stateRoot,
		TxHash:           txsRoot(txs),
		ReceiptHash:      types.EmptyReceiptsHash,
		Bloom:            types.Bloom{},
		Difficulty:       new(big.Int),
		Number:           new(big.Int).Add(parent.Number, common.Big1),
		GasLimit:         gasLimit,
		GasUsed:          gasUsed,
		Time:             attrs.Timestamp,
		Extra:            extra,
		MixDigest:        attrs.Random,
		Nonce:            types.BlockNonce{},
		BaseFee:          baseFee,
		WithdrawalsHash:  &withdrawalsHash,
		BlobGasUsed:      &blobGasUsed,
		ExcessBlobGas:    &excessBlobGas,
		ParentBeaconRoot: beaconRoot,
	}
}

func executableData(h *types.Header, txs [][]byte) *engine.ExecutableData {
	// The withdrawals root travels as its own payload field instead of being
	// derived from the (empty) withdrawals list.
	return &engine.ExecutableData{
		WithdrawalsRoot: h.WithdrawalsHash,
		ParentHash:      h.ParentHash,
		FeeRecipient:    h.Coinbase,
		StateRoot:       h.Root,
		ReceiptsRoot:    h.ReceiptHash,
		LogsBloom:       h.Bloom.Bytes(),
		Random:          h.MixDigest,
		Number:          h.Number.Uint64(),
		GasLimit:        h.GasLimit,
		GasUsed:         h.GasUsed,
		Timestamp:       h.Time,
		ExtraData:       h.Extra,
		BaseFeePerGas:   h.BaseFee,
		BlockHash:       h.Hash(),
		Transactions:    txs,
		Withdrawals:     []*types.Withdrawal{},
		BlobGasUsed:     h.BlobGasUsed,
		ExcessBlobGas:   h.ExcessBlobGas,
	}
}

// headerFromPayload reconstructs the header committed by an execution payload
// so its hash can be checked against payload.BlockHash. It mirrors
// buildHeader's field choices; a payload that deviates (uncles, withdrawals,
// blob gas) simply fails the block-hash check.
func (c Config) headerFromPayload(data *engine.ExecutableData, beaconRoot *common.Hash) (*types.Header, error) {
	if len(data.LogsBloom) != types.BloomByteLength {
		return nil, fmt.Errorf("logsBloom has %d bytes, want %d", len(data.LogsBloom), types.BloomByteLength)
	}
	if data.Withdrawals == nil || len(data.Withdrawals) != 0 {
		return nil, fmt.Errorf("withdrawals must be an empty list")
	}
	if data.BaseFeePerGas == nil {
		return nil, fmt.Errorf("missing baseFeePerGas")
	}
	if err := eip1559.ValidateOptimismExtraData(c, data.Timestamp, data.ExtraData); err != nil {
		return nil, fmt.Errorf("invalid extraData: %w", err)
	}
	if data.WithdrawalsRoot == nil {
		return nil, fmt.Errorf("missing withdrawalsRoot")
	}
	withdrawalsHash := *data.WithdrawalsRoot
	requestsHash := types.EmptyRequestsHash
	if beaconRoot == nil {
		beaconRoot = &common.Hash{}
	}
	return &types.Header{
		RequestsHash:     &requestsHash,
		ParentHash:       data.ParentHash,
		UncleHash:        types.EmptyUncleHash,
		Coinbase:         data.FeeRecipient,
		Root:             data.StateRoot,
		TxHash:           txsRoot(data.Transactions),
		ReceiptHash:      data.ReceiptsRoot,
		Bloom:            types.BytesToBloom(data.LogsBloom),
		Difficulty:       new(big.Int),
		Number:           new(big.Int).SetUint64(data.Number),
		GasLimit:         data.GasLimit,
		GasUsed:          data.GasUsed,
		Time:             data.Timestamp,
		Extra:            data.ExtraData,
		MixDigest:        data.Random,
		Nonce:            types.BlockNonce{},
		BaseFee:          data.BaseFeePerGas,
		WithdrawalsHash:  &withdrawalsHash,
		BlobGasUsed:      data.BlobGasUsed,
		ExcessBlobGas:    data.ExcessBlobGas,
		ParentBeaconRoot: beaconRoot,
	}, nil
}
