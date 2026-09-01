package engineapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/tuler/op-cartesi/host/go/chain"
	"github.com/tuler/op-cartesi/host/go/machine"
	"github.com/tuler/op-cartesi/host/go/mempool"
)

type rpcClient struct {
	t      *testing.T
	url    string
	secret []byte
	nextID int
}

func (c *rpcClient) call(method string, result any, params ...any) *json.RawMessage {
	c.t.Helper()
	c.nextID++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != nil {
		req.Header.Set("Authorization", "Bearer "+makeJWT(c.t, c.secret, time.Now()))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("%s: http %d", method, resp.StatusCode)
	}
	var rr struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		c.t.Fatal(err)
	}
	if rr.Error != nil {
		c.t.Fatalf("%s: rpc error %d: %s", method, rr.Error.Code, rr.Error.Message)
	}
	if result != nil {
		if err := json.Unmarshal(rr.Result, result); err != nil {
			c.t.Fatalf("%s: decoding result: %v", method, err)
		}
	}
	return &rr.Result
}

// rpcError is a JSON-RPC error object, for tests asserting the error itself.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// callError performs a call expected to fail and returns the error object.
func (c *rpcClient) callError(method string, params ...any) *rpcError {
	c.t.Helper()
	c.nextID++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != nil {
		req.Header.Set("Authorization", "Bearer "+makeJWT(c.t, c.secret, time.Now()))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var rr struct {
		Error *rpcError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		c.t.Fatal(err)
	}
	if rr.Error == nil {
		c.t.Fatalf("%s: expected an rpc error, got success", method)
	}
	return rr.Error
}

func makeJWT(t *testing.T, secret []byte, at time.Time) string {
	t.Helper()
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	unsigned := enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(map[string]int64{"iat": at.Unix()})
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newTestServer(t *testing.T) (*rpcClient, *chain.Chain) {
	t.Helper()
	m := machine.NewMock([]byte("engine-test"))
	pool := mempool.New(64)
	c, err := chain.New(context.Background(), chain.Config{ChainID: 901, GenesisTimestamp: 1000}, m, pool)
	if err != nil {
		t.Fatal(err)
	}
	secret := crypto.Keccak256([]byte("jwt-secret"))
	handler, err := NewHandler(c, pool, true, secret)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &rpcClient{t: t, url: srv.URL, secret: secret}, c
}

func testDeposit(t *testing.T) []byte {
	t.Helper()
	to := common.HexToAddress("0x4200000000000000000000000000000000000015")
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte("l1-info")),
		From:       common.HexToAddress("0xdead"),
		To:         &to,
		Gas:        1_000_000,
		Value:      new(big.Int),
		Mint:       new(big.Int),
		Data:       []byte("l1 attributes"),
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestEngineRPCSequencingRoundtrip(t *testing.T) {
	client, c := newTestServer(t)
	genesis := c.GenesisHash()

	var chainID string
	client.call("eth_chainId", &chainID)
	if chainID != "0x385" {
		t.Fatalf("eth_chainId = %s, want 0x385", chainID)
	}

	gasLimit := uint64(30_000_000)
	beaconRoot := common.HexToHash("0xbeac0")
	attrs := &engine.PayloadAttributes{
		Timestamp:             1002,
		Random:                common.HexToHash("0x1"),
		SuggestedFeeRecipient: common.HexToAddress("0x42"),
		Withdrawals:           []*types.Withdrawal{},
		BeaconRoot:            &beaconRoot,
		Transactions:          [][]byte{testDeposit(t)},
		NoTxPool:              false,
		GasLimit:              &gasLimit,
		EIP1559Params:         make([]byte, 8),
	}
	fc := engine.ForkchoiceStateV1{HeadBlockHash: genesis, SafeBlockHash: genesis, FinalizedBlockHash: genesis}

	var fcuResp engine.ForkChoiceResponse
	client.call("engine_forkchoiceUpdatedV3", &fcuResp, fc, attrs)
	if fcuResp.PayloadStatus.Status != engine.VALID || fcuResp.PayloadID == nil {
		t.Fatalf("forkchoiceUpdated: %+v", fcuResp)
	}

	var envelope struct {
		ExecutionPayload      engine.ExecutableData `json:"executionPayload"`
		BlockValue            string                `json:"blockValue"`
		ParentBeaconBlockRoot *common.Hash          `json:"parentBeaconBlockRoot"`
	}
	client.call("engine_getPayloadV4", &envelope, *fcuResp.PayloadID)
	payload := envelope.ExecutionPayload
	if payload.Number != 1 || len(payload.Transactions) != 1 {
		t.Fatalf("unexpected payload: number=%d txs=%d", payload.Number, len(payload.Transactions))
	}
	if envelope.ParentBeaconBlockRoot == nil || *envelope.ParentBeaconBlockRoot != beaconRoot {
		t.Fatalf("envelope missing parentBeaconBlockRoot")
	}

	var status engine.PayloadStatusV1
	client.call("engine_newPayloadV4", &status, payload, []common.Hash{}, beaconRoot, []hexutil.Bytes{})
	if status.Status != engine.VALID {
		t.Fatalf("newPayload: %+v", status)
	}

	fc.HeadBlockHash = payload.BlockHash
	client.call("engine_forkchoiceUpdatedV3", &fcuResp, fc, nil)
	if fcuResp.PayloadStatus.Status != engine.VALID {
		t.Fatalf("head-advance forkchoiceUpdated: %+v", fcuResp)
	}

	var block map[string]json.RawMessage
	client.call("eth_getBlockByNumber", &block, "latest", true)
	if string(block["number"]) != `"0x1"` {
		t.Fatalf("latest block number = %s, want 0x1", block["number"])
	}
	var txs []json.RawMessage
	if err := json.Unmarshal(block["transactions"], &txs); err != nil || len(txs) != 1 {
		t.Fatalf("bad transactions field: %s", block["transactions"])
	}
	var parsed types.Transaction
	if err := json.Unmarshal(txs[0], &parsed); err != nil {
		t.Fatalf("block tx does not round-trip through types.Transaction: %v", err)
	}
	if parsed.Type() != types.DepositTxType {
		t.Fatalf("tx type %d, want deposit", parsed.Type())
	}
}

func TestJWTEnforcement(t *testing.T) {
	client, _ := newTestServer(t)

	post := func(auth string) int {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
		req, err := http.NewRequest(http.MethodPost, client.url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("missing token: http %d, want 401", code)
	}
	if code := post("Bearer garbage"); code != http.StatusUnauthorized {
		t.Fatalf("bad token: http %d, want 401", code)
	}
	stale := makeJWT(t, client.secret, time.Now().Add(-10*time.Minute))
	if code := post("Bearer " + stale); code != http.StatusUnauthorized {
		t.Fatalf("stale token: http %d, want 401", code)
	}
	wrongKey := makeJWT(t, []byte("wrong-secret-wrong-secret-wrong!"), time.Now())
	if code := post("Bearer " + wrongKey); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: http %d, want 401", code)
	}
	if code := post("Bearer " + makeJWT(t, client.secret, time.Now())); code != http.StatusOK {
		t.Fatalf("valid token: http %d, want 200", code)
	}
}
