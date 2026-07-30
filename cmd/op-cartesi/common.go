package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/machine"
)

// chainFlags are the consensus-relevant settings. They must be identical on
// every node of the chain: they determine the genesis block hash and how
// blocks are built and validated. Both `run` and `genesis` register them so
// the generated rollup.json describes the chain the node actually serves.
type chainFlags struct {
	machineRemote string
	bootCycles    uint64
	chainID       uint64
	genesisTime   uint64
	gasLimit      uint64
	maxCycles     uint64
	snapshots     int
	holocene      bool
	isthmus       bool
	denominator   uint64
	elasticity    uint64
}

func (f *chainFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.machineRemote, "machine.remote", "", "URL of a cartesi-jsonrpc-machine server; empty runs the in-memory mock (dev only)")
	fs.Uint64Var(&f.bootCycles, "machine.boot-cycles", 4_000_000_000, "cycle budget for booting the machine to its first input yield")
	fs.Uint64Var(&f.chainID, "chain-id", 901, "L2 chain id")
	fs.Uint64Var(&f.genesisTime, "genesis.timestamp", 0, "timestamp of the genesis block")
	fs.Uint64Var(&f.gasLimit, "gas-limit", 30_000_000, "fallback block gas limit")
	fs.Uint64Var(&f.maxCycles, "max-cycles-per-input", 1_000_000_000, "mcycle budget per input")
	fs.IntVar(&f.snapshots, "snapshots", 32, "number of recent blocks to keep machine snapshots for")
	fs.BoolVar(&f.holocene, "holocene", true, "activate Holocene at genesis (EIP-1559 parameters recorded in header extraData)")
	fs.BoolVar(&f.isthmus, "isthmus", true, "activate Isthmus at genesis (outputs Merkle root published in the header's withdrawalsRoot; required for op-proposer)")
	fs.Uint64Var(&f.denominator, "eip1559.denominator", chain.DefaultEIP1559Denominator, "Holocene EIP-1559 base fee change denominator")
	fs.Uint64Var(&f.elasticity, "eip1559.elasticity", chain.DefaultEIP1559Elasticity, "Holocene EIP-1559 elasticity multiplier")
}

func (f *chainFlags) chainConfig() chain.Config {
	cfg := chain.Config{
		ChainID:            f.chainID,
		GenesisTimestamp:   f.genesisTime,
		GasLimit:           f.gasLimit,
		MaxCyclesPerInput:  f.maxCycles,
		MaxSnapshots:       f.snapshots,
		EIP1559Denominator: f.denominator,
		EIP1559Elasticity:  f.elasticity,
	}
	if f.holocene {
		genesis := uint64(0)
		cfg.HoloceneTime = &genesis
	}
	if f.isthmus {
		genesis := uint64(0)
		cfg.IsthmusTime = &genesis
	}
	return cfg
}

// openMachine connects to the emulator and boots it to its first input-wait
// yield, or returns the deterministic mock when no remote is configured.
func (f *chainFlags) openMachine(ctx context.Context) (machine.Machine, error) {
	if f.machineRemote == "" {
		slog.Warn("no -machine.remote given; using deterministic in-memory mock machine (dev only)")
		return machine.NewMock(fmt.Appendf(nil, "op-cartesi-dev-%d", f.chainID)), nil
	}
	remote, err := machine.DialRemote(ctx, f.machineRemote)
	if err != nil {
		return nil, err
	}
	slog.Info("booting machine to first input yield", "remote", f.machineRemote)
	if err := remote.EnsureReady(ctx, f.bootCycles); err != nil {
		return nil, fmt.Errorf("machine boot: %w", err)
	}
	return remote, nil
}
