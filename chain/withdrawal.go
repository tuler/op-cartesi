package chain

import (
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// A Withdrawal is an OP Stack L2→L1 message: the exact WithdrawalTransaction
// struct OptimismPortal.proveWithdrawalTransaction takes, produced by the
// guest instead of by an L2ToL1MessagePasser predeploy this chain does not
// have.
//
// The guest emits it as a Notice whose payload is the ABI call encoding
// Withdrawal(uint256,address,address,uint256,uint256,bytes) — a notice
// because the rollup device offers no raw output, and deliberately not a
// voucher: a voucher is executable by the Cartesi output executor, and a
// message that is finalizable through OptimismPortal must not have a second
// executor to be paid by. As a notice it still enters the Cartesi outputs
// tree like every provable output, so it can be *proven* both ways, but only
// the portal can *execute* it.
type Withdrawal struct {
	// Nonce disambiguates otherwise-identical withdrawals; the portal's
	// replay protection is per withdrawal hash, so two identical messages
	// with the same nonce would be finalizable only once. The guest derives
	// it from the chain-wide input index rather than a stored counter (see
	// the bridge handler), which makes it unique without guest state; OP's
	// version tag rides in the top 16 bits as encodeVersionedNonce does.
	Nonce    *big.Int
	Sender   common.Address
	Target   common.Address
	Value    *big.Int
	GasLimit *big.Int
	Data     []byte
}

// WithdrawalSignature is the ABI signature the guest wraps a withdrawal in.
const WithdrawalSignature = "Withdrawal(uint256,address,address,uint256,uint256,bytes)"

// noticeSignature is Cartesi's Notice output, from rollups-contracts Outputs.sol.
const noticeSignature = "Notice(bytes)"

var (
	withdrawalSelector = crypto.Keccak256([]byte(WithdrawalSignature))[:4]
	noticeSelector     = crypto.Keccak256([]byte(noticeSignature))[:4]

	withdrawalArgs abi.Arguments
	noticeArgs     abi.Arguments
)

func init() {
	uint256T, err := abi.NewType("uint256", "", nil)
	if err != nil {
		panic(err)
	}
	addressT, err := abi.NewType("address", "", nil)
	if err != nil {
		panic(err)
	}
	bytesT, err := abi.NewType("bytes", "", nil)
	if err != nil {
		panic(err)
	}
	withdrawalArgs = abi.Arguments{
		{Type: uint256T}, {Type: addressT}, {Type: addressT},
		{Type: uint256T}, {Type: uint256T}, {Type: bytesT},
	}
	noticeArgs = abi.Arguments{{Type: bytesT}}
}

// Hash is Hashing.hashWithdrawal: keccak256 of the ABI encoding of the
// WithdrawalTransaction fields. It is the key of OptimismPortal's
// provenWithdrawals and finalizedWithdrawals maps, and the value hashed into
// the message passer storage slot the portal's inclusion proof opens.
func (w *Withdrawal) Hash() (common.Hash, error) {
	packed, err := withdrawalArgs.Pack(w.Nonce, w.Sender, w.Target, w.Value, w.GasLimit, w.Data)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(packed), nil
}

// EncodeOutput builds the wire form the guest emits for this withdrawal: a
// Notice wrapping the Withdrawal call encoding. It is the inverse of
// ParseWithdrawalOutput, and exists so tests and tooling produce exactly what
// the guest does.
func (w *Withdrawal) EncodeOutput() ([]byte, error) {
	inner, err := withdrawalArgs.Pack(w.Nonce, w.Sender, w.Target, w.Value, w.GasLimit, w.Data)
	if err != nil {
		return nil, err
	}
	payload := append(append([]byte{}, withdrawalSelector...), inner...)
	outer, err := noticeArgs.Pack(payload)
	if err != nil {
		return nil, err
	}
	return append(append([]byte{}, noticeSelector...), outer...), nil
}

// ParseWithdrawalOutput recognizes a withdrawal in a raw machine output: a
// Notice wrapping a Withdrawal call encoding. Anything else — vouchers,
// ordinary notices, malformed bytes — returns false and is not a withdrawal.
//
// The decode is strict on shape but permissive on provenance: any accepted
// input whose output takes this form produces a withdrawal. That is the same
// trust model as the message passer predeploy, where anyone may call
// initiateWithdrawal; what a withdrawal *pays* is decided on L1 by what the
// portal holds and whom the target trusts, and on L2 by the guest debiting
// the sender before emitting.
func ParseWithdrawalOutput(output []byte) (*Withdrawal, bool) {
	payload, ok := unwrap(output, noticeSelector, noticeArgs)
	if !ok {
		return nil, false
	}
	fields, ok := unwrapAll(payload, withdrawalSelector, withdrawalArgs)
	if !ok || len(fields) != 6 {
		return nil, false
	}
	w := &Withdrawal{}
	if w.Nonce, ok = fields[0].(*big.Int); !ok {
		return nil, false
	}
	if w.Sender, ok = fields[1].(common.Address); !ok {
		return nil, false
	}
	if w.Target, ok = fields[2].(common.Address); !ok {
		return nil, false
	}
	if w.Value, ok = fields[3].(*big.Int); !ok {
		return nil, false
	}
	if w.GasLimit, ok = fields[4].(*big.Int); !ok {
		return nil, false
	}
	if w.Data, ok = fields[5].([]byte); !ok {
		return nil, false
	}
	return w, true
}

// unwrap decodes a single-bytes-argument ABI call, returning its payload.
func unwrap(data []byte, selector []byte, args abi.Arguments) ([]byte, bool) {
	fields, ok := unwrapAll(data, selector, args)
	if !ok || len(fields) != 1 {
		return nil, false
	}
	payload, ok := fields[0].([]byte)
	return payload, ok
}

// unwrapAll checks the selector and ABI-decodes the arguments after it.
func unwrapAll(data []byte, selector []byte, args abi.Arguments) ([]interface{}, bool) {
	if len(data) < 4 || string(data[:4]) != string(selector) {
		return nil, false
	}
	fields, err := args.Unpack(data[4:])
	if err != nil {
		return nil, false
	}
	return fields, true
}

// withdrawalHashes extracts the withdrawal hashes from a block's recorded
// outputs, in emission order. Outputs that are not withdrawals contribute
// nothing; a withdrawal that decodes but does not hash (which Pack cannot
// actually produce for decoded fields) is skipped rather than aborting the
// block, because by this point the output is already committed machine state.
func withdrawalHashes(txs []TxOutputs) []common.Hash {
	var hashes []common.Hash
	for _, tx := range txs {
		for _, output := range tx.Outputs {
			w, ok := ParseWithdrawalOutput(output)
			if !ok {
				continue
			}
			h, err := w.Hash()
			if err != nil {
				continue
			}
			hashes = append(hashes, h)
		}
	}
	return hashes
}
