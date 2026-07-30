package chain

import (
	"fmt"
	"math/big"
)

// CyclesPerGas converts machine mcycles (the native cost unit) into the gas
// figures reported in block headers. Placeholder ratio until real metering is
// calibrated; both building and import use it, so it is consensus-critical.
const CyclesPerGas = 1000

// Default Holocene EIP-1559 parameters, matching the OP Stack defaults. They
// are recorded in block headers from Holocene onward and are only used when
// op-node sends zeroed parameters (which mean "use the chain defaults").
const (
	DefaultEIP1559Denominator = 250
	DefaultEIP1559Elasticity  = 6
)

type Config struct {
	// ChainID of the L2 chain.
	ChainID uint64
	// GenesisTimestamp is the timestamp of block 0.
	GenesisTimestamp uint64
	// GasLimit is the block gas limit used when payload attributes do not
	// carry one (op-node normally sends the SystemConfig gas limit).
	GasLimit uint64
	// BaseFee is the constant base fee recorded in headers. EIP-1559 base fee
	// adjustment is not implemented in the MVP.
	BaseFee *big.Int
	// MaxCyclesPerInput bounds machine execution for a single input; an input
	// exceeding it is treated as rejected (no state effect).
	MaxCyclesPerInput uint64
	// MaxSnapshots is how many recent blocks keep a live machine snapshot.
	// Blocks older than this cannot be built on or re-verified locally.
	MaxSnapshots int

	// HoloceneTime activates Holocene at the given L2 timestamp. From
	// Holocene onward the EIP-1559 parameters are encoded in each header's
	// extraData; before it, extraData must be empty. Nil means never.
	HoloceneTime *uint64
	// EIP1559Denominator and EIP1559Elasticity are the chain defaults written
	// into headers when op-node sends zeroed Holocene parameters.
	EIP1559Denominator uint64
	EIP1559Elasticity  uint64
}

// IsHolocene reports whether Holocene is active at the given L2 timestamp. It
// is half of op-geth's eip1559.ForkChecker, which this config implements so
// op-geth's own extraData encoders can be reused.
func (c Config) IsHolocene(time uint64) bool {
	return c.HoloceneTime != nil && time >= *c.HoloceneTime
}

// IsJovian always reports false: Jovian requires a minimum-base-fee field this
// shim does not implement yet. Configuring it is rejected at startup.
func (c Config) IsJovian(uint64) bool { return false }

// Validate rejects configurations this shim cannot serve correctly.
func (c Config) Validate() error {
	if c.ChainID == 0 {
		return fmt.Errorf("chain id must be set")
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.GasLimit == 0 {
		c.GasLimit = 30_000_000
	}
	if c.BaseFee == nil {
		c.BaseFee = big.NewInt(1_000_000_000)
	}
	if c.MaxCyclesPerInput == 0 {
		c.MaxCyclesPerInput = 1_000_000_000
	}
	if c.MaxSnapshots <= 0 {
		c.MaxSnapshots = 32
	}
	if c.EIP1559Denominator == 0 {
		c.EIP1559Denominator = DefaultEIP1559Denominator
	}
	if c.EIP1559Elasticity == 0 {
		c.EIP1559Elasticity = DefaultEIP1559Elasticity
	}
	return c
}
