// op-cartesi is the Engine API shim that lets a Cartesi Machine serve as the
// execution layer of an OP Stack chain. It speaks engine_* + a minimal eth_*
// subset to op-node on an authenticated port, eth_* to everything else on a
// public port, and drives a Cartesi Machine (remote emulator, or an in-memory
// mock for development) behind them.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/engineapi"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		engineAddr    = flag.String("engine.addr", "127.0.0.1:8551", "listen address for the authenticated Engine API (op-node)")
		httpAddr      = flag.String("http.addr", "127.0.0.1:8545", "listen address for the public eth_* RPC")
		jwtSecretPath = flag.String("engine.jwt-secret", "", "path to a 32-byte hex JWT secret; empty disables auth (dev only)")
		machineRemote = flag.String("machine.remote", "", "URL of a cartesi-jsonrpc-machine server; empty runs the in-memory mock (dev only)")
		bootCycles    = flag.Uint64("machine.boot-cycles", 4_000_000_000, "cycle budget for booting the machine to its first input yield")
		chainID       = flag.Uint64("chain-id", 901, "L2 chain id")
		genesisTime   = flag.Uint64("genesis.timestamp", 0, "timestamp of the genesis block")
		gasLimit      = flag.Uint64("gas-limit", 30_000_000, "fallback block gas limit")
		maxCycles     = flag.Uint64("max-cycles-per-input", 1_000_000_000, "mcycle budget per input")
		snapshots     = flag.Int("snapshots", 32, "number of recent blocks to keep machine snapshots for")
		poolSize      = flag.Int("mempool.size", 4096, "maximum transactions queued in the mempool")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var jwtSecret []byte
	if *jwtSecretPath != "" {
		raw, err := os.ReadFile(*jwtSecretPath)
		if err != nil {
			return fmt.Errorf("reading jwt secret: %w", err)
		}
		jwtSecret, err = hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
		if err != nil || len(jwtSecret) != 32 {
			return fmt.Errorf("jwt secret must be 32 hex-encoded bytes")
		}
	} else {
		slog.Warn("engine API authentication is DISABLED; pass -engine.jwt-secret outside of development")
	}

	var m machine.Machine
	if *machineRemote != "" {
		remote, err := machine.DialRemote(ctx, *machineRemote)
		if err != nil {
			return err
		}
		slog.Info("booting machine to first input yield", "remote", *machineRemote)
		if err := remote.EnsureReady(ctx, *bootCycles); err != nil {
			return fmt.Errorf("machine boot: %w", err)
		}
		m = remote
	} else {
		slog.Warn("no -machine.remote given; using deterministic in-memory mock machine (dev only)")
		m = machine.NewMock(fmt.Appendf(nil, "op-cartesi-dev-%d", *chainID))
	}

	pool := mempool.New(*poolSize)
	c, err := chain.New(ctx, chain.Config{
		ChainID:           *chainID,
		GenesisTimestamp:  *genesisTime,
		GasLimit:          *gasLimit,
		MaxCyclesPerInput: *maxCycles,
		MaxSnapshots:      *snapshots,
	}, m, pool)
	if err != nil {
		return err
	}
	genesis := c.HeadBlock()
	slog.Info("chain initialized",
		"chainId", *chainID,
		"genesisHash", genesis.Hash(),
		"genesisStateRoot", genesis.Header.Root,
		"genesisTimestamp", genesis.Time(),
	)

	engineHandler, err := engineapi.NewHandler(c, pool, true, jwtSecret)
	if err != nil {
		return err
	}
	ethHandler, err := engineapi.NewHandler(c, pool, false, nil)
	if err != nil {
		return err
	}

	servers := []*http.Server{
		{Addr: *engineAddr, Handler: engineHandler},
		{Addr: *httpAddr, Handler: ethHandler},
	}
	errCh := make(chan error, len(servers))
	for i, srv := range servers {
		name := []string{"engine", "http"}[i]
		go func() {
			slog.Info("rpc server listening", "endpoint", name, "addr", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		return err
	}
	shutdownCtx := context.Background()
	for _, srv := range servers {
		srv.Shutdown(shutdownCtx)
	}
	return nil
}
