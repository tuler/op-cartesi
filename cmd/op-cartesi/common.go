package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// chainFlags are the consensus-relevant settings. They must be identical on
// every node of the chain: they determine the genesis block hash and how
// blocks are built and validated. Both `run` and `genesis` register them so
// the generated rollup.json describes the chain the node actually serves.
//
// The fork schedule is not among them: every fork through Isthmus is active
// from genesis and none of them is optional. See chain.Config.
type chainFlags struct {
	machineRemote       string
	machineSnapshot     string
	chainID             uint64
	genesisTime         uint64
	gasLimit            uint64
	maxCycles           uint64
	snapshots           int
	checkpointInterval  uint64
	checkpointRetention int
	denominator         uint64
	elasticity          uint64
}

func (f *chainFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.machineRemote, "machine.remote", "", "URL of a cartesi-jsonrpc-machine server; empty runs the in-memory mock (dev only)")
	fs.StringVar(&f.machineSnapshot, "machine.snapshot", "", "directory of a stored machine to load into the server; empty uses whatever it already has")
	fs.Uint64Var(&f.chainID, "chain-id", 901, "L2 chain id")
	fs.Uint64Var(&f.genesisTime, "genesis.timestamp", 0, "timestamp of the genesis block")
	fs.Uint64Var(&f.gasLimit, "gas-limit", 30_000_000, "fallback block gas limit")
	fs.Uint64Var(&f.maxCycles, "max-cycles-per-input", 1_000_000_000, "mcycle budget per input")
	fs.IntVar(&f.snapshots, "snapshots", 32, "number of recent blocks to keep machine snapshots for")
	fs.Uint64Var(&f.checkpointInterval, "checkpoint-interval", 100, "blocks between machine checkpoints; 0 disables them")
	fs.IntVar(&f.checkpointRetention, "checkpoint-retention", 3, "how many machine checkpoints to keep on disk")
	fs.Uint64Var(&f.denominator, "eip1559.denominator", chain.DefaultEIP1559Denominator, "Holocene EIP-1559 base fee change denominator")
	fs.Uint64Var(&f.elasticity, "eip1559.elasticity", chain.DefaultEIP1559Elasticity, "Holocene EIP-1559 elasticity multiplier")
}

func (f *chainFlags) chainConfig() chain.Config {
	cfg := chain.Config{
		ChainID:             f.chainID,
		GenesisTimestamp:    f.genesisTime,
		GasLimit:            f.gasLimit,
		MaxCyclesPerInput:   f.maxCycles,
		MaxSnapshots:        f.snapshots,
		CheckpointInterval:  f.checkpointInterval,
		CheckpointRetention: f.checkpointRetention,
		EIP1559Denominator:  f.denominator,
		EIP1559Elasticity:   f.elasticity,
	}
	return cfg
}

// openMachine connects to the emulator and checks the machine it holds is
// ready for input, or returns the deterministic mock when no remote is
// configured. The machine is never booted here — see Remote.CheckReady.
func (f *chainFlags) openMachine(ctx context.Context) (machine.Machine, error) {
	if f.machineRemote == "" {
		slog.Warn("no -machine.remote given; using deterministic in-memory mock machine (dev only)")
		return machine.NewMock(fmt.Appendf(nil, "op-cartesi-dev-%d", f.chainID)), nil
	}
	remote, err := machine.DialRemote(ctx, f.machineRemote)
	if err != nil {
		return nil, err
	}
	if f.machineSnapshot != "" {
		slog.Info("loading stored machine", "dir", f.machineSnapshot)
		if err := remote.Load(ctx, f.machineSnapshot); err != nil {
			return nil, fmt.Errorf("loading machine from %s: %w", f.machineSnapshot, err)
		}
	} else if loaded, err := remote.Loaded(ctx); err != nil {
		return nil, err
	} else if !loaded {
		return nil, fmt.Errorf("the machine server has no machine loaded; pass -machine.snapshot")
	}
	if err := remote.CheckReady(ctx); err != nil {
		return nil, fmt.Errorf("machine at %s: %w", f.machineRemote, err)
	}
	return remote, nil
}

// openChain restores the chain from its store when there is one to restore,
// and otherwise starts a fresh one from the machine the server holds.
//
// The distinction matters for the machine: restoring loads a checkpoint into
// the emulator, so the operator's snapshot is not what the node runs. Only a
// first start uses it.
func (f *chainFlags) openChain(ctx context.Context, dataDir string, pool *mempool.Pool) (*chain.Chain, error) {
	if dataDir == "" {
		m, err := f.openMachine(ctx)
		if err != nil {
			return nil, err
		}
		slog.Warn("running without -datadir; the chain is in memory and a restart loses it")
		return chain.New(ctx, f.chainConfig(), m, pool)
	}

	store, err := chain.OpenStore(dataDir)
	if err != nil {
		return nil, err
	}
	restored, ok, err := chain.Restore(ctx, f.chainConfig(), store, f.loadCheckpoint, pool)
	if err != nil {
		store.Close()
		return nil, err
	}
	if ok {
		return restored, nil
	}

	m, err := f.openMachine(ctx)
	if err != nil {
		store.Close()
		return nil, err
	}
	c, err := chain.New(ctx, f.chainConfig(), m, pool)
	if err != nil {
		store.Close()
		return nil, err
	}
	if err := c.Adopt(ctx, store); err != nil {
		store.Close()
		return nil, err
	}
	return c, nil
}

// loadCheckpoint puts a stored machine into the emulator server. It replaces
// whatever the server holds, which is why a restored node ignores
// -machine.snapshot: the snapshot is the chain's genesis, and the checkpoint
// is where the chain actually is.
func (f *chainFlags) loadCheckpoint(ctx context.Context, dir string) (machine.Machine, error) {
	if f.machineRemote == "" {
		return machine.LoadMock(dir)
	}
	remote, err := machine.DialRemote(ctx, f.machineRemote)
	if err != nil {
		return nil, err
	}
	if err := remote.Load(ctx, dir); err != nil {
		return nil, fmt.Errorf("loading checkpoint %s: %w", dir, err)
	}
	if err := remote.CheckReady(ctx); err != nil {
		return nil, err
	}
	return remote, nil
}
