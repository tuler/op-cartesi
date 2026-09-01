package chain

import (
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// EvmLogSignature is the routed guest's event encoding (EVM-COMPAT §8): a
// Notice whose payload is EvmLog(address emitter, bytes32[] topics, bytes
// data). Receipt synthesis decodes it into a real log with the guest's own
// emitter and topics, which is what makes standard tooling — viem's
// getWithdrawals over MessagePassed, event-driven indexers — read this
// chain's receipts like any other's.
const EvmLogSignature = "EvmLog(address,bytes32[],bytes)"

var (
	evmLogSelector = crypto.Keccak256([]byte(EvmLogSignature))[:4]
	evmLogArgs     abi.Arguments
)

func init() {
	addressT, err := abi.NewType("address", "", nil)
	if err != nil {
		panic(err)
	}
	topicsT, err := abi.NewType("bytes32[]", "", nil)
	if err != nil {
		panic(err)
	}
	bytesT, err := abi.NewType("bytes", "", nil)
	if err != nil {
		panic(err)
	}
	evmLogArgs = abi.Arguments{{Type: addressT}, {Type: topicsT}, {Type: bytesT}}
}

// EvmLog is one decoded guest event.
type EvmLog struct {
	Emitter common.Address
	Topics  []common.Hash
	Data    []byte
}

// ParseEvmLogOutput recognizes a guest event in a raw machine output: a
// Notice wrapping an EvmLog call encoding. Anything else returns false and
// stays in the raw CartesiOutput receipt form.
func ParseEvmLogOutput(output []byte) (*EvmLog, bool) {
	payload, ok := unwrap(output, noticeSelector, noticeArgs)
	if !ok {
		return nil, false
	}
	fields, ok := unwrapAll(payload, evmLogSelector, evmLogArgs)
	if !ok || len(fields) != 3 {
		return nil, false
	}
	log := &EvmLog{}
	if log.Emitter, ok = fields[0].(common.Address); !ok {
		return nil, false
	}
	raw, ok := fields[1].([][32]byte)
	if !ok {
		return nil, false
	}
	for _, topic := range raw {
		log.Topics = append(log.Topics, common.Hash(topic))
	}
	if log.Data, ok = fields[2].([]byte); !ok {
		return nil, false
	}
	return log, true
}
