package chain

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// MessagePasserAddress is OP's L2ToL1MessagePasser predeploy address. This
// chain has no such contract — the guest is the message passer — but the
// address is the fixed name under which the rest of the OP Stack looks the
// withdrawal commitment up: viem asks eth_getProof for it, and the header's
// withdrawalsRoot stands in for its storage root under Isthmus.
var MessagePasserAddress = common.HexToAddress("0x4200000000000000000000000000000000000016")

// OutputsRootSlot is the reserved storage slot in the passer trie that holds
// the Cartesi outputs Merkle root. It keeps the outputs tree committed now
// that the header's withdrawalsRoot is the passer trie root rather than the
// outputs root itself: one storage proof against withdrawalsRoot opens the
// outputs root, and Cartesi output validity proofs verify against that as
// before.
//
// The slot is the hash of a name rather than a small integer so it reads as
// what it is, and it cannot collide with a sentMessages slot — those are
// keccak256 of a 64-byte mapping preimage, this is keccak256 of a short
// string, so a collision is a keccak collision.
var OutputsRootSlot = crypto.Keccak256Hash([]byte("op-cartesi.outputsMerkleRoot"))

// passerTrueValue is what L2ToL1MessagePasser stores under a sent message:
// bool true, which as a storage value RLP-encodes to the single byte 0x01.
// OptimismPortal verifies inclusion of exactly this value.
var passerTrueValue = []byte{0x01}

// PasserTrie is the withdrawal commitment: a genuine Ethereum storage trie,
// secure-keyed the way geth keys account storage, holding
//
//	keccak256(abi.encode(withdrawalHash, uint256(0))) → 0x01
//
// for every withdrawal the guest has emitted — the exact slots
// L2ToL1MessagePasser.sentMessages would occupy — plus the Cartesi outputs
// root at OutputsRootSlot. Its root is published as the header's
// withdrawalsRoot, which op-node folds into the L2 output root as
// messagePasserStorageRoot; a storage proof from this trie therefore
// satisfies OptimismPortal.proveWithdrawalTransaction's SecureMerkleTrie
// check unchanged.
//
// Like the outputs tree it is cumulative over the chain: a withdrawal must
// stay provable against the output root of any later proposal. Unlike the
// outputs tree it cannot be carried as a frontier — proofs need the full
// structure — so the whole trie is kept in memory, and rebuilt from stored
// block outputs on restart. Nodes are structurally shared between the copies
// the per-block map holds, so the marginal cost of a block is its own
// insertions.
type PasserTrie struct {
	t *trie.Trie
}

// NewPasserTrie returns a trie holding nothing — not even the outputs root
// slot. Callers seed it with SetOutputsRoot before taking a root, genesis
// included: every block's trie carries the outputs root as of that block.
func NewPasserTrie() *PasserTrie {
	return &PasserTrie{t: trie.NewEmpty(nil)}
}

// Copy returns an independent trie sharing structure with the receiver.
// Divergent updates are safe: the underlying trie copies nodes on write.
func (p *PasserTrie) Copy() *PasserTrie {
	return &PasserTrie{t: p.t.Copy()}
}

// withdrawalSlot is the storage slot sentMessages[withdrawalHash] occupies:
// keccak256 of the mapping key concatenated with the mapping's base slot,
// which is 0 in L2ToL1MessagePasser's layout.
func withdrawalSlot(withdrawalHash common.Hash) common.Hash {
	var base common.Hash
	return crypto.Keccak256Hash(withdrawalHash.Bytes(), base.Bytes())
}

// InsertWithdrawal marks a withdrawal hash sent. Inserting the same hash
// twice writes the same slot to the same value, which mirrors the predeploy:
// sentMessages[hash] = true is idempotent, and the portal's replay protection
// is what makes a duplicated message spendable only once.
func (p *PasserTrie) InsertWithdrawal(withdrawalHash common.Hash) error {
	return p.setSlot(withdrawalSlot(withdrawalHash), passerTrueValue)
}

// SetOutputsRoot records the Cartesi outputs root as of this block.
func (p *PasserTrie) SetOutputsRoot(root common.Hash) error {
	return p.setSlot(OutputsRootSlot, common.TrimLeftZeroes(root.Bytes()))
}

// setSlot writes a storage value the way geth writes account storage: the
// trie key is the keccak256 of the 32-byte slot, and the value is the RLP
// encoding of the value's significant bytes.
func (p *PasserTrie) setSlot(slot common.Hash, value []byte) error {
	encoded, err := rlp.EncodeToBytes(value)
	if err != nil {
		return err
	}
	return p.t.Update(crypto.Keccak256(slot.Bytes()), encoded)
}

// Root returns the trie's current root hash — the block's withdrawalsRoot.
func (p *PasserTrie) Root() common.Hash {
	return p.t.Hash()
}

// SlotValue returns the raw storage value at a slot — RLP-decoded, so 0x01
// for a sent message — or nil when the slot is empty.
func (p *PasserTrie) SlotValue(slot common.Hash) ([]byte, error) {
	encoded, err := p.t.Get(crypto.Keccak256(slot.Bytes()))
	if err != nil || len(encoded) == 0 {
		return nil, err
	}
	var value []byte
	if err := rlp.DecodeBytes(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// Prove builds the Merkle proof for a slot: the RLP-encoded trie nodes from
// the root to the slot's leaf, which is the []bytes form eth_getProof serves
// and SecureMerkleTrie.verifyInclusionProof consumes. A proof for an absent
// slot is an exclusion proof, exactly as eth_getProof would serve one.
func (p *PasserTrie) Prove(slot common.Hash) ([][]byte, error) {
	var proof proofList
	if err := p.t.Prove(crypto.Keccak256(slot.Bytes()), &proof); err != nil {
		return nil, err
	}
	return proof, nil
}

// ProveWithdrawal is Prove for a withdrawal hash's sentMessages slot,
// returning the slot alongside so callers do not recompute it.
func (p *PasserTrie) ProveWithdrawal(withdrawalHash common.Hash) (common.Hash, [][]byte, error) {
	slot := withdrawalSlot(withdrawalHash)
	proof, err := p.Prove(slot)
	return slot, proof, err
}

// proofList collects trie nodes in path order, which is the order proof
// verifiers expect. The trie's Prove writes each node once, root first.
type proofList [][]byte

func (p *proofList) Put(_ []byte, value []byte) error {
	*p = append(*p, common.CopyBytes(value))
	return nil
}

func (p *proofList) Delete([]byte) error {
	return errors.New("proofList does not delete")
}

// StorageProof is one storage slot's Merkle proof against a block's
// withdrawalsRoot — the payload of eth_getProof's storageProof entries.
type StorageProof struct {
	Slot common.Hash
	// Value is the RLP-decoded storage value: 0x01 for a sent withdrawal,
	// the outputs root for OutputsRootSlot, nil for an absent slot.
	Value []byte
	// Proof is the RLP-encoded trie nodes, root first.
	Proof [][]byte
}

// PasserProofAt builds storage proofs against the withdrawal commitment as of
// a block. Proofs for absent slots are exclusion proofs, as eth_getProof
// serves them. The second result is false if the block is unknown.
func (c *Chain) PasserProofAt(blockHash common.Hash, slots []common.Hash) ([]StorageProof, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	passer, ok := c.passers[blockHash]
	if !ok {
		return nil, false, nil
	}
	proofs := make([]StorageProof, 0, len(slots))
	for _, slot := range slots {
		proof, err := passer.Prove(slot)
		if err != nil {
			return nil, true, err
		}
		value, err := passer.SlotValue(slot)
		if err != nil {
			return nil, true, err
		}
		proofs = append(proofs, StorageProof{Slot: slot, Value: value, Proof: proof})
	}
	return proofs, true, nil
}

// rebuildPasser reconstructs the passer trie as of a block from the store, by
// walking the chain back to genesis and replaying every block's recorded
// outputs into a fresh trie. It is how a restart recovers the commitment
// without re-executing the machine: the withdrawal set is a pure function of
// the outputs already persisted per block.
//
// The rebuilt root is checked against the header the block was stored with,
// so a store whose outputs no longer reproduce the committed root fails the
// restart rather than serving unprovable withdrawals.
func rebuildPasser(store *Store, base *Block, tree OutputTree) (*PasserTrie, error) {
	var lineage []*Block
	for b := base; b.NumberU64() > 0; {
		parent := store.Block(b.Header.ParentHash)
		if parent == nil {
			return nil, fmt.Errorf("block %s is missing from the store", b.Header.ParentHash)
		}
		lineage = append(lineage, b)
		b = parent
	}
	passer := NewPasserTrie()
	for i := len(lineage) - 1; i >= 0; i-- {
		b := lineage[i]
		outputs, _, _, err := store.Records(b.Hash())
		if err != nil {
			return nil, fmt.Errorf("no output records for block %d: %w", b.NumberU64(), err)
		}
		for _, h := range withdrawalHashes(outputs) {
			if err := passer.InsertWithdrawal(h); err != nil {
				return nil, err
			}
		}
	}
	if err := passer.SetOutputsRoot(tree.Root()); err != nil {
		return nil, err
	}
	if base.Header.WithdrawalsHash == nil || passer.Root() != *base.Header.WithdrawalsHash {
		return nil, fmt.Errorf("rebuilt withdrawals root %s, but block %d records %v",
			passer.Root(), base.NumberU64(), base.Header.WithdrawalsHash)
	}
	return passer, nil
}
