package chain

import (
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Inputs are handed to the machine wrapped in Cartesi's EvmAdvance envelope:
//
//	EvmAdvance(uint256 chainId, address appContract, address msgSender,
//	           uint256 blockNumber, uint256 blockTimestamp, uint256 prevRandao,
//	           uint256 index, bytes payload)
//
// This is the encoding Cartesi's guest tools already decode, so a stock rootfs
// and existing Cartesi applications run unmodified — the same reuse argument
// that picked the OP Stack for everything else. The raw transaction travels as
// the payload, so blocks stay ordinary RLP-parseable Ethereum transactions for
// op-batcher and op-node; only what the guest sees is wrapped.
//
// The envelope also carries the L2 block context the guest would otherwise
// have no way to learn, since the machine has no clock and no view of the
// chain: which block it is in, when, and with what randomness.
var (
	evmAdvanceOnce     sync.Once
	evmAdvanceArgs     abi.Arguments
	evmAdvanceSelector []byte
)

// EvmAdvanceSignature is the canonical signature the selector is derived from.
const EvmAdvanceSignature = "EvmAdvance(uint256,address,address,uint256,uint256,uint256,uint256,bytes)"

func evmAdvanceABI() (abi.Arguments, []byte) {
	evmAdvanceOnce.Do(func() {
		uint256Ty, err := abi.NewType("uint256", "", nil)
		if err != nil {
			panic(err)
		}
		addressTy, err := abi.NewType("address", "", nil)
		if err != nil {
			panic(err)
		}
		bytesTy, err := abi.NewType("bytes", "", nil)
		if err != nil {
			panic(err)
		}
		evmAdvanceArgs = abi.Arguments{
			{Type: uint256Ty}, {Type: addressTy}, {Type: addressTy},
			{Type: uint256Ty}, {Type: uint256Ty}, {Type: uint256Ty},
			{Type: uint256Ty}, {Type: bytesTy},
		}
		evmAdvanceSelector = crypto.Keccak256([]byte(EvmAdvanceSignature))[:4]
	})
	return evmAdvanceArgs, evmAdvanceSelector
}

// inputContext is the per-block half of the envelope. Every field is known
// before the block executes and every field ends up in the header, so a
// verifier re-executing the block derives exactly the same context the builder
// used — which is what keeps the two agreeing on the state root.
type inputContext struct {
	ChainID     uint64
	AppContract common.Address
	BlockNumber uint64
	Timestamp   uint64
	PrevRandao  common.Hash
	// FirstIndex is the chain-wide index of this block's first input. Indices
	// are chain-wide rather than per-block because the chain is one
	// application: the guest sees a single, gapless input sequence, exactly as
	// it would reading from an InputBox.
	FirstIndex uint64
}

// inputContextFor builds the context for the block being appended to parent.
func (c Config) inputContextFor(parent *types.Header, timestamp uint64, prevRandao common.Hash, firstIndex uint64) inputContext {
	return inputContext{
		ChainID:     c.ChainID,
		AppContract: c.AppContract,
		BlockNumber: parent.Number.Uint64() + 1,
		Timestamp:   timestamp,
		PrevRandao:  prevRandao,
		FirstIndex:  firstIndex,
	}
}

// inputContextOf recovers the context from a block header, so the import path
// reconstructs what the builder used without being told.
func (c Config) inputContextOf(header *types.Header, firstIndex uint64) inputContext {
	return inputContext{
		ChainID:     c.ChainID,
		AppContract: c.AppContract,
		BlockNumber: header.Number.Uint64(),
		Timestamp:   header.Time,
		PrevRandao:  header.MixDigest,
		FirstIndex:  firstIndex,
	}
}

// encodeInput wraps one transaction for the machine. offset is the
// transaction's position within the block, which added to the context's
// FirstIndex gives the chain-wide input index.
func (ic inputContext) encodeInput(offset int, rawTx []byte) ([]byte, error) {
	args, selector := evmAdvanceABI()
	packed, err := args.Pack(
		new(big.Int).SetUint64(ic.ChainID),
		ic.AppContract,
		ic.msgSenderOf(rawTx),
		new(big.Int).SetUint64(ic.BlockNumber),
		new(big.Int).SetUint64(ic.Timestamp),
		ic.PrevRandao.Big(),
		new(big.Int).SetUint64(ic.FirstIndex+uint64(offset)),
		rawTx,
	)
	if err != nil {
		return nil, fmt.Errorf("encoding EvmAdvance: %w", err)
	}
	return append(append([]byte{}, selector...), packed...), nil
}

// SystemSenderAddress stands in for the sender of a transaction whose signer
// cannot be recovered. It should not occur for well-formed blocks; using a
// fixed address rather than the zero address keeps such an input
// distinguishable from one genuinely sent by address zero.
var SystemSenderAddress = common.HexToAddress("0x4200000000000000000000000000000000000cA0")

// msgSenderOf is the envelope's msgSender: the transaction's own sender.
//
// Cartesi Rollups puts the relaying portal contract here. This chain has no
// portal between the transaction and the machine — op-node hands deposits
// straight to the engine — so the sender itself is both more informative and
// uniform across deposits and user transactions. For an OP deposit that is the
// (aliased) L1 originator carried in the transaction; for a user transaction
// it is the recovered signer.
func (ic inputContext) msgSenderOf(rawTx []byte) common.Address {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return SystemSenderAddress
	}
	// The signer is built from the chain's id rather than the transaction's: a
	// deposit reports chain id 0, which the signer rejects. Deposits short-
	// circuit inside Sender anyway, returning the From the transaction carries.
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(ic.ChainID))
	from, err := types.Sender(signer, tx)
	if err != nil {
		return SystemSenderAddress
	}
	return from
}
