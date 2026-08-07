package engineapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/chain"
)

// CartesiAPI exposes the machine's own vocabulary, which the eth_* surface
// cannot express faithfully.
//
// Two things need it. Reports are diagnostic and explicitly not provable, so
// they must not be dressed up as logs — logs are what outputs become, and
// conflating them would suggest reports can be proven on L1. And an output's
// chain-wide index and the accumulator it belongs to are what an on-chain
// proof is built from, which no standard receipt field carries.
type CartesiAPI struct {
	chain *chain.Chain
}

func NewCartesiAPI(c *chain.Chain) *CartesiAPI {
	return &CartesiAPI{chain: c}
}

// Output is one provable emission, with the index an on-chain proof needs.
type Output struct {
	// Index is the output's position in the chain-wide outputs tree, which is
	// the outputIndex of a Cartesi output validity proof.
	Index hexutil.Uint64 `json:"index"`
	Data  hexutil.Bytes  `json:"data"`
}

// Emissions is everything the machine produced for one transaction.
type Emissions struct {
	TransactionHash  common.Hash    `json:"transactionHash"`
	TransactionIndex hexutil.Uint   `json:"transactionIndex"`
	BlockHash        common.Hash    `json:"blockHash"`
	BlockNumber      hexutil.Uint64 `json:"blockNumber"`
	// Accepted is false when the machine rejected the input, in which case it
	// had no effect on state and produced no provable outputs.
	Accepted bool `json:"accepted"`
	// Cycles consumed processing the input: the chain's native cost unit.
	Cycles hexutil.Uint64 `json:"cycles"`
	// Outputs are the provable emissions (vouchers, notices).
	Outputs []Output `json:"outputs"`
	// Reports are diagnostic and not provable. They are the machine's
	// explanation of what happened, and are typically the only account of why
	// a rejected input failed.
	Reports []hexutil.Bytes `json:"reports"`
}

// GetTransactionEmissions returns the machine emissions recorded for a
// transaction, or null if it is unknown or was reorged out.
func (a *CartesiAPI) GetTransactionEmissions(_ context.Context, txHash common.Hash) (*Emissions, error) {
	found, ok := a.chain.TxEmissions(txHash)
	if !ok {
		return nil, nil
	}
	out := &Emissions{
		TransactionHash:  found.Outputs.TxHash,
		TransactionIndex: hexutil.Uint(found.Outputs.TxIndex),
		BlockHash:        found.BlockHash,
		BlockNumber:      hexutil.Uint64(found.BlockNumber),
		Accepted:         found.Outputs.Accepted,
		Cycles:           hexutil.Uint64(found.Outputs.Cycles),
		Outputs:          make([]Output, 0, len(found.Outputs.Outputs)),
		Reports:          make([]hexutil.Bytes, 0, len(found.Outputs.Reports)),
	}
	for i, data := range found.Outputs.Outputs {
		out.Outputs = append(out.Outputs, Output{
			Index: hexutil.Uint64(found.FirstOutputIndex + uint64(i)),
			Data:  data,
		})
	}
	for _, data := range found.Outputs.Reports {
		out.Reports = append(out.Reports, data)
	}
	return out, nil
}

// OutputsRoot describes the chain-wide outputs commitment as of a block. It is
// the value the block publishes in its withdrawals root, and what op-node
// folds into the L2 output root.
type OutputsRoot struct {
	BlockHash   common.Hash    `json:"blockHash"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	Root        common.Hash    `json:"root"`
	// Count is the number of outputs the tree holds, which bounds the valid
	// output indices at this block.
	Count hexutil.Uint64 `json:"count"`
}

// GetOutputsRoot returns the outputs commitment as of a block.
func (a *CartesiAPI) GetOutputsRoot(_ context.Context, id rpc.BlockNumberOrHash) (*OutputsRoot, error) {
	b, err := blockFromChain(a.chain, id)
	if err != nil || b == nil {
		return nil, err
	}
	tree, ok := a.chain.OutputTreeAt(b.Hash())
	if !ok {
		return nil, fmt.Errorf("no outputs accumulator for block %s", b.Hash())
	}
	return &OutputsRoot{
		BlockHash:   b.Hash(),
		BlockNumber: hexutil.Uint64(b.NumberU64()),
		Root:        tree.Root(),
		Count:       hexutil.Uint64(tree.Count()),
	}, nil
}

// ContractEntry is one routed address, as cartesi_getContracts reports it.
type ContractEntry struct {
	Address common.Address `json:"address"`
	// Kind is "system" (a built-in of the standard), "app" (an application
	// contract), or "token" (a registered token's ERC-20 façade).
	Kind string `json:"kind"`
	// Abi is the recorded standard JSON ABI, embedded as JSON — present for
	// system and app entries. Token façades omit it: their shared ABI is
	// fixed by the standard (EVM-COMPAT §9).
	Abi json.RawMessage `json:"abi,omitempty"`
	// L1Token names the registered L1 token a façade serves.
	L1Token *common.Address `json:"l1Token,omitempty"`
}

// Contracts is cartesi_getContracts's answer: the guest's interface surface
// as of a block.
type Contracts struct {
	BlockHash   common.Hash     `json:"blockHash"`
	BlockNumber hexutil.Uint64  `json:"blockNumber"`
	Contracts   []ContractEntry `json:"contracts"`
}

// GetContracts lists every address the guest routes as of a block — the
// contracts recorded in the ABI drive with their ABIs, and the token façades
// the accounts drive's registry implies — read straight off the parked
// machine's drives, with no execution and no knowledge of the application's
// implementation (EVM-COMPAT §10a). With the accounts drive serving balances
// and this serving interfaces, a node answers "what does this chain speak?"
// from drive bytes alone. The block tag defaults to the head.
func (a *CartesiAPI) GetContracts(ctx context.Context, id *rpc.BlockNumberOrHash) (*Contracts, error) {
	b, err := blockFromChainOptional(a.chain, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	found, err := a.chain.ContractsAt(ctx, b.Hash())
	if err != nil {
		return nil, err
	}
	out := &Contracts{
		BlockHash:   b.Hash(),
		BlockNumber: hexutil.Uint64(b.NumberU64()),
		Contracts:   make([]ContractEntry, 0, len(found)),
	}
	for _, c := range found {
		entry := ContractEntry{Address: c.Address, L1Token: c.L1Token}
		switch c.Kind {
		case chain.CodeKindSystem:
			entry.Kind = "system"
		case chain.CodeKindApp:
			entry.Kind = "app"
		case chain.CodeKindToken:
			entry.Kind = "token"
		default:
			entry.Kind = fmt.Sprintf("kind-%d", c.Kind)
		}
		// Embed the recorded ABI only when it is well-formed JSON — a drive
		// written by a broken guest must not corrupt the whole response.
		if len(c.Abi) > 0 && json.Valid(c.Abi) {
			entry.Abi = json.RawMessage(c.Abi)
		}
		out.Contracts = append(out.Contracts, entry)
	}
	return out, nil
}

// InspectResult is the machine's answer to a read-only query.
type InspectResult struct {
	// Accepted is false when the query was rejected, the inspect analogue of
	// a reverted call.
	Accepted bool            `json:"accepted"`
	Cycles   hexutil.Uint64  `json:"cycles"`
	Reports  []hexutil.Bytes `json:"reports"`
}

// Inspect runs a read-only query against the machine state as of a block,
// returning the reports individually. eth_call exposes the same mechanism but
// must concatenate them into a single return value.
func (a *CartesiAPI) Inspect(ctx context.Context, query hexutil.Bytes, id *rpc.BlockNumberOrHash) (*InspectResult, error) {
	b, err := blockFromChainOptional(a.chain, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	res, err := a.chain.Inspect(ctx, b.Hash(), query)
	if err != nil {
		return nil, err
	}
	out := &InspectResult{
		Accepted: res.Accepted,
		Cycles:   hexutil.Uint64(res.Cycles),
		Reports:  make([]hexutil.Bytes, 0, len(res.Reports)),
	}
	for _, report := range res.Reports {
		out.Reports = append(out.Reports, report)
	}
	return out, nil
}

// AccountProofResult is cartesi_getAccountProof's answer: the account record
// (or its provable absence) plus machine Merkle proofs against the block's
// stateRoot. It is this chain's eth_getProof analogue — there is no MPT and
// never will be (docs/ACCOUNTS.md §6.2) — and it hands an external verifier
// everything spec §12 of docs/ACCOUNTS-DRIVE-SPEC.md asks for:
//
//   - step 1's header constants: the geometry object (echoed from the drive
//     header — seed, capacities, offsets, slot sizes, profile) plus the
//     proven header page itself, so nothing is taken on this node's word;
//   - step 2's byte ranges with proofs: headerPage and walkPages, each one
//     4 KiB page (log2Size 12) carried both drive-relative (driveOffset) and
//     as an absolute machine address, with its raw bytes and its proof of
//     64 − 12 = 52 siblings against stateRoot;
//   - step 3's walk: homeSlot and walkLength say which slots the probe
//     examined; re-running the §6.2 lookup inside the proven pages must
//     terminate there with this result — found holding the returned record,
//     or absent (walk ends at an empty slot or an early termination, both
//     inside the proven range).
//
// All integers are hex-quantity encoded; byte fields are hex data.
type AccountProofResult struct {
	Address     common.Address `json:"address"`
	BlockHash   common.Hash    `json:"blockHash"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	// StateRoot is the block's stateRoot — the machine root hash every
	// proof below folds up to.
	StateRoot common.Hash `json:"stateRoot"`

	// DriveBase is the drive's start address in the machine's address space;
	// driveOffset + driveBase = address for every page below.
	DriveBase hexutil.Uint64 `json:"driveBase"`
	// Geometry echoes the drive header's deployment constants (spec §5).
	Geometry AccountProofGeometry `json:"geometry"`

	// Found reports whether the address has a record. When false, nonce and
	// balance are zero — and the walk pages prove the absence.
	Found   bool           `json:"found"`
	Nonce   hexutil.Uint64 `json:"nonce"`
	Balance *hexutil.Big   `json:"balance"`
	// Record is the raw slot bytes (spec §7 layout), present when found.
	Record hexutil.Bytes `json:"record,omitempty"`
	// SlotIndex is the record's slot in the accounts table, when found.
	SlotIndex *hexutil.Uint64 `json:"slotIndex,omitempty"`

	// HomeSlot and WalkLength describe the probe walk: slots
	// homeSlot … homeSlot+walkLength−1 (cyclic) were examined.
	HomeSlot   hexutil.Uint64 `json:"homeSlot"`
	WalkLength hexutil.Uint64 `json:"walkLength"`

	HeaderPage AccountProofPage `json:"headerPage"`
	// WalkPages cover every examined slot, ascending by driveOffset; a walk
	// wrapping the table end contributes pages from both ends.
	WalkPages []AccountProofPage `json:"walkPages"`
}

// AccountProofGeometry is the drive header's deployment constants, the
// spec §12 step-1 inputs a verifier replays the walk with.
type AccountProofGeometry struct {
	Capacity    hexutil.Uint64 `json:"capacity"`
	TableOffset hexutil.Uint64 `json:"tableOffset"`
	SlotSize    hexutil.Uint64 `json:"slotSize"`
	LoadLimit   hexutil.Uint64 `json:"loadLimit"`
	Seed        hexutil.Bytes  `json:"seed"`
	Profile     hexutil.Uint64 `json:"profile"`
}

// AccountProofPage is one proven 4 KiB drive page.
type AccountProofPage struct {
	DriveOffset hexutil.Uint64 `json:"driveOffset"`
	Address     hexutil.Uint64 `json:"address"`
	Log2Size    hexutil.Uint64 `json:"log2Size"`
	Data        hexutil.Bytes  `json:"data"`
	Proof       MachineProof   `json:"proof"`
}

// MachineProof is a machine.get_proof result, hex-encoded: TargetHash folded
// with the SiblingHashes (in the emulator's order) reproduces RootHash.
type MachineProof struct {
	TargetAddress  hexutil.Uint64 `json:"targetAddress"`
	Log2TargetSize hexutil.Uint64 `json:"log2TargetSize"`
	TargetHash     common.Hash    `json:"targetHash"`
	Log2RootSize   hexutil.Uint64 `json:"log2RootSize"`
	RootHash       common.Hash    `json:"rootHash"`
	SiblingHashes  []common.Hash  `json:"siblingHashes"`
}

// GetAccountProof serves the account record for addr with machine Merkle
// proofs against the block's stateRoot (see AccountProofResult for the
// shape and the verification recipe). The block tag defaults to the head.
// It requires a machine that can prove — a real emulator; against the mock
// this errors rather than serving proofless "proofs".
func (a *CartesiAPI) GetAccountProof(ctx context.Context, addr common.Address, id *rpc.BlockNumberOrHash) (*AccountProofResult, error) {
	b, err := blockFromChainOptional(a.chain, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("unknown block")
	}
	proof, err := a.chain.AccountProofAt(ctx, b.Hash(), addr, true)
	if err != nil {
		return nil, err
	}
	out := &AccountProofResult{
		Address:     addr,
		BlockHash:   b.Hash(),
		BlockNumber: hexutil.Uint64(b.NumberU64()),
		StateRoot:   b.Header.Root,
		DriveBase:   hexutil.Uint64(proof.DriveBase),
		Geometry: AccountProofGeometry{
			Capacity:    hexutil.Uint64(proof.Config.Capacity),
			TableOffset: hexutil.Uint64(proof.Config.TableOffset),
			SlotSize:    hexutil.Uint64(proof.Config.SlotSize),
			LoadLimit:   hexutil.Uint64(proof.Config.LoadLimit),
			Seed:        proof.Config.Seed[:],
			Profile:     hexutil.Uint64(proof.Config.Profile),
		},
		Found:      proof.Found,
		Nonce:      hexutil.Uint64(proof.Nonce),
		Balance:    (*hexutil.Big)(proof.Balance),
		HomeSlot:   hexutil.Uint64(proof.HomeSlot),
		WalkLength: hexutil.Uint64(proof.WalkLength),
		HeaderPage: marshalProvenPage(proof.HeaderPage),
		WalkPages:  make([]AccountProofPage, 0, len(proof.WalkPages)),
	}
	if proof.Found {
		out.Record = proof.Record
		slot := hexutil.Uint64(proof.SlotIndex)
		out.SlotIndex = &slot
	}
	for _, p := range proof.WalkPages {
		out.WalkPages = append(out.WalkPages, marshalProvenPage(p))
	}
	return out, nil
}

func marshalProvenPage(p chain.ProvenPage) AccountProofPage {
	out := AccountProofPage{
		DriveOffset: hexutil.Uint64(p.DriveOffset),
		Address:     hexutil.Uint64(p.Address),
		Log2Size:    12,
		Data:        p.Data,
	}
	if p.Proof != nil {
		out.Proof = MachineProof{
			TargetAddress:  hexutil.Uint64(p.Proof.TargetAddress),
			Log2TargetSize: hexutil.Uint64(p.Proof.Log2TargetSize),
			TargetHash:     p.Proof.TargetHash,
			Log2RootSize:   hexutil.Uint64(p.Proof.Log2RootSize),
			RootHash:       p.Proof.RootHash,
			SiblingHashes:  p.Proof.SiblingHashes,
		}
	}
	return out
}

// OutputProof is what Cartesi's Application.executeOutput needs, plus enough
// context to find the output again.
type OutputProof struct {
	// Output is the raw bytes the machine emitted — the first argument of
	// executeOutput.
	Output hexutil.Bytes `json:"output"`
	// OutputIndex and OutputHashesSiblings are Cartesi's OutputValidityProof,
	// named as its ABI names them so the reply can be passed through.
	OutputIndex          hexutil.Uint64 `json:"outputIndex"`
	OutputHashesSiblings []common.Hash  `json:"outputHashesSiblings"`
	// OutputsMerkleRoot is the root the proof reproduces, which is the block's
	// withdrawalsRoot and the value an L1 validator must have accepted.
	OutputsMerkleRoot common.Hash `json:"outputsMerkleRoot"`
	// ProvenAgainst names the block whose commitment this proof is against.
	ProvenAgainst common.Hash    `json:"provenAgainst"`
	BlockNumber   hexutil.Uint64 `json:"blockNumber"`
	// EmittedIn and EmittedBy name where the output came from.
	EmittedIn hexutil.Uint64 `json:"emittedIn"`
	EmittedBy common.Hash    `json:"emittedBy"`
}

// GetOutputProof proves an output belongs to the outputs commitment of a
// block, which is what lets it be executed on L1.
//
// The block matters: the outputs tree is cumulative, so an output is provable
// against the commitment of the block that emitted it and of every block
// after. A caller executing on L1 wants the proof against a block that has
// actually been proposed, which is usually not the one that emitted the
// output — hence the second argument, defaulting to the safe head, since
// proposals follow the safe chain.
func (a *CartesiAPI) GetOutputProof(_ context.Context, index hexutil.Uint64, id *rpc.BlockNumberOrHash) (*OutputProof, error) {
	target := a.chain.SafeBlock()
	if id != nil {
		b, err := blockFromChain(a.chain, *id)
		if err != nil {
			return nil, err
		}
		target = b
	}
	if target == nil {
		return nil, fmt.Errorf("no block to prove against")
	}
	found, proof, err := a.chain.OutputProofAt(uint64(index), target.Hash())
	if err != nil {
		return nil, err
	}
	return &OutputProof{
		Output:               found.Output,
		OutputIndex:          hexutil.Uint64(proof.OutputIndex),
		OutputHashesSiblings: proof.Siblings,
		OutputsMerkleRoot:    *target.Header.WithdrawalsHash,
		ProvenAgainst:        target.Hash(),
		BlockNumber:          hexutil.Uint64(target.NumberU64()),
		EmittedIn:            hexutil.Uint64(found.BlockNumber),
		EmittedBy:            found.TxHash,
	}, nil
}
