// Package integration drives an op-cartesi engine with op-node's own wire
// types, rather than with hand-written JSON. That is the point of these
// tests: op-node serializes payload attributes, deserializes execution
// payloads, and independently recomputes block hashes using logic the engine
// never sees. If header construction or JSON shape drifts from what op-node
// expects, these tests fail where the unit tests could not.
//
// The suite talks to the engine over authenticated HTTP and nothing else, so
// *which* engine it drives is a choice made at startup:
//
//   - by default, one wired up in this process (chain + engineapi over
//     httptest), which is what `go test ./integration` runs;
//   - or one already listening somewhere, named by OP_CARTESI_TEST_ENGINE_URL.
//
// The second is what makes this a conformance suite rather than a Go test:
// docs/ENGINE-RPC-SPEC.md is a wire contract, and an implementation in any
// language that serves it should be able to face this.
package integration

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/engineapi"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
	opcrollup "github.com/tuler/op-cartesi/rollup"
)

// Environment variables selecting a remote engine.
const (
	// envEngineURL is the authenticated Engine API endpoint. Setting it
	// switches every test in this package to that engine.
	envEngineURL = "OP_CARTESI_TEST_ENGINE_URL"
	// envEthURL is the public eth_/cartesi_ endpoint. Defaults to the engine
	// URL, which also serves them (ENGINE-RPC-SPEC §1).
	envEthURL = "OP_CARTESI_TEST_ETH_URL"
	// envJWT is the hex-encoded JWT secret, or a file holding one. Empty
	// means the endpoint is unauthenticated.
	envJWT = "OP_CARTESI_TEST_JWT"
)

const (
	l1ChainID = 900
	blockTime = 2
	// The chain the in-process provider builds. A remote engine's own values
	// are read off it instead (newHarnessFrom), so nothing here is assumed of
	// an engine this suite did not start.
	l2ChainID = 901
	genesisTS = 1_700_000_000
)

// harness is a running engine plus clients wired to it. It holds no handle on
// the engine's internals: everything the tests assert is asked for over the
// wire.
type harness struct {
	t      *testing.T
	engine *engineClient
	node   *nodeClient
	// base is the block this harness builds on: the engine's head when the
	// harness was made. Not necessarily genesis — a remote engine is shared
	// by every test in the run, so each one starts from wherever the last
	// one left it. Tests that need a known starting state say so
	// (newExclusiveHarness).
	base    eth.L2BlockRef
	chainID uint64
	// scripted reports whether this provider can decide what the machine
	// emits. Only the in-process one can; tests that need a particular
	// emission skip without it.
	scripted bool
}

// newHarness returns a harness against whichever engine this run targets.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithOutputs(t, nil)
}

// newEmittingHarness builds a chain whose machine emits one provable output
// per input, so the outputs commitment is exercised. It requires an engine
// whose machine this suite controls.
func newEmittingHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithOutputs(t, func(input []byte) []machine.Output {
		return []machine.Output{
			{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: append([]byte("voucher:"), input[:8]...)},
			{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: []byte("diagnostic")},
		}
	})
}

func newHarnessWithOutputs(t *testing.T, outputFn func([]byte) []machine.Output) *harness {
	t.Helper()
	if url := os.Getenv(envEngineURL); url != "" {
		if outputFn != nil {
			t.Skipf("this test scripts the machine's emissions, which %s cannot do", envEngineURL)
		}
		return remoteHarness(t, url)
	}
	return inProcessHarness(t, outputFn)
}

// inProcessHarness wires an engine into this process and serves it over
// httptest — the same handler assembly cmd/op-cartesi runs, so the
// authentication and JSON layers are the real ones.
func inProcessHarness(t *testing.T, outputFn func([]byte) []machine.Output) *harness {
	t.Helper()
	ctx := context.Background()

	cfg := chain.Config{
		ChainID:          l2ChainID,
		GenesisTimestamp: genesisTS,
		GasLimit:         30_000_000,
	}

	m := machine.NewMock([]byte("op-cartesi-integration"))
	m.OutputFn = outputFn
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
	// HTTP. An engine that got the auth handshake wrong would fail right here.
	client := dial(t, srv.URL, secret[:])
	return newHarnessFrom(t, client, client, true)
}

// remoteHarness dials an engine someone else started. Every test in the run
// shares it and extends it, so each harness builds on the head it finds.
func remoteHarness(t *testing.T, engineURL string) *harness {
	t.Helper()
	secret := jwtSecret(t)
	engineRPC := dial(t, engineURL, secret)

	ethRPC := engineRPC
	if url := os.Getenv(envEthURL); url != "" {
		ethRPC = dial(t, url, secret)
	}
	return newHarnessFrom(t, engineRPC, ethRPC, false)
}

// newHarnessFrom assembles the clients and reads the chain's identity from the
// node itself, so the suite describes whatever engine it was pointed at.
func newHarnessFrom(t *testing.T, engineRPC, ethRPC *rpc.Client, scripted bool) *harness {
	t.Helper()
	ctx := context.Background()
	nodeAPI := newNodeClient(ethRPC)

	chainID, err := nodeAPI.ChainID(ctx)
	if err != nil {
		t.Fatalf("eth_chainId: %v", err)
	}
	head, err := nodeAPI.BlockByTag(ctx, "latest")
	if err != nil {
		t.Fatalf("reading the engine's head: %v", err)
	}
	return &harness{
		t:       t,
		engine:  newEngineClient(engineRPC),
		node:    nodeAPI,
		chainID: chainID,
		base: eth.L2BlockRef{
			Hash:   head.Hash,
			Number: uint64(head.Number),
			Time:   uint64(head.Timestamp),
		},
		scripted: scripted,
	}
}

func dial(t *testing.T, url string, secret []byte) *rpc.Client {
	t.Helper()
	var opts []rpc.ClientOption
	if len(secret) > 0 {
		opts = append(opts, rpc.WithHTTPAuth(node.NewJWTAuth([32]byte(secret))))
	}
	client, err := rpc.DialOptions(context.Background(), url, opts...)
	if err != nil {
		t.Fatalf("dialling %s: %v", url, err)
	}
	t.Cleanup(client.Close)
	return client
}

// jwtSecret reads OP_CARTESI_TEST_JWT, which may be the hex secret itself or
// the path of a file holding one — the same file op-node is given.
func jwtSecret(t *testing.T) []byte {
	t.Helper()
	value := os.Getenv(envJWT)
	if value == "" {
		return nil
	}
	if raw, err := os.ReadFile(value); err == nil {
		value = string(raw)
	}
	secret, err := hexutil.Decode("0x" + strings.TrimPrefix(strings.TrimSpace(value), "0x"))
	if err != nil {
		t.Fatalf("%s is neither a hex secret nor a file holding one: %v", envJWT, err)
	}
	if len(secret) != 32 {
		t.Fatalf("%s decodes to %d bytes, want 32", envJWT, len(secret))
	}
	return secret
}

// head returns the height of the engine's unsafe head.
func (h *harness) head(ctx context.Context) uint64 {
	h.t.Helper()
	b, err := h.node.BlockByTag(ctx, "latest")
	if err != nil {
		h.t.Fatalf("reading the head: %v", err)
	}
	return uint64(b.Number)
}

// headHash returns the hash of the engine's unsafe head.
func (h *harness) headHash(ctx context.Context) common.Hash {
	h.t.Helper()
	b, err := h.node.BlockByTag(ctx, "latest")
	if err != nil {
		h.t.Fatalf("reading the head: %v", err)
	}
	return b.Hash
}

// newExclusiveHarness returns an engine this test alone drives, from a state
// it chose. The in-process provider makes a fresh one per call; a remote
// engine is shared by the whole run and cannot be rewound, so tests needing
// this skip against one.
//
// Two properties need it: that identical input sequences reach identical
// state roots (which requires running the same sequence twice from the same
// state), and that an independent verifier rebuilds a sequencer's block
// (which requires two engines). The devnet checks the second against two real
// nodes anyway — compose.yaml's verifier-engine — which is the stronger
// version of the same test.
func newExclusiveHarness(t *testing.T) *harness {
	t.Helper()
	if os.Getenv(envEngineURL) != "" {
		t.Skipf("this test needs an engine it alone drives; %s is shared by the run", envEngineURL)
	}
	return newHarness(t)
}

// rollupParams builds the inputs the `genesis` subcommand would use for the
// chain this harness is pointed at.
func (h *harness) rollupParams() opcrollup.Params {
	return opcrollup.Params{
		L1ChainID: l1ChainID,
		L2ChainID: h.chainID,
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
}
