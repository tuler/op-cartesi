package engineapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/host/go/chain"
)

// AccountResult is eth_getProof's reply, in geth's exact wire shape so
// clients that were written against geth parse it unchanged.
type AccountResult struct {
	Address      common.Address  `json:"address"`
	AccountProof []hexutil.Bytes `json:"accountProof"`
	Balance      *hexutil.Big    `json:"balance"`
	CodeHash     common.Hash     `json:"codeHash"`
	Nonce        hexutil.Uint64  `json:"nonce"`
	StorageHash  common.Hash     `json:"storageHash"`
	StorageProof []StorageResult `json:"storageProof"`
}

// StorageResult is one storage slot's proof within an AccountResult.
type StorageResult struct {
	Key   string          `json:"key"`
	Value *hexutil.Big    `json:"value"`
	Proof []hexutil.Bytes `json:"proof"`
}

// GetProof serves storage proofs for the L2ToL1MessagePasser address — and
// only that address, because it is the only account this chain maintains a
// storage trie for. The trie is the block's withdrawal commitment: its root
// is the header's withdrawalsRoot, which op-node publishes as the L2 output
// root's messagePasserStorageRoot, so a storage proof from here is exactly
// what OptimismPortal.proveWithdrawalTransaction verifies. This is the call
// viem's buildProveWithdrawal makes.
//
// The account proof is empty: there is no Ethereum account trie on this
// chain, so the account itself cannot be proven against the state root —
// which nothing on the Isthmus withdrawal path needs. Clients must read
// storageHash and storageProof, as viem and the portal do, and not attempt
// to verify the account against stateRoot. For every other address,
// cartesi_getAccountProof proves the account record against the machine's
// state root.
func (e *EthAPI) GetProof(_ context.Context, addr common.Address, storageKeys []string, id rpc.BlockNumberOrHash) (*AccountResult, error) {
	if addr != chain.MessagePasserAddress {
		return nil, fmt.Errorf("eth_getProof serves only the message passer %s; this chain has no Ethereum state trie — use cartesi_getAccountProof for accounts", chain.MessagePasserAddress)
	}
	b, err := blockFromChain(e.chain, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	slots := make([]common.Hash, 0, len(storageKeys))
	for _, key := range storageKeys {
		slot, err := parseStorageKey(key)
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	proofs, ok, err := e.chain.PasserProofAt(b.Hash(), slots)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no withdrawal commitment for block %s", b.Hash())
	}
	results := make([]StorageResult, 0, len(proofs))
	for i, p := range proofs {
		nodes := make([]hexutil.Bytes, 0, len(p.Proof))
		for _, node := range p.Proof {
			nodes = append(nodes, node)
		}
		results = append(results, StorageResult{
			Key:   storageKeys[i],
			Value: (*hexutil.Big)(new(big.Int).SetBytes(p.Value)),
			Proof: nodes,
		})
	}
	return &AccountResult{
		Address:      addr,
		AccountProof: []hexutil.Bytes{},
		Balance:      (*hexutil.Big)(new(big.Int)),
		CodeHash:     crypto.Keccak256Hash(nil),
		Nonce:        0,
		StorageHash:  *b.Header.WithdrawalsHash,
		StorageProof: results,
	}, nil
}

// parseStorageKey turns a client's hex storage key into a 32-byte slot,
// left-padded the way geth pads short keys.
func parseStorageKey(key string) (common.Hash, error) {
	s := strings.TrimPrefix(key, "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return common.Hash{}, fmt.Errorf("invalid storage key %q: %w", key, err)
	}
	if len(raw) > common.HashLength {
		return common.Hash{}, fmt.Errorf("storage key %q is longer than 32 bytes", key)
	}
	return common.BytesToHash(raw), nil
}
