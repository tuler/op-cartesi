package chain

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
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

// buildHeader assembles the header for a new block. Field choices follow the
// OP Stack post-Ecotone shape: no uncles, empty withdrawals list, zero blob
// gas, parentBeaconRoot from the payload attributes.
func buildHeader(parent *types.Header, attrs *engine.PayloadAttributes, stateRoot common.Hash, txs [][]byte, gasUsed, gasLimit uint64, baseFee *big.Int) *types.Header {
	withdrawalsHash := types.EmptyWithdrawalsHash
	blobGasUsed := uint64(0)
	excessBlobGas := uint64(0)
	beaconRoot := attrs.BeaconRoot
	if beaconRoot == nil {
		beaconRoot = &common.Hash{}
	}
	return &types.Header{
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
		Extra:            []byte{},
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
	return &engine.ExecutableData{
		ParentHash:    h.ParentHash,
		FeeRecipient:  h.Coinbase,
		StateRoot:     h.Root,
		ReceiptsRoot:  h.ReceiptHash,
		LogsBloom:     h.Bloom.Bytes(),
		Random:        h.MixDigest,
		Number:        h.Number.Uint64(),
		GasLimit:      h.GasLimit,
		GasUsed:       h.GasUsed,
		Timestamp:     h.Time,
		ExtraData:     h.Extra,
		BaseFeePerGas: h.BaseFee,
		BlockHash:     h.Hash(),
		Transactions:  txs,
		Withdrawals:   []*types.Withdrawal{},
		BlobGasUsed:   h.BlobGasUsed,
		ExcessBlobGas: h.ExcessBlobGas,
	}
}

// headerFromPayload reconstructs the header committed by an execution payload
// so its hash can be checked against payload.BlockHash. It mirrors
// buildHeader's field choices; a payload that deviates (uncles, withdrawals,
// blob gas) simply fails the block-hash check.
func headerFromPayload(data *engine.ExecutableData, beaconRoot *common.Hash) (*types.Header, error) {
	if len(data.LogsBloom) != types.BloomByteLength {
		return nil, fmt.Errorf("logsBloom has %d bytes, want %d", len(data.LogsBloom), types.BloomByteLength)
	}
	if data.Withdrawals == nil || len(data.Withdrawals) != 0 {
		return nil, fmt.Errorf("withdrawals must be an empty list")
	}
	if data.BaseFeePerGas == nil {
		return nil, fmt.Errorf("missing baseFeePerGas")
	}
	withdrawalsHash := types.EmptyWithdrawalsHash
	if beaconRoot == nil {
		beaconRoot = &common.Hash{}
	}
	return &types.Header{
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
