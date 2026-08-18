package chain

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// encodeEvmLogOutput builds the guest's event wire form: a Notice wrapping
// the EvmLog call encoding.
func encodeEvmLogOutput(t *testing.T, emitter common.Address, topics []common.Hash, data []byte) []byte {
	t.Helper()
	raw := make([][32]byte, len(topics))
	for i, topic := range topics {
		raw[i] = topic
	}
	inner, err := evmLogArgs.Pack(emitter, raw, data)
	if err != nil {
		t.Fatal(err)
	}
	payload := append(append([]byte{}, evmLogSelector...), inner...)
	outer, err := noticeArgs.Pack(payload)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte{}, noticeSelector...), outer...)
}

// A guest event must surface in the receipt as a real log — the guest's own
// emitter and topics — while a non-event output keeps the CartesiOutput
// form. This is what makes event-matching tooling (viem's getWithdrawals
// over MessagePassed, Transfer indexers) read these receipts unchanged.
func TestReceiptDecodesGuestEvents(t *testing.T) {
	emitter := common.HexToAddress("0x4200000000000000000000000000000000000016")
	topics := []common.Hash{
		crypto.Keccak256Hash([]byte("MessagePassed(uint256,address,address,uint256,uint256,bytes,bytes32)")),
		common.HexToHash("0x01"),
	}
	payload := []byte{0xaa, 0xbb}

	m := machine.NewMock([]byte("evmlog"))
	var event []byte
	m.OutputFn = func([]byte) []machine.Output {
		return []machine.Output{
			{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: event},
			{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: []byte("opaque output")},
		}
	}
	event = encodeEvmLogOutput(t, emitter, topics, payload)
	c, err := New(context.Background(), testConfig(), m, mempool.New(8))
	if err != nil {
		t.Fatal(err)
	}
	env := buildBlock(c, t, isthmusAttrs(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}))

	receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
	if len(receipts) != 1 || len(receipts[0].Logs) != 2 {
		t.Fatalf("want 1 receipt with 2 logs, got %d receipts", len(receipts))
	}
	decoded := receipts[0].Logs[0]
	if decoded.Address != emitter {
		t.Errorf("log emitter %s, want %s", decoded.Address, emitter)
	}
	if len(decoded.Topics) != 2 || decoded.Topics[0] != topics[0] || decoded.Topics[1] != topics[1] {
		t.Errorf("log topics %v, want %v", decoded.Topics, topics)
	}
	if !bytes.Equal(decoded.Data, payload) {
		t.Errorf("log data %x, want %x", decoded.Data, payload)
	}
	raw := receipts[0].Logs[1]
	if raw.Address != OutputsEmitterAddress || raw.Topics[0] != OutputEventTopic {
		t.Errorf("non-event output lost its CartesiOutput form: %v", raw)
	}
	if !bytes.Equal(raw.Data, []byte("opaque output")) {
		t.Errorf("raw output data %q", raw.Data)
	}
}
