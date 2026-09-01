package integration

// The engine transcript: a whole sequencing run recorded as the JSON-RPC that
// crossed the wire, replayed against an engine to see whether it answers the
// same way.
//
//	go test ./integration -run TestEngineTranscript -update
//
// This is the vector set docs/ENGINE-RPC-SPEC.md §10 calls the most useful and
// conformance/ could not hold, because it is the one thing the header vectors
// cannot pin: the wire itself — method names, argument order, the shape of an
// envelope, SYNCING rather than an error. A second implementation can replay
// it and diff, without reading any Go.
//
// It is replayable against a remote engine too, because the configuration it
// records is exactly what `op-cartesi run` serves with no flags: the same
// chain id, timestamp and gas limit, and the same deterministic mock machine.
// So the transcript is not a recording of this test — it is a recording of
// the node as anyone runs it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/engineapi"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

var update = flag.Bool("update", false, "rewrite ../conformance/engine/sequencing.json")

const transcriptPath = "../conformance/engine/sequencing.json"

// The configuration `op-cartesi run` serves with no flags, so the transcript
// replays against a stock development node (cmd/op-cartesi/common.go).
const (
	devChainID   = 901
	devGenesisTS = 0
	devGasLimit  = 30_000_000
)

// devMockSeed is the seed cmd/op-cartesi gives the mock when no machine
// server is configured. Reproducing the machine is reproducing this.
func devMockSeed() []byte { return fmt.Appendf(nil, "op-cartesi-dev-%d", devChainID) }

type transcriptFile struct {
	Description string `json:"description"`
	Spec        string `json:"spec"`
	// Machine describes the state transition backing the run, so an
	// implementation can stand up the same one.
	Machine transcriptMachine `json:"machine"`
	Params  transcriptParams  `json:"params"`
	// GenesisHash is what the engine must serve at block 0 for the rest of
	// the transcript to mean anything.
	GenesisHash string           `json:"genesisHash"`
	Calls       []transcriptCall `json:"calls"`
}

type transcriptMachine struct {
	Kind string `json:"kind"`
	Seed string `json:"seed"`
	Rule string `json:"rule"`
}

type transcriptParams struct {
	ChainID          uint64 `json:"chainId"`
	GenesisTimestamp uint64 `json:"genesisTimestamp"`
	GasLimit         uint64 `json:"gasLimit"`
}

// transcriptCall is one JSON-RPC exchange, as the client sent and received it.
type transcriptCall struct {
	Note   string            `json:"note"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	Result json.RawMessage   `json:"result"`
	// PayloadIDIn says the sole parameter is the payload id the previous
	// forkchoice update returned. Ids are opaque and need only be stable
	// within one node (BLOCKS-SPEC §15), so a replay substitutes its own
	// rather than sending the recorded one.
	PayloadIDIn bool `json:"payloadIdIn,omitempty"`
	// PayloadIDOut says the result carries a payload id, which for the same
	// reason is not compared.
	PayloadIDOut bool `json:"payloadIdOut,omitempty"`
}

// recorder places calls through an underlying client and keeps what crossed.
type recorder struct {
	inner caller
	note  string
	calls []transcriptCall
}

func (r *recorder) CallContext(ctx context.Context, result any, method string, args ...any) error {
	params := make([]json.RawMessage, 0, len(args))
	for _, arg := range args {
		encoded, err := json.Marshal(arg)
		if err != nil {
			return err
		}
		params = append(params, encoded)
	}
	// Take the response as bytes first, then decode it into what the caller
	// asked for. Re-marshalling the decoded value instead would record this
	// client's idea of the shape — omitting the nulls the server actually
	// sent — and a transcript has to hold what crossed the wire.
	var raw json.RawMessage
	if err := r.inner.CallContext(ctx, &raw, method, args...); err != nil {
		return err
	}
	if result != nil {
		if err := json.Unmarshal(raw, result); err != nil {
			return err
		}
	}
	r.calls = append(r.calls, transcriptCall{Note: r.note, Method: method, Params: params, Result: raw})
	return nil
}

// devEngine wires an engine configured exactly as `op-cartesi run` does with
// no flags, and returns a client onto it.
func devEngine(t *testing.T) (*rpcHarness, *recorder) {
	t.Helper()
	ctx := context.Background()
	cfg := chain.Config{ChainID: devChainID, GenesisTimestamp: devGenesisTS, GasLimit: devGasLimit}
	c, err := chain.New(ctx, cfg, machine.NewMock(devMockSeed()), mempool.New(256))
	if err != nil {
		t.Fatal(err)
	}
	secret := [32]byte(crypto.Keccak256([]byte("transcript-jwt-secret")))
	handler, err := engineapi.NewHandler(c, mempool.New(256), true, secret[:])
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	rec := &recorder{inner: dial(t, srv.URL, secret[:])}
	return &rpcHarness{engine: newEngineClient(rec), node: newNodeClient(rec)}, rec
}

// rpcHarness is the pair of clients a transcript is driven through.
type rpcHarness struct {
	engine *engineClient
	node   *nodeClient
}

// rawCall places a call whose parameters are already encoded, which is how a
// transcript replays what it recorded.
func (h *rpcHarness) rawCall(ctx context.Context, result any, method string, args ...any) error {
	return h.engine.rpc.CallContext(ctx, result, method, args...)
}

// transcriptTarget is the engine a replay runs against: the one named by
// OP_CARTESI_TEST_ENGINE_URL, or a dev-configured engine in this process.
//
// Note that it is NOT the standard harness's engine, which serves a different
// chain — its own genesis timestamp and machine seed. A transcript is tied to
// the exact state transition it recorded, so replaying it anywhere else is
// meaningless rather than merely failing.
func transcriptTarget(t *testing.T) *rpcHarness {
	t.Helper()
	if os.Getenv(envEngineURL) != "" {
		h := newHarness(t)
		return &rpcHarness{engine: h.engine, node: h.node}
	}
	h, _ := devEngine(t)
	return h
}

// driveTranscript performs the run a transcript records: three sequenced
// blocks, then a forkchoice on a head the engine has never heard of.
func driveTranscript(t *testing.T, h *rpcHarness, rec *recorder) {
	t.Helper()
	ctx := context.Background()

	// Part of the conversation, and worth recording: it pins the genesis
	// block's JSON, including the requestsHash and withdrawalsRoot a client
	// that recomputes the hash needs (ENGINE-RPC-SPEC §5.1).
	rec.note = "the genesis block, as a client reads it"
	genesis, err := h.node.BlockByTag(ctx, "earliest")
	if err != nil {
		t.Fatal(err)
	}
	parent := eth.L2BlockRef{Hash: genesis.Hash, Number: 0, Time: uint64(genesis.Timestamp)}

	for i := range 3 {
		rec.note = fmt.Sprintf("block %d: start building on %s", i+1, parent.Hash)
		gasLimit := eth.Uint64Quantity(devGasLimit)
		beaconRoot := common.HexToHash("0xbeac0")
		params := eth.Bytes8{}
		attrs := &eth.PayloadAttributes{
			Timestamp:             eth.Uint64Quantity(parent.Time + blockTime),
			PrevRandao:            eth.Bytes32(common.HexToHash("0x0badc0de")),
			SuggestedFeeRecipient: common.HexToAddress("0x4200000000000000000000000000000000000011"),
			Withdrawals:           &types.Withdrawals{},
			ParentBeaconBlockRoot: &beaconRoot,
			Transactions:          []eth.Data{depositTx(t, parent.Number+1)},
			GasLimit:              &gasLimit,
			EIP1559Params:         &params,
		}
		fc := eth.ForkchoiceState{
			HeadBlockHash:      parent.Hash,
			SafeBlockHash:      genesis.Hash,
			FinalizedBlockHash: genesis.Hash,
		}
		result, err := h.engine.ForkchoiceUpdate(ctx, &fc, attrs)
		if err != nil {
			t.Fatal(err)
		}
		if result.PayloadID == nil {
			t.Fatalf("block %d: no payload id", i+1)
		}

		rec.note = fmt.Sprintf("block %d: fetch what was built", i+1)
		env, err := h.engine.GetPayload(ctx, eth.PayloadInfo{ID: *result.PayloadID})
		if err != nil {
			t.Fatal(err)
		}

		rec.note = fmt.Sprintf("block %d: import it, as a verifier would", i+1)
		if _, err := h.engine.NewPayload(ctx, env.ExecutionPayload, env.ParentBeaconBlockRoot); err != nil {
			t.Fatal(err)
		}

		rec.note = fmt.Sprintf("block %d: adopt it as the head", i+1)
		head := env.ExecutionPayload.BlockHash
		if _, err := h.engine.ForkchoiceUpdate(ctx, &eth.ForkchoiceState{
			HeadBlockHash: head, SafeBlockHash: head, FinalizedBlockHash: genesis.Hash,
		}, nil); err != nil {
			t.Fatal(err)
		}
		parent = eth.L2BlockRef{
			Hash:   common.Hash(head),
			Number: uint64(env.ExecutionPayload.BlockNumber),
			Time:   uint64(env.ExecutionPayload.Timestamp),
		}
	}

	rec.note = "a head the engine has never seen is SYNCING, not an error"
	if _, err := h.engine.ForkchoiceUpdate(ctx, &eth.ForkchoiceState{
		HeadBlockHash:      common.HexToHash("0xdeadbeef"),
		SafeBlockHash:      genesis.Hash,
		FinalizedBlockHash: genesis.Hash,
	}, nil); err != nil {
		t.Fatal(err)
	}
}

// TestEngineTranscript records the run above and replays it — against the
// in-process engine, or against whichever one this run targets.
func TestEngineTranscript(t *testing.T) {
	if *update {
		writeTranscript(t)
	}
	stored := readTranscript(t)
	replayTranscript(t, stored)
}

func writeTranscript(t *testing.T) {
	t.Helper()
	h, rec := devEngine(t)
	genesis, err := h.node.BlockByTag(context.Background(), "earliest")
	if err != nil {
		t.Fatal(err)
	}
	// The genesis read is setup, not part of the conversation op-node has.
	rec.calls = nil
	driveTranscript(t, h, rec)

	calls := make([]transcriptCall, 0, len(rec.calls))
	for _, call := range rec.calls {
		switch call.Method {
		case "engine_forkchoiceUpdatedV3":
			// The id is this node's to choose; blank it so a diff against
			// another implementation does not light up on it.
			var result map[string]json.RawMessage
			if err := json.Unmarshal(call.Result, &result); err != nil {
				t.Fatal(err)
			}
			if _, ok := result["payloadId"]; ok && string(result["payloadId"]) != "null" {
				result["payloadId"] = json.RawMessage(`null`)
				call.PayloadIDOut = true
				if call.Result, err = json.Marshal(result); err != nil {
					t.Fatal(err)
				}
			}
		case "engine_getPayloadV4":
			call.PayloadIDIn = true
			call.Params = []json.RawMessage{json.RawMessage(`null`)}
		}
		calls = append(calls, call)
	}

	built := transcriptFile{
		Description: "A whole sequencing run as JSON-RPC: three blocks built, fetched, imported and " +
			"adopted, then a forkchoice on an unknown head. Recorded from the engine `op-cartesi run` " +
			"serves with no flags, so it replays against a stock development node as well as against " +
			"an implementation that reproduces the machine below.",
		Spec: "docs/ENGINE-RPC-SPEC.md#4-engine--the-engine-api",
		Machine: transcriptMachine{
			Kind: "mock",
			Seed: string(devMockSeed()),
			Rule: "root0 = keccak256(seed); every input advances root' = keccak256(root || input) " +
				"and costs 1000 + 10*len(input) mcycles; every input is accepted and emits nothing.",
		},
		Params:      transcriptParams{ChainID: devChainID, GenesisTimestamp: devGenesisTS, GasLimit: devGasLimit},
		GenesisHash: genesis.Hash.Hex(),
		Calls:       calls,
	}

	encoded, err := json.MarshalIndent(built, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(transcriptPath), append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d calls)", transcriptPath, len(calls))
}

func readTranscript(t *testing.T) *transcriptFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(transcriptPath))
	if err != nil {
		t.Fatalf("%v — run `go test ./integration -run TestEngineTranscript -update`", err)
	}
	var file transcriptFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	return &file
}

// replayTranscript sends the recorded requests to an engine and compares the
// answers. Whether that engine is this process or one named by
// OP_CARTESI_TEST_ENGINE_URL is the usual choice; either way it must be
// running the machine and parameters the transcript names, so a node serving
// a different chain is refused rather than reported as non-conforming.
func replayTranscript(t *testing.T, file *transcriptFile) {
	t.Helper()
	ctx := context.Background()
	h := transcriptTarget(t)

	chainID, err := h.node.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if chainID != file.Params.ChainID {
		t.Skipf("the transcript is for chain %d; this engine serves %d", file.Params.ChainID, chainID)
	}
	genesis, err := h.node.BlockByTag(ctx, "earliest")
	if err != nil {
		t.Fatal(err)
	}
	if genesis.Hash.Hex() != file.GenesisHash {
		t.Skipf("the transcript is against genesis %s; this engine serves %s — the same chain parameters over a different machine",
			file.GenesisHash, genesis.Hash)
	}
	head, err := h.node.BlockByTag(ctx, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if head.Number != 0 {
		t.Skipf("the transcript replays from genesis; this engine is already at block %d", uint64(head.Number))
	}

	var payloadID json.RawMessage
	for i, call := range file.Calls {
		params := call.Params
		if call.PayloadIDIn {
			if payloadID == nil {
				t.Fatalf("call %d asks for a payload id no earlier call returned", i)
			}
			params = []json.RawMessage{payloadID}
		}
		args := make([]any, 0, len(params))
		for _, p := range params {
			args = append(args, p)
		}
		var got json.RawMessage
		if err := h.rawCall(ctx, &got, call.Method, args...); err != nil {
			t.Fatalf("call %d (%s — %s): %v", i, call.Method, call.Note, err)
		}
		if call.PayloadIDOut {
			payloadID = takePayloadID(t, i, got)
			got = blankPayloadID(t, got)
		}
		if !equalJSON(t, got, call.Result) {
			t.Errorf("call %d (%s — %s):\n  got  %s\n  want %s", i, call.Method, call.Note, got, call.Result)
		}
	}
}

func takePayloadID(t *testing.T, i int, result json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result, &fields); err != nil {
		t.Fatalf("call %d: result is not an object: %v", i, err)
	}
	id, ok := fields["payloadId"]
	if !ok || string(id) == "null" {
		t.Fatalf("call %d: the engine returned no payload id for a build request", i)
	}
	return id
}

func blankPayloadID(t *testing.T, result json.RawMessage) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result, &fields); err != nil {
		t.Fatal(err)
	}
	fields["payloadId"] = json.RawMessage(`null`)
	blanked, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return blanked
}

// equalJSON compares two documents by value rather than by their bytes. Key
// order, integer formatting, and a null against an absent field are not part
// of the contract: op-node decodes these into typed structs, where a field
// that is null and one that is missing are the same thing.
func equalJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, err := json.Marshal(dropNulls(x))
	if err != nil {
		return false
	}
	by, err := json.Marshal(dropNulls(y))
	if err != nil {
		return false
	}
	return string(ax) == string(by)
}

// dropNulls removes null-valued object members, recursively.
func dropNulls(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, member := range value {
			if member == nil {
				continue
			}
			out[k] = dropNulls(member)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, member := range value {
			out[i] = dropNulls(member)
		}
		return out
	default:
		return v
	}
}
