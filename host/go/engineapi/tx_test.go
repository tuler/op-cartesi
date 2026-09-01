package engineapi

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// eth_getTransactionByHash is what viem's waitForTransactionReceipt calls
// before it ever polls for a receipt, so every script's first L2 wait dies
// without it. A mined transaction answers with its block coordinates and
// sender; a pooled one with explicit nulls; an unknown hash with null.
func TestEthGetTransactionByHash(t *testing.T) {
	client, c := newPublicServer(t)
	payload := sequenceOneBlock(t, c)

	// The block's one transaction is the deposit sequenceOneBlock injects.
	deposit := new(types.Transaction)
	if err := deposit.UnmarshalBinary(testDeposit(t)); err != nil {
		t.Fatal(err)
	}

	var mined map[string]json.RawMessage
	client.call("eth_getTransactionByHash", &mined, deposit.Hash())
	if mined == nil {
		t.Fatal("the mined deposit was not found")
	}
	assertField := func(field, want string) {
		t.Helper()
		var got string
		if err := json.Unmarshal(mined[field], &got); err != nil {
			t.Fatalf("%s: %v (raw %s)", field, err, mined[field])
		}
		if got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	assertField("hash", deposit.Hash().Hex())
	assertField("blockHash", payload.BlockHash.Hex())
	assertField("from", "0x000000000000000000000000000000000000dead")
	assertField("transactionIndex", "0x0")
	var number string
	_ = json.Unmarshal(mined["blockNumber"], &number)
	if number != "0x1" {
		t.Errorf("blockNumber = %s, want 0x1", number)
	}

	// A pooled transaction: accepted by eth_sendRawTransaction, not yet in a
	// block — served with null block coordinates, geth's pending shape.
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := types.LatestSignerForChainID(big.NewInt(901))
	to := common.HexToAddress("0xbeef")
	signed, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID:   big.NewInt(901),
		Nonce:     0,
		GasTipCap: new(big.Int),
		GasFeeCap: big.NewInt(1),
		Gas:       21000,
		To:        &to,
		Value:     big.NewInt(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var txHash string
	client.call("eth_sendRawTransaction", &txHash, rawHex(raw))
	var pending map[string]json.RawMessage
	client.call("eth_getTransactionByHash", &pending, signed.Hash())
	if pending == nil {
		t.Fatal("the pooled transaction was not found")
	}
	if string(pending["blockHash"]) != "null" {
		t.Errorf("pending blockHash = %s, want null", pending["blockHash"])
	}
	var from string
	if err := json.Unmarshal(pending["from"], &from); err != nil {
		t.Fatal(err)
	}
	if want := crypto.PubkeyToAddress(key.PublicKey); from != want.Hex() &&
		common.HexToAddress(from) != want {
		t.Errorf("pending from = %s, want %s", from, want)
	}

	// An unknown hash is null, not an error — the shape viem's
	// TransactionNotFoundError handling expects.
	var missing map[string]json.RawMessage
	client.call("eth_getTransactionByHash", &missing, crypto.Keccak256Hash([]byte("nope")))
	if missing != nil {
		t.Errorf("unknown hash answered %v, want null", missing)
	}
}

// rawHex renders raw bytes as the 0x-prefixed string the RPC takes.
func rawHex(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2+len(raw)*2)
	out[0], out[1] = '0', 'x'
	for i, b := range raw {
		out[2+i*2] = digits[b>>4]
		out[3+i*2] = digits[b&0x0f]
	}
	return string(out)
}
