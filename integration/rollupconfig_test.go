package integration

import (
	"bytes"
	"encoding/json"
	"testing"

	opcrollup "github.com/tuler/op-cartesi/rollup"
)

// The field set op-node's rollup.Config declares at v1.19.2. op-node's own
// parser cannot be linked here (see opnode_client.go), so the generated
// document is checked against the schema directly: every key the shim emits
// must be one op-node reads, and every key op-node requires must be present.
var (
	requiredRollupKeys = []string{
		"genesis", "block_time", "seq_window_size", "channel_timeout",
		"l1_chain_id", "l2_chain_id",
		"batch_inbox_address", "deposit_contract_address", "l1_system_config_address",
	}
	knownRollupKeys = map[string]bool{
		"genesis": true, "block_time": true, "max_sequencer_drift": true,
		"seq_window_size": true, "channel_timeout": true,
		"l1_chain_id": true, "l2_chain_id": true,
		"regolith_time": true, "canyon_time": true, "delta_time": true,
		"ecotone_time": true, "fjord_time": true, "granite_time": true,
		"holocene_time": true, "isthmus_time": true, "jovian_time": true,
		"batch_inbox_address": true, "deposit_contract_address": true,
		"l1_system_config_address": true, "chain_op_config": true,
		"alt_da": true, "pectra_blob_schedule_time": true,
		"keep_karst_upgrade_gas": true, "karst_time": true, "lagoon_time": true,
	}
	requiredSystemConfigKeys = []string{"batcherAddr", "overhead", "scalar", "gasLimit", "eip1559Params"}
)

func generateRollupJSON(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	genesis := h.chain.HeadBlock()
	cfg, err := opcrollup.Build(opcrollup.L2Genesis{
		Hash:      genesis.Hash(),
		Timestamp: genesis.Time(),
		GasLimit:  genesis.Header.GasLimit,
	}, h.rollupParams())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := cfg.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("generated rollup.json is not valid JSON: %v", err)
	}
	return doc
}

// TestGeneratedRollupConfigMatchesOpNodeSchema checks the document the
// `genesis` subcommand writes against op-node's rollup.Config schema, and
// against the chain the shim actually serves.
func TestGeneratedRollupConfigMatchesOpNodeSchema(t *testing.T) {
	h := newHarness(t, true)
	doc := generateRollupJSON(t, h)

	for _, key := range requiredRollupKeys {
		if _, ok := doc[key]; !ok {
			t.Errorf("rollup.json is missing required key %q", key)
		}
	}
	for key := range doc {
		if !knownRollupKeys[key] {
			t.Errorf("rollup.json has key %q that op-node's rollup.Config does not declare", key)
		}
	}

	var genesis struct {
		L1 struct {
			Hash   string `json:"hash"`
			Number uint64 `json:"number"`
		} `json:"l1"`
		L2 struct {
			Hash   string `json:"hash"`
			Number uint64 `json:"number"`
		} `json:"l2"`
		L2Time       uint64                     `json:"l2_time"`
		SystemConfig map[string]json.RawMessage `json:"system_config"`
	}
	if err := json.Unmarshal(doc["genesis"], &genesis); err != nil {
		t.Fatalf("genesis section does not match op-node's shape: %v", err)
	}

	// The config must describe the chain this node actually serves, or op-node
	// would derive against a genesis block the engine does not have.
	head := h.chain.HeadBlock()
	if genesis.L2.Hash != head.Hash().Hex() {
		t.Errorf("rollup.json L2 genesis hash %s, but the shim serves %s", genesis.L2.Hash, head.Hash())
	}
	if genesis.L2.Number != 0 {
		t.Errorf("rollup.json L2 genesis number %d, want 0", genesis.L2.Number)
	}
	if genesis.L2Time != head.Time() {
		t.Errorf("rollup.json l2_time %d, but genesis block timestamp is %d", genesis.L2Time, head.Time())
	}
	for _, key := range requiredSystemConfigKeys {
		if _, ok := genesis.SystemConfig[key]; !ok {
			t.Errorf("system_config is missing required key %q", key)
		}
	}

	// Every fork through Granite activates at genesis, and Isthmus (which
	// would require the V4 engine methods the shim does not implement) must be
	// absent rather than set to a future time.
	for _, fork := range []string{"regolith_time", "canyon_time", "delta_time", "ecotone_time", "fjord_time", "granite_time", "holocene_time"} {
		raw, ok := doc[fork]
		if !ok {
			t.Errorf("%s is not activated; op-node would select the wrong Engine API version", fork)
			continue
		}
		if string(raw) != "0" {
			t.Errorf("%s = %s, want activation at genesis (0)", fork, raw)
		}
	}
	if _, ok := doc["isthmus_time"]; ok {
		t.Error("isthmus_time is set, but the shim does not implement the V4 engine methods it requires")
	}
}

// Holocene changes the header encoding, so the rollup config and the chain the
// shim serves must agree on whether it is active.
func TestRollupConfigTracksHoloceneSetting(t *testing.T) {
	for _, holocene := range []bool{false, true} {
		h := newHarness(t, holocene)
		doc := generateRollupJSON(t, h)
		_, present := doc["holocene_time"]
		if present != holocene {
			t.Errorf("holocene=%v: holocene_time present=%v, want %v", holocene, present, holocene)
		}

		var sysCfg struct {
			SystemConfig struct {
				EIP1559Params string `json:"eip1559Params"`
			} `json:"system_config"`
		}
		if err := json.Unmarshal(doc["genesis"], &sysCfg); err != nil {
			t.Fatal(err)
		}
		want := "0x0000000000000000"
		if holocene {
			want = "0x000000fa00000006" // denominator 250, elasticity 6
		}
		if sysCfg.SystemConfig.EIP1559Params != want {
			t.Errorf("holocene=%v: eip1559Params %s, want %s", holocene, sysCfg.SystemConfig.EIP1559Params, want)
		}
	}
}
