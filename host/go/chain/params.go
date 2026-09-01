package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"
)

// Params are a chain's consensus parameters: the genesis parameters of
// docs/BLOCKS-SPEC.md §4.1 and the state-transition parameters of §4.2, and
// nothing else.
//
// The distinction is the whole point of the type. §4.1 enters the genesis
// header, so op-node's handshake catches a node that disagrees; §4.2 changes
// what the machine computes while being invisible to that handshake, so nodes
// that disagree on it diverge at the first block that notices — which may be
// long after startup. Both must therefore travel together, as one document
// every node of a chain is given.
//
// Node policy (§4.3 — snapshot retention, checkpointing, mempool size) is
// deliberately absent: two nodes of the same chain may differ there, and
// putting it here would suggest otherwise.
//
// This is the document a second implementation reads. It is JSON rather than
// a command line because a command line is one implementation's flag syntax,
// and the devnet used to share these values between the config generator and
// the engines as a file of exactly that.
type Params struct {
	// ChainID is the L2 chain id. It does *not* enter the genesis header, so
	// two chains differing only here share a genesis hash and op-node cannot
	// tell them apart — one reason this document exists.
	ChainID uint64 `json:"chainId"`
	// GenesisTimestamp is the timestamp of block 0.
	GenesisTimestamp uint64 `json:"genesisTimestamp"`
	// GasLimit is the genesis header's, and the fallback when payload
	// attributes carry none.
	GasLimit uint64 `json:"gasLimit"`
	// BaseFee is the constant every header carries. There is no fee market
	// (§9.2), so this is not a starting value but the value.
	BaseFee *big.Int `json:"baseFee"`
	// MaxCyclesPerInput bounds one input's execution.
	MaxCyclesPerInput uint64 `json:"maxCyclesPerInput"`
	// AppContract is reported to the guest in every input envelope (§7.1).
	AppContract common.Address `json:"appContract"`
	// EIP1559Denominator and EIP1559Elasticity are written into headers when
	// op-node sends zeroed Holocene parameters.
	EIP1559Denominator uint64 `json:"eip1559Denominator"`
	EIP1559Elasticity  uint64 `json:"eip1559Elasticity"`
}

// paramsJSON is the wire form. BaseFee travels as a decimal string because a
// JSON number is a float to most parsers, and a base fee is a uint256.
type paramsJSON struct {
	ChainID          uint64 `json:"chainId"`
	GenesisTimestamp uint64 `json:"genesisTimestamp"`
	GasLimit         uint64 `json:"gasLimit"`
	// A pointer so an omitted baseFee stays absent and picks up the default,
	// while an explicit "0" is honoured as zero.
	BaseFee            *string        `json:"baseFee"`
	MaxCyclesPerInput  uint64         `json:"maxCyclesPerInput"`
	AppContract        common.Address `json:"appContract"`
	EIP1559Denominator uint64         `json:"eip1559Denominator"`
	EIP1559Elasticity  uint64         `json:"eip1559Elasticity"`
}

func (p Params) MarshalJSON() ([]byte, error) {
	baseFee := p.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	encoded := baseFee.String()
	return json.Marshal(paramsJSON{
		ChainID:            p.ChainID,
		GenesisTimestamp:   p.GenesisTimestamp,
		GasLimit:           p.GasLimit,
		BaseFee:            &encoded,
		MaxCyclesPerInput:  p.MaxCyclesPerInput,
		AppContract:        p.AppContract,
		EIP1559Denominator: p.EIP1559Denominator,
		EIP1559Elasticity:  p.EIP1559Elasticity,
	})
}

func (p *Params) UnmarshalJSON(data []byte) error {
	var raw paramsJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	// An unknown key is a mistake worth reporting: a typo in a consensus
	// parameter would otherwise be silently ignored and leave the node
	// serving a different chain than its operator wrote down.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	// Absent means "the default", which withDefaults fills from a nil; a
	// present value, zero included, is what the document says.
	var baseFee *big.Int
	if raw.BaseFee != nil {
		parsed, ok := new(big.Int).SetString(*raw.BaseFee, 10)
		if !ok {
			return fmt.Errorf("baseFee %q is not a decimal integer", *raw.BaseFee)
		}
		if parsed.Sign() < 0 {
			return fmt.Errorf("baseFee %q is negative", *raw.BaseFee)
		}
		baseFee = parsed
	}
	*p = Params{
		ChainID:            raw.ChainID,
		GenesisTimestamp:   raw.GenesisTimestamp,
		GasLimit:           raw.GasLimit,
		BaseFee:            baseFee,
		MaxCyclesPerInput:  raw.MaxCyclesPerInput,
		AppContract:        raw.AppContract,
		EIP1559Denominator: raw.EIP1559Denominator,
		EIP1559Elasticity:  raw.EIP1559Elasticity,
	}
	return nil
}

// ParamsOf extracts the consensus half of a configuration.
func ParamsOf(cfg Config) Params {
	cfg = cfg.withDefaults()
	return Params{
		ChainID:            cfg.ChainID,
		GenesisTimestamp:   cfg.GenesisTimestamp,
		GasLimit:           cfg.GasLimit,
		BaseFee:            cfg.BaseFee,
		MaxCyclesPerInput:  cfg.MaxCyclesPerInput,
		AppContract:        cfg.AppContract,
		EIP1559Denominator: cfg.EIP1559Denominator,
		EIP1559Elasticity:  cfg.EIP1559Elasticity,
	}
}

// Apply returns cfg with the consensus parameters replaced by p's, leaving
// its node policy alone. A zero field means "the default", the same as it
// does everywhere else, so a sparse document is legal and means what a
// complete one written from it would.
func (p Params) Apply(cfg Config) Config {
	cfg.ChainID = p.ChainID
	cfg.GenesisTimestamp = p.GenesisTimestamp
	cfg.GasLimit = p.GasLimit
	cfg.BaseFee = p.BaseFee
	cfg.MaxCyclesPerInput = p.MaxCyclesPerInput
	cfg.AppContract = p.AppContract
	cfg.EIP1559Denominator = p.EIP1559Denominator
	cfg.EIP1559Elasticity = p.EIP1559Elasticity
	return cfg.withDefaults()
}

// LoadParams reads a chain configuration document.
func LoadParams(path string) (Params, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Params{}, fmt.Errorf("reading the chain configuration: %w", err)
	}
	var p Params
	if err := json.Unmarshal(raw, &p); err != nil {
		return Params{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if p.ChainID == 0 {
		return Params{}, fmt.Errorf("%s sets no chainId", path)
	}
	return p, nil
}

// Write serializes the document, filled in: every parameter explicit, so what
// a node was told is what a reader sees rather than a set of omissions.
func (p Params) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	return enc.Encode(ParamsOf(p.Apply(Config{})))
}
