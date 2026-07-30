package engineapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

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
	m.InspectFn = func(query []byte) [][]byte {
		return [][]byte{append([]byte("answer:"), query...)}
	}
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

// eth_call runs the machine's inspect protocol and returns the reports.
func TestEthCallRunsInspect(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	headBefore := c.HeadBlock().Hash()

	var result hexutil.Bytes
	client.call("eth_call", &result, map[string]any{
		"to":   common.HexToAddress("0x1234"),
		"data": hexutil.Bytes("ping"),
	}, "latest")
	if string(result) != "answer:ping" {
		t.Errorf("eth_call returned %q, want the inspect report", result)
	}

	// And the same query through the cartesi namespace, which keeps the
	// reports separate rather than concatenating them.
	var inspect InspectResult
	client.call("cartesi_inspect", &inspect, hexutil.Bytes("ping"), "latest")
	if !inspect.Accepted {
		t.Error("inspect was rejected")
	}
	if len(inspect.Reports) != 1 || string(inspect.Reports[0]) != "answer:ping" {
		t.Errorf("inspect reports %q", inspect.Reports)
	}

	if c.HeadBlock().Hash() != headBefore {
		t.Error("a read-only query moved the chain head")
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
