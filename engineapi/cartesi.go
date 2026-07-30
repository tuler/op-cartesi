package engineapi

import (
	"context"
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
