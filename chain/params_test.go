package chain

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A document written from a configuration reproduces that configuration's
// consensus half exactly. This is the property the devnet rests on: one file
// written once, read by the config generator and by every engine.
func TestParamsRoundTrip(t *testing.T) {
	original := Params{
		ChainID:            42,
		GenesisTimestamp:   1_720_000_000,
		GasLimit:           20_000_000,
		BaseFee:            big.NewInt(7),
		MaxCyclesPerInput:  123_456,
		AppContract:        common.HexToAddress("0x00000000000000000000000000000000000A9911"),
		EIP1559Denominator: 50,
		EIP1559Elasticity:  10,
	}
	var buf bytes.Buffer
	if err := original.Write(&buf); err != nil {
		t.Fatal(err)
	}
	var decoded Params
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BaseFee.Cmp(original.BaseFee) != 0 {
		t.Errorf("baseFee %s, want %s", decoded.BaseFee, original.BaseFee)
	}
	decoded.BaseFee, original.BaseFee = nil, nil
	if decoded != original {
		t.Errorf("round trip lost something:\n  got  %+v\n  want %+v", decoded, original)
	}
}

// The consensus parameters a document carries are the ones the chain runs
// with, and the node policy it says nothing about is left alone.
func TestParamsApplyKeepsLocalPolicy(t *testing.T) {
	p := Params{ChainID: 5, GasLimit: 1_000_000, BaseFee: big.NewInt(3)}
	cfg := p.Apply(Config{MaxSnapshots: 7, CheckpointInterval: 9, CheckpointRetention: 2})

	if cfg.ChainID != 5 || cfg.GasLimit != 1_000_000 || cfg.BaseFee.Cmp(big.NewInt(3)) != 0 {
		t.Errorf("consensus parameters not applied: %+v", cfg)
	}
	if cfg.MaxSnapshots != 7 || cfg.CheckpointInterval != 9 || cfg.CheckpointRetention != 2 {
		t.Errorf("node policy was overwritten: %+v", cfg)
	}
	// Anything the document omitted takes the same default it would from a
	// bare Config, so a sparse document means what a complete one written
	// from it would.
	if want := (Config{}).withDefaults().MaxCyclesPerInput; cfg.MaxCyclesPerInput != want {
		t.Errorf("omitted maxCyclesPerInput is %d, not the default", cfg.MaxCyclesPerInput)
	}
}

// An omitted base fee is the default; an explicit zero is zero. The two are
// different chains — the base fee is in every header, so it is in the genesis
// hash — and a document must be able to say either.
func TestParamsBaseFeeAbsentIsNotZero(t *testing.T) {
	var absent, zero Params
	if err := json.Unmarshal([]byte(`{"chainId":1}`), &absent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"chainId":1,"baseFee":"0"}`), &zero); err != nil {
		t.Fatal(err)
	}
	if got := absent.Apply(Config{}).BaseFee; got.Sign() == 0 {
		t.Error("an omitted baseFee became zero rather than the default")
	}
	if got := zero.Apply(Config{}).BaseFee; got.Sign() != 0 {
		t.Errorf("an explicit zero baseFee became %s", got)
	}
}

// A key the document does not define is a mistake worth reporting: a
// misspelled consensus parameter would otherwise be dropped, and the node
// would serve a chain its operator did not write down.
func TestParamsRejectsUnknownFields(t *testing.T) {
	var p Params
	if err := json.Unmarshal([]byte(`{"chainId":1,"maxCycles":5}`), &p); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestLoadParamsRequiresAChainID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.json")
	if err := os.WriteFile(path, []byte(`{"gasLimit":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadParams(path); err == nil {
		t.Fatal("a document with no chainId was accepted")
	}
}
