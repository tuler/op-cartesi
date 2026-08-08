package chain

import (
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// EvmCall is the inspect envelope of the routed guest standard
// (docs/EVM-COMPAT.md §7): an eth_call reaches the guest as
//
//	EvmCall(uint256 chainId, address from, address to, uint256 value, bytes data)
//
// and the guest answers with tagged reports — one framing byte before each
// report so the host can tell return data and revert data from application
// diagnostics. cartesi_inspect remains the raw path for guests (and queries)
// that speak their own dialect.
const EvmCallSignature = "EvmCall(uint256,address,address,uint256,bytes)"

// The report framing tags (EVM-COMPAT §7, mirrored by app/).
const (
	ReportTagApp    byte = 0x00 // application diagnostic, passed through
	ReportTagReturn byte = 0x01 // eth_call return data
	ReportTagRevert byte = 0x02 // revert data (Error(string) where the guest produces it)
	// ReportTagFail marks a handler failure that KEPT its state changes
	// (EVM-COMPAT §5's FAIL). It carries the same Error(string)-shaped data
	// as a revert, but means the opposite to whoever reads it: a revert is
	// safe to treat as "nothing happened", a fail is not.
	ReportTagFail byte = 0x03
)

// IsErrorTag reports whether a report tag carries handler error data, either
// flavor. Callers that only need the message treat them alike; callers that
// decide whether state moved must not.
func IsErrorTag(tag byte) bool {
	return tag == ReportTagRevert || tag == ReportTagFail
}

var (
	evmCallOnce     sync.Once
	evmCallArgs     abi.Arguments
	evmCallSelector []byte
)

func evmCallABI() (abi.Arguments, []byte) {
	evmCallOnce.Do(func() {
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
		evmCallArgs = abi.Arguments{
			{Type: uint256Ty}, {Type: addressTy}, {Type: addressTy},
			{Type: uint256Ty}, {Type: bytesTy},
		}
		evmCallSelector = crypto.Keccak256([]byte(EvmCallSignature))[:4]
	})
	return evmCallArgs, evmCallSelector
}

// EncodeEvmCall wraps one eth_call for the guest's inspect entry.
func EncodeEvmCall(chainID uint64, from, to common.Address, value *big.Int, data []byte) ([]byte, error) {
	args, selector := evmCallABI()
	if value == nil {
		value = new(big.Int)
	}
	packed, err := args.Pack(new(big.Int).SetUint64(chainID), from, to, value, data)
	if err != nil {
		return nil, fmt.Errorf("encoding EvmCall: %w", err)
	}
	return append(append([]byte{}, selector...), packed...), nil
}
