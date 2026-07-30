// Package rollup emits the rollup configuration file (rollup.json) that
// op-node consumes to derive this chain from L1. The struct mirrors op-node's
// rollup.Config JSON schema; it is redeclared here rather than imported so the
// shim keeps a light dependency tree. The integration test suite loads what
// this package writes through op-node's real parser to keep the two in step.
package rollup

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// BlockID identifies a block by hash and height.
type BlockID struct {
	Hash   common.Hash `json:"hash"`
	Number uint64      `json:"number"`
}

// SystemConfig holds the initial values of the L1 SystemConfig contract that
// derivation needs before it has processed any L1 config-update events.
type SystemConfig struct {
	BatcherAddr       common.Address `json:"batcherAddr"`
	Overhead          hexutil.Bytes  `json:"overhead"`
	Scalar            hexutil.Bytes  `json:"scalar"`
	GasLimit          uint64         `json:"gasLimit"`
	EIP1559Params     hexutil.Bytes  `json:"eip1559Params"`
	OperatorFeeParams hexutil.Bytes  `json:"operatorFeeParams"`
	MinBaseFee        uint64         `json:"minBaseFee"`
}

// Genesis is the anchor point of the rollup: the L1 block derivation starts
// after, and the L2 block it starts from.
type Genesis struct {
	L1           BlockID      `json:"l1"`
	L2           BlockID      `json:"l2"`
	L2Time       uint64       `json:"l2_time"`
	SystemConfig SystemConfig `json:"system_config"`
}

// Config is the rollup.json document.
type Config struct {
	Genesis           Genesis  `json:"genesis"`
	BlockTime         uint64   `json:"block_time"`
	MaxSequencerDrift uint64   `json:"max_sequencer_drift"`
	SeqWindowSize     uint64   `json:"seq_window_size"`
	ChannelTimeout    uint64   `json:"channel_timeout"`
	L1ChainID         *big.Int `json:"l1_chain_id"`
	L2ChainID         *big.Int `json:"l2_chain_id"`

	RegolithTime *uint64 `json:"regolith_time,omitempty"`
	CanyonTime   *uint64 `json:"canyon_time,omitempty"`
	DeltaTime    *uint64 `json:"delta_time,omitempty"`
	EcotoneTime  *uint64 `json:"ecotone_time,omitempty"`
	FjordTime    *uint64 `json:"fjord_time,omitempty"`
	GraniteTime  *uint64 `json:"granite_time,omitempty"`
	HoloceneTime *uint64 `json:"holocene_time,omitempty"`

	BatchInboxAddress      common.Address `json:"batch_inbox_address"`
	DepositContractAddress common.Address `json:"deposit_contract_address"`
	L1SystemConfigAddress  common.Address `json:"l1_system_config_address"`
}

// Params are the operator-supplied inputs to config generation: everything
// that cannot be derived from the L2 genesis block itself.
type Params struct {
	L1ChainID uint64
	L2ChainID uint64
	// L1Genesis is the L1 block the rollup starts after. Derivation of the
	// first L2 epoch begins at the following L1 block.
	L1Genesis BlockID
	BlockTime uint64
	// SequencerDrift, SeqWindowSize and ChannelTimeout default to the values
	// used by OP Stack devnets when left at zero.
	SequencerDrift uint64
	SeqWindowSize  uint64
	ChannelTimeout uint64

	BatcherAddr            common.Address
	BatchInboxAddress      common.Address
	DepositContractAddress common.Address
	L1SystemConfigAddress  common.Address

	// HoloceneTime activates Holocene; nil leaves it inactive. All earlier
	// forks are activated at genesis, which is what a new chain wants.
	HoloceneTime *uint64
	// EIP1559Denominator and EIP1559Elasticity seed the SystemConfig's
	// Holocene parameters. Ignored when Holocene is inactive.
	EIP1559Denominator uint32
	EIP1559Elasticity  uint32
}

// L2Genesis describes the shim's genesis block, as reported by the chain.
type L2Genesis struct {
	Hash      common.Hash
	Timestamp uint64
	GasLimit  uint64
}

// Build assembles the rollup config. Every fork through Granite activates at
// genesis: a new chain has no pre-fork history to preserve, and op-node picks
// the Engine API version from these timestamps. Isthmus and later are
// deliberately absent — they require the V4 engine methods, which this shim
// does not implement yet.
func Build(l2 L2Genesis, p Params) (*Config, error) {
	if p.L1ChainID == 0 || p.L2ChainID == 0 {
		return nil, fmt.Errorf("both l1 and l2 chain ids must be set")
	}
	if p.BlockTime == 0 {
		p.BlockTime = 2
	}
	if p.SequencerDrift == 0 {
		p.SequencerDrift = 600
	}
	if p.SeqWindowSize == 0 {
		p.SeqWindowSize = 3600
	}
	if p.ChannelTimeout == 0 {
		p.ChannelTimeout = 300
	}
	if p.HoloceneTime != nil && *p.HoloceneTime > l2.Timestamp {
		return nil, fmt.Errorf("holocene activation %d is after L2 genesis %d; mid-chain activation is not supported by the generator", *p.HoloceneTime, l2.Timestamp)
	}

	atGenesis := new(uint64)
	cfg := &Config{
		Genesis: Genesis{
			L1:     p.L1Genesis,
			L2:     BlockID{Hash: l2.Hash, Number: 0},
			L2Time: l2.Timestamp,
			SystemConfig: SystemConfig{
				BatcherAddr:       p.BatcherAddr,
				Overhead:          make(hexutil.Bytes, 32),
				Scalar:            make(hexutil.Bytes, 32),
				GasLimit:          l2.GasLimit,
				EIP1559Params:     make(hexutil.Bytes, 8),
				OperatorFeeParams: make(hexutil.Bytes, 32),
			},
		},
		BlockTime:              p.BlockTime,
		MaxSequencerDrift:      p.SequencerDrift,
		SeqWindowSize:          p.SeqWindowSize,
		ChannelTimeout:         p.ChannelTimeout,
		L1ChainID:              new(big.Int).SetUint64(p.L1ChainID),
		L2ChainID:              new(big.Int).SetUint64(p.L2ChainID),
		RegolithTime:           atGenesis,
		CanyonTime:             atGenesis,
		DeltaTime:              atGenesis,
		EcotoneTime:            atGenesis,
		FjordTime:              atGenesis,
		GraniteTime:            atGenesis,
		HoloceneTime:           p.HoloceneTime,
		BatchInboxAddress:      p.BatchInboxAddress,
		DepositContractAddress: p.DepositContractAddress,
		L1SystemConfigAddress:  p.L1SystemConfigAddress,
	}
	if p.HoloceneTime != nil {
		putUint32(cfg.Genesis.SystemConfig.EIP1559Params[0:4], p.EIP1559Denominator)
		putUint32(cfg.Genesis.SystemConfig.EIP1559Params[4:8], p.EIP1559Elasticity)
	}
	return cfg, nil
}

func putUint32(dst []byte, v uint32) {
	dst[0] = byte(v >> 24)
	dst[1] = byte(v >> 16)
	dst[2] = byte(v >> 8)
	dst[3] = byte(v)
}

// Encode serializes the config as indented JSON.
func (c *Config) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}
