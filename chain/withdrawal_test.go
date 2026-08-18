package chain

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// The guest's TypeScript encoder (@op-cartesi/evm withdrawal.ts) and this
// package's decoder are pinned to each other by a fixed vector: the payload
// below is what encodeWithdrawal produces for the withdrawal below, captured
// from the TS side. A drift in either encoding breaks this test instead of
// the chain.
const tsWithdrawalPayload = "0x0b0e2d670001000000000000000000000000000000000000000000000000002a000000010000000000000000000000001111111111111111111111111111111111111111000000000000000000000000222222222222222222222222222222222222222200000000000000000000000000000000000000000000000000038d7ea4c6800000000000000000000000000000000000000000000000000000000000000186a000000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002dead000000000000000000000000000000000000000000000000000000000000"

func TestWithdrawalEncodingMatchesGuest(t *testing.T) {
	// versionedWithdrawalNonce(42, 1): version 1 << 240 | 42 << 32 | 1.
	nonce := new(big.Int).Lsh(big.NewInt(1), 240)
	nonce.Or(nonce, new(big.Int).Lsh(big.NewInt(42), 32))
	nonce.Or(nonce, big.NewInt(1))
	w := &Withdrawal{
		Nonce:    nonce,
		Sender:   common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Target:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		Value:    big.NewInt(1_000_000_000_000_000),
		GasLimit: big.NewInt(100000),
		Data:     []byte{0xde, 0xad},
	}
	payload := hexutil.MustDecode(tsWithdrawalPayload)

	// The guest wraps the payload in a Notice; reproduce that framing and
	// the decoder must recognize it.
	wrapped, err := noticeArgs.Pack(payload)
	if err != nil {
		t.Fatal(err)
	}
	output := append(append([]byte{}, noticeSelector...), wrapped...)
	parsed, ok := ParseWithdrawalOutput(output)
	if !ok {
		t.Fatal("the guest's encoding did not parse as a withdrawal")
	}
	if parsed.Nonce.Cmp(w.Nonce) != 0 || parsed.Sender != w.Sender || parsed.Target != w.Target ||
		parsed.Value.Cmp(w.Value) != 0 || parsed.GasLimit.Cmp(w.GasLimit) != 0 || !bytes.Equal(parsed.Data, w.Data) {
		t.Errorf("parsed %+v, want %+v", parsed, w)
	}

	// And this package's encoder produces the identical bytes.
	encoded, err := w.EncodeOutput()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, output) {
		t.Errorf("EncodeOutput diverges from the guest encoding:\n  go %x\n  ts %x", encoded, output)
	}
}
