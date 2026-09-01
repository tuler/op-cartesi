package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

// chainFlags is how a node is told what to serve: the consensus parameters,
// which every node of the chain must agree on, and the local policy, which
// they need not.
//
// The consensus half is better given as a document than as flags —
// -chain-config, chain.Params, docs/BLOCKS-SPEC.md §4 — because it has to
// reach implementations that do not share this flag syntax. The individual
// flags remain for a single node started by hand, and refuse to be combined
// with the document rather than quietly deciding which wins.
//
// The fork schedule is in neither: every fork through Isthmus is active from
// genesis and none of them is optional. See chain.Config.
type chainFlags struct {
	machineRemote   string
	machineSnapshot string
	chainConfigPath string

	// The consensus parameters (§4.1, §4.2).
	chainID     uint64
	genesisTime uint64
	gasLimit    uint64
	baseFee     string
	maxCycles   uint64
	appContract string
	denominator uint64
	elasticity  uint64

	// Local policy (§4.3).
	snapshots           int
	checkpointInterval  uint64
	checkpointRetention int

	// fs is kept so chainConfig can tell an explicitly-set flag from a
	// default, which is what makes the -chain-config conflict detectable.
	fs *flag.FlagSet
}

// consensusFlagNames are the flags -chain-config supersedes.
var consensusFlagNames = []string{
	"chain-id", "genesis.timestamp", "gas-limit", "base-fee",
	"max-cycles-per-input", "app-contract", "eip1559.denominator", "eip1559.elasticity",
}

func (f *chainFlags) register(fs *flag.FlagSet) {
	f.fs = fs
	fs.StringVar(&f.machineRemote, "machine.remote", "", "URL of a cartesi-jsonrpc-machine server; empty runs the in-memory mock (dev only)")
	fs.StringVar(&f.machineSnapshot, "machine.snapshot", "", "directory of a stored machine to load into the server; empty uses whatever it already has")
	fs.StringVar(&f.chainConfigPath, "chain-config", "", "JSON document holding the chain's consensus parameters (docs/BLOCKS-SPEC.md §4); supersedes the flags below")

	fs.Uint64Var(&f.chainID, "chain-id", 901, "L2 chain id")
	fs.Uint64Var(&f.genesisTime, "genesis.timestamp", 0, "timestamp of the genesis block")
	fs.Uint64Var(&f.gasLimit, "gas-limit", 30_000_000, "fallback block gas limit")
	fs.StringVar(&f.baseFee, "base-fee", "1000000000", "the constant base fee every header carries, in wei")
	fs.Uint64Var(&f.maxCycles, "max-cycles-per-input", 1_000_000_000, "mcycle budget per input")
	fs.StringVar(&f.appContract, "app-contract", "", "L1 application contract address reported to the guest in every input envelope")
	fs.Uint64Var(&f.denominator, "eip1559.denominator", chain.DefaultEIP1559Denominator, "Holocene EIP-1559 base fee change denominator")
	fs.Uint64Var(&f.elasticity, "eip1559.elasticity", chain.DefaultEIP1559Elasticity, "Holocene EIP-1559 elasticity multiplier")

	fs.IntVar(&f.snapshots, "snapshots", 32, "number of recent blocks to keep machine snapshots for")
	fs.Uint64Var(&f.checkpointInterval, "checkpoint-interval", 100, "blocks between machine checkpoints; 0 disables them")
	fs.IntVar(&f.checkpointRetention, "checkpoint-retention", 3, "how many machine checkpoints to keep on disk")
}

// params resolves the consensus parameters, from the document when one is
// given and from the flags otherwise.
func (f *chainFlags) params() (chain.Params, error) {
	if f.chainConfigPath == "" {
		baseFee, ok := new(big.Int).SetString(f.baseFee, 10)
		if !ok {
			return chain.Params{}, fmt.Errorf("-base-fee %q is not a decimal integer", f.baseFee)
		}
		return chain.Params{
			ChainID:            f.chainID,
			GenesisTimestamp:   f.genesisTime,
			GasLimit:           f.gasLimit,
			BaseFee:            baseFee,
			MaxCyclesPerInput:  f.maxCycles,
			AppContract:        common.HexToAddress(f.appContract),
			EIP1559Denominator: f.denominator,
			EIP1559Elasticity:  f.elasticity,
		}, nil
	}
	// Both would be silently reconciled otherwise, and a node serving a
	// chain its operator did not describe is the failure this document
	// exists to prevent.
	if conflicting := f.explicitConsensusFlags(); len(conflicting) > 0 {
		return chain.Params{}, fmt.Errorf("-chain-config sets the consensus parameters; remove -%s",
			strings.Join(conflicting, ", -"))
	}
	return chain.LoadParams(f.chainConfigPath)
}

// explicitConsensusFlags names the consensus flags the command line actually
// set, as opposed to those sitting at their defaults.
func (f *chainFlags) explicitConsensusFlags() []string {
	consensus := make(map[string]bool, len(consensusFlagNames))
	for _, name := range consensusFlagNames {
		consensus[name] = true
	}
	var found []string
	f.fs.Visit(func(fl *flag.Flag) {
		if consensus[fl.Name] {
			found = append(found, fl.Name)
		}
	})
	return found
}

func (f *chainFlags) chainConfig() (chain.Config, error) {
	params, err := f.params()
	if err != nil {
		return chain.Config{}, err
	}
	return params.Apply(chain.Config{
		MaxSnapshots:        f.snapshots,
		CheckpointInterval:  f.checkpointInterval,
		CheckpointRetention: f.checkpointRetention,
	}), nil
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
	cfg, err := f.chainConfig()
	if err != nil {
		return nil, err
	}
	if dataDir == "" {
		m, err := f.openMachine(ctx)
		if err != nil {
			return nil, err
		}
		slog.Warn("running without -datadir; the chain is in memory and a restart loses it")
		return chain.New(ctx, cfg, m, pool)
	}

	store, err := chain.OpenStore(dataDir)
	if err != nil {
		return nil, err
	}
	restored, ok, err := chain.Restore(ctx, cfg, store, f.loadCheckpoint, pool)
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
	c, err := chain.New(ctx, cfg, m, pool)
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
