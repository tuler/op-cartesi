package chain

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

const testChainID = 901

func testConfig() Config {
	return Config{ChainID: testChainID, GenesisTimestamp: 1000, MaxSnapshots: 8}
}

func newTestChain(t *testing.T, seed string) (*Chain, *mempool.Pool, *machine.Mock) {
	t.Helper()
	return newTestChainWithConfig(t, seed, testConfig())
}

func newTestChainWithConfig(t *testing.T, seed string, cfg Config) (*Chain, *mempool.Pool, *machine.Mock) {
	t.Helper()
	m := machine.NewMock([]byte(seed))
	pool := mempool.New(64)
	c, err := New(context.Background(), cfg, m, pool)
	if err != nil {
		t.Fatal(err)
	}
	return c, pool, m
}

func depositTx(t *testing.T, note string) []byte {
	t.Helper()
	to := common.HexToAddress("0x4200000000000000000000000000000000000015")
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte(note)),
		From:       common.HexToAddress("0xdead"),
		To:         &to,
		Gas:        1_000_000,
		Value:      new(big.Int),
		Mint:       new(big.Int),
		Data:       []byte(note),
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func userTx(t *testing.T, nonce uint64, data []byte) []byte {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("op-cartesi test key")))
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x1234")
	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(testChainID)), &types.DynamicFeeTx{
		ChainID:   big.NewInt(testChainID),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       1_000_000,
		To:        &to,
		Data:      data,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func attrsOn(parent *Block, deposits [][]byte, noTxPool bool) *engine.PayloadAttributes {
	gasLimit := uint64(30_000_000)
	beaconRoot := common.HexToHash("0xbeac0")
	return &engine.PayloadAttributes{
		Timestamp:             parent.Time() + 2,
		Random:                common.HexToHash("0x1"),
		SuggestedFeeRecipient: common.HexToAddress("0x4200000000000000000000000000000000000011"),
		Withdrawals:           []*types.Withdrawal{},
		BeaconRoot:            &beaconRoot,
		Transactions:          deposits,
		NoTxPool:              noTxPool,
		GasLimit:              &gasLimit,
	}
}

func fcu(c *Chain, t *testing.T, head common.Hash, attrs *engine.PayloadAttributes) *engine.ForkChoiceResponse {
	t.Helper()
	resp, err := c.ForkchoiceUpdated(context.Background(), engine.ForkchoiceStateV1{
		HeadBlockHash:      head,
		SafeBlockHash:      c.GenesisHash(),
		FinalizedBlockHash: c.GenesisHash(),
	}, attrs)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// buildBlock drives the full sequencer path: FCU with attributes, getPayload,
// newPayload, FCU to the new head.
func buildBlock(c *Chain, t *testing.T, attrs *engine.PayloadAttributes) *engine.ExecutionPayloadEnvelope {
	t.Helper()
	ctx := context.Background()
	resp := fcu(c, t, c.HeadBlock().Hash(), attrs)
	if resp.PayloadID == nil {
		t.Fatalf("no payload id, status %s", resp.PayloadStatus.Status)
	}
	env, ok := c.Payload(*resp.PayloadID)
	if !ok {
		t.Fatal("payload not found")
	}
	st, err := c.ImportPayload(ctx, env.ExecutionPayload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("newPayload status %s (%v)", st.Status, st.ValidationError)
	}
	fcu(c, t, env.ExecutionPayload.BlockHash, nil)
	return env
}

func TestGenesisDeterminism(t *testing.T) {
	a, _, _ := newTestChain(t, "seed")
	b, _, _ := newTestChain(t, "seed")
	if a.GenesisHash() != b.GenesisHash() {
		t.Fatalf("same seed, different genesis: %s vs %s", a.GenesisHash(), b.GenesisHash())
	}
	other, _, _ := newTestChain(t, "other-seed")
	if a.GenesisHash() == other.GenesisHash() {
		t.Fatal("different machine state produced the same genesis hash")
	}
}

func TestBuildImportRoundtrip(t *testing.T) {
	sequencer, pool, _ := newTestChain(t, "seed")

	if _, err := pool.Add(userTx(t, 0, []byte("hello"))); err != nil {
		t.Fatal(err)
	}
	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, false))
	payload := env.ExecutionPayload

	if len(payload.Transactions) != 2 {
		t.Fatalf("expected deposit + user tx in block, got %d txs", len(payload.Transactions))
	}
	if sequencer.HeadBlock().NumberU64() != 1 {
		t.Fatalf("head not advanced: %d", sequencer.HeadBlock().NumberU64())
	}
	if pool.Len() != 0 {
		t.Fatalf("included tx still in pool")
	}
	if payload.GasUsed == 0 {
		t.Fatal("expected nonzero gasUsed from cycle metering")
	}

	// A verifier with the same genesis machine must re-derive the same block
	// from the payload alone (the derivation-from-L1 property).
	verifier, _, _ := newTestChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), payload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("verifier rejected payload: %s (%v)", st.Status, st.ValidationError)
	}
	fcu(verifier, t, payload.BlockHash, nil)
	if verifier.HeadBlock().Hash() != sequencer.HeadBlock().Hash() {
		t.Fatal("verifier and sequencer disagree on head")
	}
}

func TestImportRejectsWrongStateRoot(t *testing.T) {
	sequencer, _, _ := newTestChain(t, "seed")
	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))

	tampered := *env.ExecutionPayload
	tampered.StateRoot = common.HexToHash("0xbad")
	header, err := sequencer.Config().headerFromPayload(&tampered, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	tampered.BlockHash = header.Hash()

	verifier, _, _ := newTestChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), &tampered, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.INVALID {
		t.Fatalf("expected INVALID, got %s", st.Status)
	}
	if st.ValidationError == nil || !strings.Contains(*st.ValidationError, "state root mismatch") {
		t.Fatalf("unexpected validation error: %v", st.ValidationError)
	}
}

func TestImportRejectsWrongBlockHash(t *testing.T) {
	sequencer, _, _ := newTestChain(t, "seed")
	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))

	tampered := *env.ExecutionPayload
	tampered.BlockHash = common.HexToHash("0xbad")
	verifier, _, _ := newTestChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), &tampered, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.INVALID {
		t.Fatalf("expected INVALID, got %s", st.Status)
	}
}

func TestReorg(t *testing.T) {
	c, _, _ := newTestChain(t, "seed")
	genesis := c.HeadBlock()

	envA := buildBlock(c, t, attrsOn(genesis, [][]byte{depositTx(t, "branch-a")}, true))
	blockA := envA.ExecutionPayload.BlockHash
	if c.BlockByNumber(1).Hash() != blockA {
		t.Fatal("branch A not canonical")
	}

	// Competing block at the same height: park on genesis, build branch B.
	fcu(c, t, genesis.Hash(), nil)
	attrsB := attrsOn(genesis, [][]byte{depositTx(t, "branch-b")}, true)
	attrsB.Timestamp = genesis.Time() + 4
	envB := buildBlock(c, t, attrsB)
	blockB := envB.ExecutionPayload.BlockHash

	if blockA == blockB {
		t.Fatal("branches did not diverge")
	}
	if c.BlockByNumber(1).Hash() != blockB {
		t.Fatal("branch B not canonical after reorg")
	}
	if c.HeadBlock().Hash() != blockB {
		t.Fatal("head is not branch B")
	}

	// And back to branch A.
	fcu(c, t, blockA, nil)
	if c.BlockByNumber(1).Hash() != blockA {
		t.Fatal("branch A not canonical after second reorg")
	}
}

func TestRejectedPoolTxExcluded(t *testing.T) {
	m := machine.NewMock([]byte("seed"))
	m.RejectFn = func(input []byte) bool { return bytes.Contains(input, []byte("REJECTME")) }
	pool := mempool.New(64)
	c, err := New(context.Background(), testConfig(), m, pool)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Add(userTx(t, 0, []byte("REJECTME"))); err != nil {
		t.Fatal(err)
	}
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, false))
	if len(env.ExecutionPayload.Transactions) != 1 {
		t.Fatalf("rejected tx included in block: %d txs", len(env.ExecutionPayload.Transactions))
	}
	if pool.Len() != 0 {
		t.Fatal("rejected tx not dropped from pool")
	}
}

// Holocene moves the EIP-1559 parameters into each header's extraData. The
// bytes must match what an op-geth engine would produce, since op-node
// recomputes the block hash over them.
func TestHoloceneExtraData(t *testing.T) {
	holocene := uint64(0)
	cfg := testConfig()
	cfg.HoloceneTime = &holocene
	c, _, _ := newTestChainWithConfig(t, "seed", cfg)

	genesisExtra := c.HeadBlock().Header.Extra
	if len(genesisExtra) != 9 || genesisExtra[0] != 0x00 {
		t.Fatalf("genesis extraData = %x, want 9-byte version-0 encoding", genesisExtra)
	}

	// Zeroed parameters from op-node mean "use the chain defaults".
	attrs := attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true)
	attrs.EIP1559Params = make([]byte, 8)
	env := buildBlock(c, t, attrs)
	extra := env.ExecutionPayload.ExtraData
	if len(extra) != 9 {
		t.Fatalf("extraData = %x, want 9 bytes", extra)
	}
	denominator := binary.BigEndian.Uint32(extra[1:5])
	elasticity := binary.BigEndian.Uint32(extra[5:9])
	if uint64(denominator) != DefaultEIP1559Denominator || uint64(elasticity) != DefaultEIP1559Elasticity {
		t.Fatalf("extraData encodes %d/%d, want chain defaults %d/%d",
			denominator, elasticity, DefaultEIP1559Denominator, DefaultEIP1559Elasticity)
	}

	// Explicit parameters are echoed through verbatim.
	attrs2 := attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "l1-info-2")}, true)
	attrs2.EIP1559Params = []byte{0, 0, 0, 100, 0, 0, 0, 4}
	env2 := buildBlock(c, t, attrs2)
	if got := env2.ExecutionPayload.ExtraData; !bytes.Equal(got, []byte{0x00, 0, 0, 0, 100, 0, 0, 0, 4}) {
		t.Fatalf("extraData = %x, want explicit params echoed", got)
	}
}

// Before Holocene, extraData must be empty; a payload carrying any is invalid.
func TestPreHoloceneRejectsExtraData(t *testing.T) {
	sequencer, _, _ := newTestChain(t, "seed")
	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "l1-info")}, true))
	if len(env.ExecutionPayload.ExtraData) != 0 {
		t.Fatalf("pre-Holocene extraData = %x, want empty", env.ExecutionPayload.ExtraData)
	}

	tampered := *env.ExecutionPayload
	tampered.ExtraData = []byte{0x00, 0, 0, 0, 250, 0, 0, 0, 6}
	verifier, _, _ := newTestChain(t, "seed")
	st, err := verifier.ImportPayload(context.Background(), &tampered, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.INVALID {
		t.Fatalf("expected INVALID for pre-Holocene extraData, got %s", st.Status)
	}
}

func TestFCUUnknownHeadIsSyncing(t *testing.T) {
	c, _, _ := newTestChain(t, "seed")
	resp, err := c.ForkchoiceUpdated(context.Background(), engine.ForkchoiceStateV1{
		HeadBlockHash: common.HexToHash("0xdeadbeef"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.PayloadStatus.Status != engine.SYNCING {
		t.Fatalf("expected SYNCING, got %s", resp.PayloadStatus.Status)
	}
}

func TestSnapshotPruning(t *testing.T) {
	c, _, _ := newTestChain(t, "seed")
	for i := 0; i < 12; i++ {
		buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "tick")}, true))
	}
	c.mu.RLock()
	snapshots := len(c.machines)
	c.mu.RUnlock()
	if snapshots > c.cfg.MaxSnapshots+3 {
		t.Fatalf("machine snapshots not pruned: %d live", snapshots)
	}
	if c.HeadBlock().NumberU64() != 12 {
		t.Fatalf("head at %d, want 12", c.HeadBlock().NumberU64())
	}
}
