// Package integration drives the op-cartesi Engine API shim with op-node's own
// wire types, rather than with hand-written JSON. That is the point of these
// tests: op-node serializes payload attributes, deserializes execution
// payloads, and independently recomputes block hashes using logic the shim
// never sees. If the shim's header construction or JSON shape drifts from what
// op-node expects, these tests fail where the unit tests could not.
package integration

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/engineapi"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
	opcrollup "github.com/tuler/op-cartesi/rollup"
)

const (
	l1ChainID = 900
	l2ChainID = 901
	blockTime = 2
	genesisTS = 1_700_000_000
)

// harness is a running shim plus a client wired to it over authenticated HTTP.
type harness struct {
	t          *testing.T
	chain      *chain.Chain
	pool       *mempool.Pool
	engine     *engineClient
	holocene   bool
	genesisRef eth.L2BlockRef
}

func newHarness(t *testing.T, holocene bool) *harness {
	t.Helper()
	ctx := context.Background()

	cfg := chain.Config{
		ChainID:          l2ChainID,
		GenesisTimestamp: genesisTS,
		GasLimit:         30_000_000,
	}
	if holocene {
		at := uint64(0)
		cfg.HoloceneTime = &at
	}

	m := machine.NewMock([]byte("op-cartesi-integration"))
	pool := mempool.New(256)
	c, err := chain.New(ctx, cfg, m, pool)
	if err != nil {
		t.Fatal(err)
	}

	secret := [32]byte(crypto.Keccak256([]byte("integration-jwt-secret")))
	handler, err := engineapi.NewHandler(c, pool, true, secret[:])
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Dial exactly the way op-node dials an execution engine: JWT-authenticated
	// HTTP. A shim that got the auth handshake wrong would fail right here.
	rpcClient, err := rpc.DialOptions(ctx, srv.URL, rpc.WithHTTPAuth(node.NewJWTAuth(secret)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rpcClient.Close)

	genesis := c.HeadBlock()
	return &harness{
		t:        t,
		chain:    c,
		pool:     pool,
		engine:   newEngineClient(rpcClient),
		holocene: holocene,
		genesisRef: eth.L2BlockRef{
			Hash:   genesis.Hash(),
			Number: 0,
			Time:   genesis.Time(),
		},
	}
}

// rollupParams builds the inputs the `genesis` subcommand would use for this
// harness's chain.
func (h *harness) rollupParams() opcrollup.Params {
	p := opcrollup.Params{
		L1ChainID: l1ChainID,
		L2ChainID: l2ChainID,
		L1Genesis: opcrollup.BlockID{
			Hash:   common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
			Number: 0,
		},
		BlockTime:              blockTime,
		BatcherAddr:            common.HexToAddress("0x42000000000000000000000000000000000000f0"),
		BatchInboxAddress:      common.HexToAddress("0xff00000000000000000000000000000000000901"),
		DepositContractAddress: common.HexToAddress("0x6900000000000000000000000000000000000001"),
		L1SystemConfigAddress:  common.HexToAddress("0x6900000000000000000000000000000000000002"),
		EIP1559Denominator:     chain.DefaultEIP1559Denominator,
		EIP1559Elasticity:      chain.DefaultEIP1559Elasticity,
	}
	if h.holocene {
		at := uint64(0)
		p.HoloceneTime = &at
	}
	return p
}
