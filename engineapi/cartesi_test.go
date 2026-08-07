package engineapi

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/tuler/op-cartesi/chain"
	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// newPublicServer builds the unauthenticated endpoint users talk to, backed by
// a machine that emits one provable output and one report per input.
func newPublicServer(t *testing.T) (*rpcClient, *chain.Chain) {
	t.Helper()
	m := machine.NewMock([]byte("cartesi-api-test"))
	m.OutputFn = func(input []byte) []machine.Output {
		return []machine.Output{
			{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: []byte("voucher payload")},
			{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: []byte("why it happened")},
		}
	}
	// Tagged like the routed guest answers (chain.ReportTag*): a return-
	// tagged echo, so eth_call strips the tag and cartesi_inspect shows it.
	m.InspectFn = func(query []byte) [][]byte {
		if bytes.Contains(query, []byte("REVERTME")) {
			return [][]byte{append([]byte{chain.ReportTagRevert}, revertData("nope")...)}
		}
		return [][]byte{append(append([]byte{chain.ReportTagReturn}, []byte("answer:")...), query...)}
	}
	m.RejectFn = func(query []byte) bool { return bytes.Contains(query, []byte("REVERTME")) }
	pool := mempool.New(64)
	c, err := chain.New(context.Background(), chain.Config{ChainID: 901, GenesisTimestamp: 1000}, m, pool)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(c, pool, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &rpcClient{t: t, url: srv.URL}, c
}

// sequenceOneBlock drives a single block through the chain directly, which is
// enough to give the RPC layer something to report on.
func sequenceOneBlock(t *testing.T, c *chain.Chain) *engine.ExecutableData {
	t.Helper()
	ctx := context.Background()
	genesis := c.HeadBlock()
	gasLimit := uint64(30_000_000)
	beaconRoot := common.HexToHash("0xbeac0")
	attrs := &engine.PayloadAttributes{
		Timestamp:             genesis.Time() + 2,
		Random:                common.HexToHash("0x1"),
		SuggestedFeeRecipient: common.HexToAddress("0x42"),
		Withdrawals:           []*types.Withdrawal{},
		BeaconRoot:            &beaconRoot,
		Transactions:          [][]byte{testDeposit(t)},
		NoTxPool:              true,
		GasLimit:              &gasLimit,
		EIP1559Params:         make([]byte, 8),
	}
	fc := engine.ForkchoiceStateV1{HeadBlockHash: genesis.Hash(), SafeBlockHash: genesis.Hash(), FinalizedBlockHash: genesis.Hash()}
	resp, err := c.ForkchoiceUpdated(ctx, fc, attrs)
	if err != nil {
		t.Fatal(err)
	}
	env, ok := c.Payload(*resp.PayloadID)
	if !ok {
		t.Fatal("payload not found")
	}
	if _, err := c.ImportPayload(ctx, env.ExecutionPayload, env.ParentBeaconBlockRoot); err != nil {
		t.Fatal(err)
	}
	fc.HeadBlockHash = env.ExecutionPayload.BlockHash
	if _, err := c.ForkchoiceUpdated(ctx, fc, nil); err != nil {
		t.Fatal(err)
	}
	return env.ExecutionPayload
}

// A receipt served over RPC must have the standard shape, with the machine's
// outputs appearing as logs.
func TestGetTransactionReceiptOverRPC(t *testing.T) {
	client, c := newPublicServer(t)
	payload := sequenceOneBlock(t, c)

	var tx types.Transaction
	if err := tx.UnmarshalBinary(payload.Transactions[0]); err != nil {
		t.Fatal(err)
	}

	var receipt map[string]json.RawMessage
	client.call("eth_getTransactionReceipt", &receipt, tx.Hash())
	if receipt == nil {
		t.Fatal("no receipt returned")
	}
	for _, field := range []string{
		"transactionHash", "transactionIndex", "blockHash", "blockNumber",
		"from", "to", "status", "gasUsed", "cumulativeGasUsed", "logs", "logsBloom",
	} {
		if _, ok := receipt[field]; !ok {
			t.Errorf("receipt is missing standard field %q", field)
		}
	}
	if string(receipt["status"]) != `"0x1"` {
		t.Errorf("status %s, want 0x1", receipt["status"])
	}

	var logs []struct {
		Address common.Address `json:"address"`
		Topics  []common.Hash  `json:"topics"`
		Data    hexutil.Bytes  `json:"data"`
	}
	if err := json.Unmarshal(receipt["logs"], &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("%d logs, want 1", len(logs))
	}
	if string(logs[0].Data) != "voucher payload" {
		t.Errorf("log data %q, want the raw output bytes", logs[0].Data)
	}
	if len(logs[0].Topics) != 2 {
		t.Fatalf("%d topics, want the event signature and the output index", len(logs[0].Topics))
	}
	// The report must not have leaked into the logs: it is not provable.
	if string(logs[0].Data) == "why it happened" {
		t.Error("a report was reported as a log")
	}
}

// The cartesi namespace exposes what a receipt cannot: reports, and the output
// index an on-chain proof needs.
func TestCartesiGetTransactionEmissions(t *testing.T) {
	client, c := newPublicServer(t)
	payload := sequenceOneBlock(t, c)

	var tx types.Transaction
	if err := tx.UnmarshalBinary(payload.Transactions[0]); err != nil {
		t.Fatal(err)
	}

	var emissions Emissions
	client.call("cartesi_getTransactionEmissions", &emissions, tx.Hash())
	if !emissions.Accepted {
		t.Error("emissions report the input as rejected")
	}
	if emissions.Cycles == 0 {
		t.Error("no cycles reported")
	}
	if len(emissions.Outputs) != 1 {
		t.Fatalf("%d outputs, want 1", len(emissions.Outputs))
	}
	if emissions.Outputs[0].Index != 0 {
		t.Errorf("first output index %d, want 0", emissions.Outputs[0].Index)
	}
	if string(emissions.Outputs[0].Data) != "voucher payload" {
		t.Errorf("output data %q", emissions.Outputs[0].Data)
	}
	if len(emissions.Reports) != 1 || string(emissions.Reports[0]) != "why it happened" {
		t.Errorf("reports %q, want the diagnostic preserved", emissions.Reports)
	}
}

// The outputs root served over RPC must be the one the block committed to.
func TestCartesiGetOutputsRoot(t *testing.T) {
	client, c := newPublicServer(t)
	payload := sequenceOneBlock(t, c)

	var root OutputsRoot
	client.call("cartesi_getOutputsRoot", &root, "latest")
	if root.BlockHash != payload.BlockHash {
		t.Errorf("root reported for %s, want %s", root.BlockHash, payload.BlockHash)
	}
	if root.Count != 1 {
		t.Errorf("output count %d, want 1", root.Count)
	}
	if payload.WithdrawalsRoot == nil || root.Root != *payload.WithdrawalsRoot {
		t.Errorf("served root %s, but the header committed %v", root.Root, payload.WithdrawalsRoot)
	}
}

// revertData ABI-encodes Error(string), the require-style revert payload.
func revertData(reason string) []byte {
	stringTy, err := abi.NewType("string", "", nil)
	if err != nil {
		panic(err)
	}
	packed, err := (abi.Arguments{{Type: stringTy}}).Pack(reason)
	if err != nil {
		panic(err)
	}
	return append([]byte{0x08, 0xc3, 0x79, 0xa0}, packed...)
}

// eth_call wraps the query in the EvmCall envelope, runs inspect, and strips
// the return tag from the guest's report.
func TestEthCallRunsInspect(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	headBefore := c.HeadBlock().Hash()

	from := common.HexToAddress("0xabcd")
	to := common.HexToAddress("0x1234")

	var result hexutil.Bytes
	client.call("eth_call", &result, map[string]any{
		"from":  from,
		"to":    to,
		"value": (*hexutil.Big)(big.NewInt(7)),
		"data":  hexutil.Bytes("ping"),
	}, "latest")
	// The mock echoes its query, so the result doubles as proof of the
	// envelope the shim built: EvmCall(chainId, from, to, value, data).
	envelope, err := chain.EncodeEvmCall(901, from, to, big.NewInt(7), []byte("ping"))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("answer:"), envelope...)
	if !bytes.Equal(result, want) {
		t.Errorf("eth_call returned %x, want the tag-stripped echo of the EvmCall envelope", result)
	}

	// The same raw payload through the cartesi namespace passes through
	// untouched — no envelope, and the framing tag stays visible.
	var inspect InspectResult
	client.call("cartesi_inspect", &inspect, hexutil.Bytes("ping"), "latest")
	if !inspect.Accepted {
		t.Error("inspect was rejected")
	}
	wantRaw := append([]byte{chain.ReportTagReturn}, []byte("answer:ping")...)
	if len(inspect.Reports) != 1 || !bytes.Equal(inspect.Reports[0], wantRaw) {
		t.Errorf("inspect reports %q", inspect.Reports)
	}

	if c.HeadBlock().Hash() != headBefore {
		t.Error("a read-only query moved the chain head")
	}
}

// A rejected inspect with a revert-tagged report becomes the standard
// Ethereum revert error: code 3, the decoded Error(string) reason in the
// message, and the raw revert bytes in data — which is what lets viem and
// cast surface require-style messages.
func TestEthCallRevertCarriesData(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	rpcErr := client.callError("eth_call", map[string]any{
		"to":   common.HexToAddress("0x1234"),
		"data": hexutil.Bytes("REVERTME"),
	}, "latest")
	if rpcErr.Code != 3 {
		t.Errorf("revert error code %d, want 3", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "execution reverted: nope") {
		t.Errorf("revert message %q, want the decoded Error(string) reason", rpcErr.Message)
	}
	var data hexutil.Bytes
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil {
		t.Fatalf("revert data is not hex bytes: %v", err)
	}
	if !bytes.Equal(data, revertData("nope")) {
		t.Errorf("revert data %x, want the Error(string) encoding", data)
	}
}

// eth_call without a target has nothing to route: there is no contract
// creation to simulate on this chain.
func TestEthCallRequiresTo(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)
	rpcErr := client.callError("eth_call", map[string]any{"data": hexutil.Bytes("ping")}, "latest")
	if !strings.Contains(rpcErr.Message, "requires 'to'") {
		t.Errorf("error %q, want the missing-to explanation", rpcErr.Message)
	}
}

// The cartesi namespace must be reachable from the public endpoint, since it
// exists for users rather than for op-node.
func TestCartesiNamespaceOnPublicEndpoint(t *testing.T) {
	client, _ := newPublicServer(t)
	var chainID string
	client.call("eth_chainId", &chainID)
	if chainID != "0x385" {
		t.Fatalf("eth_chainId = %s", chainID)
	}
	var root OutputsRoot
	client.call("cartesi_getOutputsRoot", &root, "latest")
	if root.Root == (common.Hash{}) {
		t.Error("genesis outputs root is the zero hash, want the empty-tree root")
	}
}
