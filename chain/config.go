package chain

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
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
	// AppContract is the L1 application contract address reported to the guest
	// in each input envelope. It carries no meaning inside the machine yet; it
	// becomes load-bearing when vouchers are executed through a Cartesi
	// Application contract, which is where its value must match.
	AppContract common.Address
	// MaxSnapshots is how many recent blocks keep a live machine snapshot.
	// Blocks older than this cannot be built on or re-verified locally.
	MaxSnapshots int

	// EIP1559Denominator and EIP1559Elasticity are the chain defaults written
	// into headers when op-node sends zeroed Holocene parameters.
	EIP1559Denominator uint64
	EIP1559Elasticity  uint64
}

// The chain runs every fork through Isthmus from genesis; there is no pre-fork
// history to preserve, and Isthmus is not optional. op-node computes the L2
// output root pre-Isthmus by asking for an eth_getProof of the
// L2ToL1MessagePasser account and verifying it against the block's state root,
// which cannot work here: the state root is a Cartesi hash tree, not an
// Ethereum MPT. A pre-Isthmus chain could therefore never be proposed, so the
// fork schedule is fixed rather than configurable.
//
// IsHolocene and IsJovian implement op-geth's eip1559.ForkChecker, which lets
// op-geth's own extraData encoders be reused directly.

// IsHolocene always reports true: Holocene is active from genesis.
func (c Config) IsHolocene(uint64) bool { return true }

// IsJovian always reports false: Jovian adds a minimum-base-fee field this
// shim does not implement.
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
