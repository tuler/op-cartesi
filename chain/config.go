package chain

import "math/big"

// CyclesPerGas converts machine mcycles (the native cost unit) into the gas
// figures reported in block headers. Placeholder ratio until real metering is
// calibrated; both building and import use it, so it is consensus-critical.
const CyclesPerGas = 1000

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
	return c
}
