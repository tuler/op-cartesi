package engineapi

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/tuler/op-cartesi/chain"
)

// A client that rebuilds a header from what eth_getBlockByNumber serves must
// arrive at the hash we reported. op-batcher does exactly this to chain blocks
// together, and a single omitted header field makes it reject the chain — which
// is how a missing requestsHash was found, only once a real batcher ran.
func TestServedBlockRehashesToTheSameHash(t *testing.T) {
	client, c := newPublicServer(t)
	payload := sequenceOneBlock(t, c)

	var raw map[string]json.RawMessage
	client.call("eth_getBlockByNumber", &raw, "latest", false)
	if raw == nil {
		t.Fatal("no block returned")
	}

	get := func(key string, out any) {
		t.Helper()
		v, ok := raw[key]
		if !ok {
			t.Fatalf("served block omits %q, so its hash cannot be reconstructed", key)
		}
		if err := json.Unmarshal(v, out); err != nil {
			t.Fatalf("decoding %q: %v", key, err)
		}
	}

	var (
		parentHash, uncleHash, stateRoot, txRoot, receiptsRoot common.Hash
		withdrawalsRoot, beaconRoot, requestsHash, mixHash     common.Hash
		coinbase                                               common.Address
		bloom                                                  types.Bloom
		nonce                                                  types.BlockNonce
		number, difficulty, baseFee                            hexutil.Big
		gasLimit, gasUsed, timestamp                           hexutil.Uint64
		blobGasUsed, excessBlobGas                             hexutil.Uint64
		extra                                                  hexutil.Bytes
	)
	get("parentHash", &parentHash)
	get("sha3Uncles", &uncleHash)
	get("miner", &coinbase)
	get("stateRoot", &stateRoot)
	get("transactionsRoot", &txRoot)
	get("receiptsRoot", &receiptsRoot)
	get("logsBloom", &bloom)
	get("difficulty", &difficulty)
	get("number", &number)
	get("gasLimit", &gasLimit)
	get("gasUsed", &gasUsed)
	get("timestamp", &timestamp)
	get("extraData", &extra)
	get("mixHash", &mixHash)
	get("nonce", &nonce)
	get("baseFeePerGas", &baseFee)
	get("withdrawalsRoot", &withdrawalsRoot)
	get("blobGasUsed", &blobGasUsed)
	get("excessBlobGas", &excessBlobGas)
	get("parentBeaconBlockRoot", &beaconRoot)
	get("requestsHash", &requestsHash)

	blob, excess := uint64(blobGasUsed), uint64(excessBlobGas)
	header := &types.Header{
		ParentHash:       parentHash,
		UncleHash:        uncleHash,
		Coinbase:         coinbase,
		Root:             stateRoot,
		TxHash:           txRoot,
		ReceiptHash:      receiptsRoot,
		Bloom:            bloom,
		Difficulty:       (*big.Int)(&difficulty),
		Number:           (*big.Int)(&number),
		GasLimit:         uint64(gasLimit),
		GasUsed:          uint64(gasUsed),
		Time:             uint64(timestamp),
		Extra:            extra,
		MixDigest:        mixHash,
		Nonce:            nonce,
		BaseFee:          (*big.Int)(&baseFee),
		WithdrawalsHash:  &withdrawalsRoot,
		BlobGasUsed:      &blob,
		ExcessBlobGas:    &excess,
		ParentBeaconRoot: &beaconRoot,
		RequestsHash:     &requestsHash,
	}
	if got := header.Hash(); got != payload.BlockHash {
		t.Fatalf("rebuilt header hashes to %s, but the block was served as %s", got, payload.BlockHash)
	}
}

// The gas methods exist so a plain `cast send` needs no gas flags: cast asks
// eth_gasPrice for the price and eth_estimateGas for the limit, and wallets
// read eth_feeHistory. There is no fee market, so the honest answers are the
// header's constant base fee and a zero tip.
func TestGasPriceIsTheHeadBaseFeeWithZeroTip(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	var price hexutil.Big
	client.call("eth_gasPrice", &price)
	want := c.HeadBlock().Header.BaseFee
	if (*big.Int)(&price).Cmp(want) != 0 {
		t.Errorf("eth_gasPrice = %s, want the head base fee %s", (*big.Int)(&price), want)
	}

	var tip hexutil.Big
	client.call("eth_maxPriorityFeePerGas", &tip)
	if (*big.Int)(&tip).Sign() != 0 {
		t.Errorf("eth_maxPriorityFeePerGas = %s, want 0: there is no fee market to tip into", (*big.Int)(&tip))
	}
}

// eth_feeHistory must match go-ethereum's wire shape exactly — cast and
// wallets parse it strictly: oldestBlock, a baseFeePerGas array one longer
// than the window (the extra entry projects the next block), gasUsedRatio from
// header gasUsed/gasLimit, and a reward matrix when percentiles are requested.
func TestFeeHistoryMatchesTheGethWireShape(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)
	head := c.HeadBlock().Header

	var raw map[string]json.RawMessage
	client.call("eth_feeHistory", &raw, "0x2", "latest", []float64{25, 75})
	for _, key := range []string{"oldestBlock", "baseFeePerGas", "gasUsedRatio", "reward"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("fee history omits %q, which strict clients require", key)
		}
	}
	decode := func(key string, out any) {
		t.Helper()
		if err := json.Unmarshal(raw[key], out); err != nil {
			t.Fatalf("decoding %q: %v", key, err)
		}
	}

	var oldest hexutil.Big
	decode("oldestBlock", &oldest)
	if (*big.Int)(&oldest).Sign() != 0 {
		t.Errorf("oldestBlock = %s, want 0: two blocks ending at the head start at genesis", (*big.Int)(&oldest))
	}

	var baseFees []hexutil.Big
	decode("baseFeePerGas", &baseFees)
	if len(baseFees) != 3 {
		t.Fatalf("%d baseFeePerGas entries, want 2 blocks + 1 next-block projection", len(baseFees))
	}
	for i := range baseFees {
		if (*big.Int)(&baseFees[i]).Cmp(head.BaseFee) != 0 {
			t.Errorf("baseFeePerGas[%d] = %s, want the constant %s", i, (*big.Int)(&baseFees[i]), head.BaseFee)
		}
	}

	var ratios []float64
	decode("gasUsedRatio", &ratios)
	if len(ratios) != 2 {
		t.Fatalf("%d gasUsedRatio entries, want one per block", len(ratios))
	}
	if ratios[0] != 0 {
		t.Errorf("genesis gasUsedRatio = %v, want 0", ratios[0])
	}
	if want := float64(head.GasUsed) / float64(head.GasLimit); ratios[1] != want {
		t.Errorf("head gasUsedRatio = %v, want %v from the header", ratios[1], want)
	}
	if ratios[1] == 0 {
		t.Error("head gasUsedRatio is 0, but the deposit consumed cycles")
	}

	var reward [][]hexutil.Big
	decode("reward", &reward)
	if len(reward) != 2 {
		t.Fatalf("%d reward rows, want one per block", len(reward))
	}
	for i, row := range reward {
		if len(row) != 2 {
			t.Fatalf("reward[%d] has %d entries, want one per percentile", i, len(row))
		}
		for j := range row {
			if (*big.Int)(&row[j]).Sign() != 0 {
				t.Errorf("reward[%d][%d] = %s, want 0: nobody pays priority fees", i, j, (*big.Int)(&row[j]))
			}
		}
	}
}

// The fee-history window clamps to the blocks that exist, and the geth error
// behaviors are mirrored: zero blocks is an empty answer, a window ending
// beyond the head is an error, and so are misordered percentiles.
func TestFeeHistoryClampsToAvailableBlocks(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	// Far more blocks than exist: the answer covers genesis through the head.
	var res struct {
		OldestBlock  hexutil.Big     `json:"oldestBlock"`
		Reward       [][]hexutil.Big `json:"reward"`
		BaseFee      []hexutil.Big   `json:"baseFeePerGas"`
		GasUsedRatio []float64       `json:"gasUsedRatio"`
	}
	client.call("eth_feeHistory", &res, "0x400", "latest", []float64{})
	if (*big.Int)(&res.OldestBlock).Sign() != 0 {
		t.Errorf("oldestBlock = %s, want the clamp to land on genesis", (*big.Int)(&res.OldestBlock))
	}
	if len(res.GasUsedRatio) != 2 {
		t.Errorf("%d gasUsedRatio entries, want the 2 blocks that exist", len(res.GasUsedRatio))
	}
	if len(res.BaseFee) != 3 {
		t.Errorf("%d baseFeePerGas entries, want 2 blocks + 1", len(res.BaseFee))
	}
	if res.Reward != nil {
		t.Errorf("reward = %v, want it omitted when no percentiles are requested", res.Reward)
	}

	// Zero blocks requested: geth's empty answer, not an error.
	var empty struct {
		OldestBlock  hexutil.Big   `json:"oldestBlock"`
		BaseFee      []hexutil.Big `json:"baseFeePerGas"`
		GasUsedRatio []float64     `json:"gasUsedRatio"`
	}
	client.call("eth_feeHistory", &empty, "0x0", "latest", []float64{})
	if (*big.Int)(&empty.OldestBlock).Sign() != 0 || empty.BaseFee != nil || empty.GasUsedRatio != nil {
		t.Errorf("zero-count answer = %+v, want the empty result", empty)
	}

	// A window ending beyond the head is an error, as geth reports it.
	if msg := client.callError("eth_feeHistory", "0x1", "0x99", []float64{}).Message; !strings.Contains(msg, "beyond head") {
		t.Errorf("beyond-head error %q does not say so", msg)
	}

	// Misordered percentiles are an error, as geth reports them.
	if msg := client.callError("eth_feeHistory", "0x1", "latest", []float64{75, 25}).Message; !strings.Contains(msg, "percentile") {
		t.Errorf("percentile error %q does not name the percentiles", msg)
	}
}

// eth_estimateGas answers with the chain's per-input cycle budget expressed as
// gas: the upper bound the chain will accept, not a measurement — an unsigned
// estimate cannot replay the guest's enforced sender/nonce path (see the
// method's comment). The bound is what `cast send` adopts as the gas limit
// when no --gas-limit flag is given, so it must fit under the block gas limit.
func TestEstimateGasServesTheInputBudget(t *testing.T) {
	client, c := newPublicServer(t)
	sequenceOneBlock(t, c)

	// The args carry the fields cast sends; from and value are outside
	// CallArgs and must be tolerated, not rejected.
	args := map[string]any{
		"from":  common.HexToAddress("0xa11ce"),
		"to":    common.Address{},
		"data":  hexutil.Bytes("payload"),
		"value": (*hexutil.Big)(big.NewInt(1)),
	}
	var est hexutil.Uint64
	client.call("eth_estimateGas", &est, args)

	want := c.Config().MaxCyclesPerInput / chain.CyclesPerGas
	if uint64(est) != want {
		t.Errorf("eth_estimateGas = %d, want the cycle budget as gas, %d", est, want)
	}
	if limit := c.HeadBlock().Header.GasLimit; uint64(est) > limit {
		t.Errorf("estimate %d exceeds the block gas limit %d; tools would refuse it", est, limit)
	}

	// The block tag resolves like everywhere else in the namespace...
	client.call("eth_estimateGas", &est, args, "latest")
	if uint64(est) != want {
		t.Errorf("estimate at latest = %d, want %d", est, want)
	}
	// ...so a block that does not exist is an error, not an estimate.
	if msg := client.callError("eth_estimateGas", args, "0x99").Message; msg == "" {
		t.Error("estimating against a missing block did not error")
	}
}
