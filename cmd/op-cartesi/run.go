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
	"github.com/tuler/op-cartesi/mempool"
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

	m, err := cf.openMachine(ctx)
	if err != nil {
		return err
	}

	pool := mempool.New(*poolSize)
	c, err := chain.New(ctx, cf.chainConfig(), m, pool)
	if err != nil {
		return err
	}
	genesis := c.HeadBlock()
	slog.Info("chain initialized",
		"chainId", cf.chainID,
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
