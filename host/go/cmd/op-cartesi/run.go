package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/engineapi"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

func runCommand(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var cf chainFlags
	cf.register(fs)
	var (
		engineAddr    = fs.String("engine.addr", "127.0.0.1:8551", "listen address for the authenticated Engine API (op-node)")
		httpAddr      = fs.String("http.addr", "127.0.0.1:8545", "listen address for the public eth_* RPC")
		jwtSecretPath = fs.String("engine.jwt-secret", "", "path to a 32-byte hex JWT secret; empty disables auth (dev only)")
		poolSize      = fs.Int("mempool.size", 4096, "maximum transactions queued in the mempool")
		dataDir       = fs.String("datadir", "", "directory for the persistent chain store; empty keeps the chain in memory only")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	jwtSecret, err := loadJWTSecret(*jwtSecretPath)
	if err != nil {
		return err
	}

	pool := mempool.New(*poolSize)
	c, err := cf.openChain(ctx, *dataDir, pool)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	// Nonce-gate the mempool against the guest's accounts drive at the head
	// block. This is a courtesy filter at ingress — the guest is the enforcer
	// (docs/ACCOUNTS.md §6.2) — so a machine without a drive (the mock, or a
	// pre-drive guest) simply reports nonce 0 and the gate degrades to
	// duplicate-(sender,nonce) filtering.
	pool.SetNonceGate(new(big.Int).SetUint64(c.Config().ChainID), func(addr common.Address) (uint64, error) {
		nonce, _, err := c.AccountAt(ctx, c.HeadBlock().Hash(), addr)
		if errors.Is(err, chain.ErrNoAccountsDrive) {
			return 0, nil
		}
		return nonce, err
	})
	head := c.HeadBlock()
	slog.Info("chain initialized",
		"chainId", c.Config().ChainID,
		"genesisHash", c.GenesisHash(),
		"head", head.NumberU64(),
		"headStateRoot", head.Header.Root,
		"headTimestamp", head.Time(),
	)

	engineHandler, err := engineapi.NewHandler(c, pool, true, jwtSecret)
	if err != nil {
		return err
	}
	ethHandler, err := engineapi.NewHandler(c, pool, false, nil)
	if err != nil {
		return err
	}

	servers := []struct {
		name string
		srv  *http.Server
	}{
		{"engine", &http.Server{Addr: *engineAddr, Handler: engineHandler}},
		{"http", &http.Server{Addr: *httpAddr, Handler: ethHandler}},
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			slog.Info("rpc server listening", "endpoint", s.name, "addr", s.srv.Addr)
			if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("%s server: %w", s.name, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		return err
	}
	for _, s := range servers {
		s.srv.Shutdown(context.Background())
	}
	return nil
}

func loadJWTSecret(path string) ([]byte, error) {
	if path == "" {
		slog.Warn("engine API authentication is DISABLED; pass -engine.jwt-secret outside of development")
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading jwt secret: %w", err)
	}
	secret, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil || len(secret) != 32 {
		return nil, fmt.Errorf("jwt secret must be 32 hex-encoded bytes")
	}
	return secret, nil
}
