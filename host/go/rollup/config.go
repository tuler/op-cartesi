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
	IsthmusTime  *uint64 `json:"isthmus_time,omitempty"`

	BatchInboxAddress      common.Address `json:"batch_inbox_address"`
	DepositContractAddress common.Address `json:"deposit_contract_address"`
	L1SystemConfigAddress  common.Address `json:"l1_system_config_address"`

	// ChainOpConfig mirrors the execution layer's OptimismConfig. op-node
	// refuses to start without it on a chain it cannot look up in the bundled
	// superchain registry, which is every chain this generator produces.
	ChainOpConfig *ChainOpConfig `json:"chain_op_config,omitempty"`
}

// ChainOpConfig is the fee-parameter half of the execution layer's chain
// config that op-node needs in order to translate zeroed SystemConfig
// EIP-1559 parameters into protocol values.
type ChainOpConfig struct {
	EIP1559Elasticity        uint64  `json:"eip1559Elasticity"`
	EIP1559Denominator       uint64  `json:"eip1559Denominator"`
	EIP1559DenominatorCanyon *uint64 `json:"eip1559DenominatorCanyon,omitempty"`
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

	// EIP1559Denominator and EIP1559Elasticity seed the SystemConfig's
	// Holocene parameters.
	EIP1559Denominator uint32
	EIP1559Elasticity  uint32
	// BaseFeeScalar and BlobBaseFeeScalar are the L1 fee scalars encoded into
	// the Ecotone-format scalar. They default to the values OP chains use.
	BaseFeeScalar     uint32
	BlobBaseFeeScalar uint32
}

// L2Genesis describes the shim's genesis block, as reported by the chain.
type L2Genesis struct {
	Hash      common.Hash
	Timestamp uint64
	GasLimit  uint64
}

// Build assembles the rollup config. Every fork through Isthmus activates at
// genesis: a new chain has no pre-fork history to preserve, and op-node picks
// the Engine API version from these timestamps. Isthmus in particular is not
// optional — without it op-node cannot compute an output root for this chain
// at all. Jovian and later are deliberately absent: Jovian adds a
// minimum-base-fee field the shim does not implement.
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
	if p.BaseFeeScalar == 0 {
		p.BaseFeeScalar = DefaultBaseFeeScalar
	}
	if p.BlobBaseFeeScalar == 0 {
		p.BlobBaseFeeScalar = DefaultBlobBaseFeeScalar
	}

	atGenesis := new(uint64)
	canyonDenominator := uint64(p.EIP1559Denominator)
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
		HoloceneTime:           atGenesis,
		IsthmusTime:            atGenesis,
		BatchInboxAddress:      p.BatchInboxAddress,
		DepositContractAddress: p.DepositContractAddress,
		L1SystemConfigAddress:  p.L1SystemConfigAddress,
		ChainOpConfig: &ChainOpConfig{
			EIP1559Elasticity:        uint64(p.EIP1559Elasticity),
			EIP1559Denominator:       uint64(p.EIP1559Denominator),
			EIP1559DenominatorCanyon: &canyonDenominator,
		},
	}
	putUint32(cfg.Genesis.SystemConfig.EIP1559Params[0:4], p.EIP1559Denominator)
	putUint32(cfg.Genesis.SystemConfig.EIP1559Params[4:8], p.EIP1559Elasticity)
	encodeEcotoneScalar(cfg.Genesis.SystemConfig.Scalar, p.BlobBaseFeeScalar, p.BaseFeeScalar)
	return cfg, nil
}

// Scalar encoding constants. op-node rejects a rollup config whose genesis
// system-config scalar is zero, so a chain that never sets one cannot start.
const (
	// EcotoneScalarVersion is the version byte at the front of the scalar.
	EcotoneScalarVersion = 1
	// DefaultBaseFeeScalar and DefaultBlobBaseFeeScalar are the values OP
	// chains use; they only affect L1 fee accounting, which this chain does
	// not charge for yet.
	DefaultBaseFeeScalar     = 1368
	DefaultBlobBaseFeeScalar = 810949
)

// encodeEcotoneScalar writes the Ecotone scalar layout: a version byte, then
// the two fee scalars in the final eight bytes.
func encodeEcotoneScalar(dst []byte, blobBaseFeeScalar, baseFeeScalar uint32) {
	dst[0] = EcotoneScalarVersion
	putUint32(dst[24:28], blobBaseFeeScalar)
	putUint32(dst[28:32], baseFeeScalar)
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
