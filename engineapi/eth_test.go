package engineapi

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
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
