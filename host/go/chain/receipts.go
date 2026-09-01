package chain

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// OutputsEmitterAddress is the address synthesized logs are attributed to. The
// machine has no contract addresses, so outputs are reported as coming from a
// single system address rather than from an invented per-application one.
//
// It is not consensus-critical: the header commits an empty receipts root, so
// nothing about the log encoding is fixed by consensus yet.
var OutputsEmitterAddress = common.HexToAddress("0x4200000000000000000000000000000000000cA1")

// OutputEventTopic is topics[0] of a synthesized output log. It is the hash of
// the event signature a Solidity emitter would have used.
var OutputEventTopic = crypto.Keccak256Hash([]byte("CartesiOutput(uint64,bytes)"))

// Receipt is the synthesized receipt for one transaction.
//
// The machine produces no EVM receipts, so these are built from what it did
// emit: provable outputs become logs, acceptance becomes status, and consumed
// mcycles become gas. Nothing here is committed to — the header's receipts
// root and bloom stay empty — because the encoding is still expected to
// change, and committing it would make every future adjustment a hard fork.
type Receipt struct {
	TxHash            common.Hash
	TxIndex           uint
	BlockHash         common.Hash
	BlockNumber       uint64
	From              common.Address
	To                *common.Address
	Type              uint8
	Status            uint64
	GasUsed           uint64
	CumulativeGasUsed uint64
	EffectiveGasPrice *big.Int
	Logs              []*types.Log
	Bloom             types.Bloom
}

// buildReceipts synthesizes the receipts for a whole block. It works per block
// rather than per transaction because cumulative gas and log indices are
// block-scoped, and firstOutputIndex anchors the logs to the chain-wide output
// numbering that on-chain proofs use.
func buildReceipts(b *Block, txOutputs []TxOutputs, firstOutputIndex uint64, cfg Config) []*Receipt {
	receipts := make([]*Receipt, 0, len(txOutputs))
	var cumulativeGas uint64
	var logIndex uint
	outputIndex := firstOutputIndex

	for _, txo := range txOutputs {
		gasUsed := txo.Cycles / CyclesPerGas
		cumulativeGas += gasUsed

		receipt := &Receipt{
			TxHash:            txo.TxHash,
			TxIndex:           uint(txo.TxIndex),
			BlockHash:         b.Hash(),
			BlockNumber:       b.NumberU64(),
			Status:            types.ReceiptStatusFailed,
			GasUsed:           gasUsed,
			CumulativeGasUsed: cumulativeGas,
			EffectiveGasPrice: cfg.BaseFee,
		}
		if txo.Accepted {
			receipt.Status = types.ReceiptStatusSuccessful
		}
		if txo.TxIndex < len(b.Txs) {
			if tx := new(types.Transaction); tx.UnmarshalBinary(b.Txs[txo.TxIndex]) == nil {
				receipt.Type = tx.Type()
				receipt.To = tx.To()
				// The signer is built from the chain's own id: a deposit
				// transaction reports chain id 0, which the signer rejects.
				signer := types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.ChainID))
				if from, err := types.Sender(signer, tx); err == nil {
					receipt.From = from
				}
			}
		}
		for _, output := range txo.Outputs {
			log := &types.Log{
				Address: OutputsEmitterAddress,
				// The output's chain-wide index is a topic so that a client
				// reading this receipt has everything an on-chain output proof
				// needs: the index, and the raw bytes in Data.
				Topics: []common.Hash{
					OutputEventTopic,
					common.BigToHash(new(big.Int).SetUint64(outputIndex)),
				},
				Data:        output,
				BlockNumber: b.NumberU64(),
				TxHash:      txo.TxHash,
				TxIndex:     uint(txo.TxIndex),
				BlockHash:   b.Hash(),
				Index:       logIndex,
			}
			// A guest event decodes into a real log — the guest's own
			// emitter, topics and data — which is what event-matching
			// tooling (viem's getWithdrawals over MessagePassed, indexers
			// reading Transfer) expects. Other outputs keep the raw
			// CartesiOutput form; either way the output's chain-wide index
			// stays available via cartesi_getTransactionEmissions, and
			// nothing here is consensus — the receipts root is uncommitted.
			if evmLog, ok := ParseEvmLogOutput(output); ok {
				log.Address = evmLog.Emitter
				log.Topics = evmLog.Topics
				log.Data = evmLog.Data
			}
			receipt.Logs = append(receipt.Logs, log)
			outputIndex++
			logIndex++
		}
		receipt.Bloom = types.CreateBloom(&types.Receipt{Logs: receipt.Logs})
		receipts = append(receipts, receipt)
	}
	return receipts
}

// ReceiptByTxHash returns the synthesized receipt for a transaction in the
// canonical chain, or nil if the transaction is unknown or was reorged out.
func (c *Chain) ReceiptByTxHash(hash common.Hash) *Receipt {
	c.mu.RLock()
	defer c.mu.RUnlock()

	blockHash, ok := c.canonicalBlockOf(hash)
	if !ok {
		return nil
	}
	for _, r := range c.receiptsIn(blockHash) {
		if r.TxHash == hash {
			return r
		}
	}
	return nil
}

// BlockReceipts returns the synthesized receipts for a block.
func (c *Chain) BlockReceipts(blockHash common.Hash) []*Receipt {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.receiptsIn(blockHash)
}

// receiptsIn builds a block's receipts on demand. Callers hold c.mu.
func (c *Chain) receiptsIn(blockHash common.Hash) []*Receipt {
	b, ok := c.blocks[blockHash]
	if !ok {
		return nil
	}
	tree, ok := c.trees[blockHash]
	if !ok {
		return nil
	}
	outputs := c.outputs[blockHash]
	// The tree at this block already includes the block's own outputs, so the
	// first output index of the block is the count before they were appended.
	firstOutputIndex := tree.Count() - uint64(countOutputs(outputs))
	return buildReceipts(b, outputs, firstOutputIndex, c.cfg)
}

func countOutputs(txs []TxOutputs) int {
	n := 0
	for _, tx := range txs {
		n += len(tx.Outputs)
	}
	return n
}

// canonicalBlockOf finds the canonical block containing a transaction. A
// transaction can appear in several blocks across reorgs, so the index keeps
// every occurrence and canonicality is resolved at lookup time rather than
// maintained on every reorg. Callers hold c.mu.
func (c *Chain) canonicalBlockOf(txHash common.Hash) (common.Hash, bool) {
	for _, blockHash := range c.txBlocks[txHash] {
		b, ok := c.blocks[blockHash]
		if !ok {
			continue
		}
		if c.canonical[b.NumberU64()] == blockHash {
			return blockHash, true
		}
	}
	return common.Hash{}, false
}
