package chain

// The conformance vectors: fixtures under ../conformance that any
// implementation of docs/BLOCKS-SPEC.md replays, rather than one
// implementation checking itself against another. Regenerate with
//
//	go test ./chain -run TestConformance -update
//
// Generation is not a rubber stamp. Wherever an independent check exists it
// runs before the file is written — the outputs tree against the naive
// level-by-level builder, every trie proof against geth's own verifier, every
// header against its own hash — and the constants that were captured from the
// TypeScript guest and the Solidity suite before these files existed are
// asserted here (pinnedTS*, pinnedSolidity*), so regeneration cannot silently
// bless a drift in an encoding that something outside this repository
// already depends on.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

var update = flag.Bool("update", false, "rewrite the vectors under ../conformance")

const vectorDir = "../conformance"

// Constants that predate the vector files: captured from the TypeScript guest
// (@op-cartesi/evm) and pinned in the Solidity suite (contracts/test). The
// generator asserts them, which is what makes moving them into JSON a
// migration rather than a fresh guess.
const (
	pinnedTSWithdrawalPayload = "0x0b0e2d670001000000000000000000000000000000000000000000000000002a000000010000000000000000000000001111111111111111111111111111111111111111000000000000000000000000222222222222222222222222222222222222222200000000000000000000000000000000000000000000000000038d7ea4c6800000000000000000000000000000000000000000000000000000000000000186a000000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002dead000000000000000000000000000000000000000000000000000000000000"

	pinnedSolidityOutputsRoot     = "0xc6848d313a7dbcdb409c9606318673fde2df2896460c76716c63ba695a0466e5"
	pinnedSolidityW1Hash          = "0x27f31ff201b73d4a552b801c296a72166443942679761eb8f895fc846e2edb39"
	pinnedSolidityW2Hash          = "0xb3eb3decba3830b9d78bed4d85fb5a8d4c1ac75032d150fad77b3a8c068c6760"
	pinnedSolidityWithdrawalsRoot = "0xb9e3308bcb994a5eb2144c3841889a2eab0c855b9b3251b41c67b856cff0f398"
	pinnedSolidityOutputsOnlyRoot = "0xeb1f9992f27f25df88c87c5bce969cb7ac49298870c512cf55481d1c2219b9ea"
	pinnedSolidityMessengerHash   = "0xbbbf25d8ab7f7259713453eddad8ecdb6317541a9e5b0b4f3152169d421253a8"

	// The relayMessage payload CrossDomainVectors.t.sol pins, carried here as
	// opaque withdrawal data: it is what makes the messenger-shaped
	// withdrawal below the one the devnet actually produces.
	pinnedRelayMessage = "0xd764ad0b0001000000000000000000000000000000000000000000000000000000000000000000000000000000000000420000000000000000000000000000000000001000000000000000000000000000000000000000000000000000000000000f001000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000030d4000000000000000000000000000000000000000000000000000000000000000c000000000000000000000000000000000000000000000000000000000000000e40166a07a00000000000000000000000059b670e9fa9d0a427751af201d676719a970857b000000000000000000000000e78a0f7e598cc8b0bb87894b0f60dd2a88d6a8ab000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb9226600000000000000000000000000000000000000000000000000000000000000bb000000000000000000000000000000000000000000000000000000000000012c00000000000000000000000000000000000000000000000000000000000000c0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
)

// ---------------------------------------------------------------- plumbing

func hexs(b []byte) string { return hexutil.Encode(b) }

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hexutil.Decode(s)
	if err != nil {
		t.Fatalf("decoding %q: %v", s, err)
	}
	return b
}

func dec(v *big.Int) string { return v.String() }

func undec(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("not a decimal integer: %q", s)
	}
	return v
}

// vector loads a vector file into v, or writes what the generator built when
// -update is set. The comparison is on the marshalled bytes, so a case the
// generator no longer produces fails just as loudly as one whose expectation
// moved.
func vector(t *testing.T, path string, built any, into any) {
	t.Helper()
	full := filepath.Join(vectorDir, path)
	encoded, err := json.MarshalIndent(built, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if *update {
		if err := os.WriteFile(full, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", full, len(encoded))
	}
	stored, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("%v — run `go test ./chain -run TestConformance -update` to create it", err)
	}
	if string(stored) != string(encoded) {
		t.Errorf("%s is out of date; rerun with -update and review the diff", full)
	}
	if into != nil {
		if err := json.Unmarshal(stored, into); err != nil {
			t.Fatalf("re-reading %s: %v", full, err)
		}
	}
}

// ------------------------------------------------------- the scripted machine

// scriptStep is one machine answer, as a vector records it. Responses are
// consumed in call order, not per fork, which is what lets a vector describe
// a build that forks per input and drops the forks it rejects.
type scriptStep struct {
	Accepted bool         `json:"accepted"`
	Cycles   uint64       `json:"cycles"`
	PostRoot string       `json:"postRoot,omitempty"`
	Outputs  []scriptEmit `json:"outputs,omitempty"`
	// Fail is "cycleLimit" or "halted" for an input the machine could not
	// finish. The instance is discarded, as §8.1 requires.
	Fail string `json:"fail,omitempty"`
}

type scriptEmit struct {
	// Reason is the CMIO automatic-yield reason: 2 output, 4 report.
	Reason uint16 `json:"reason"`
	Data   string `json:"data"`
}

// scriptedMachine answers from a script instead of executing anything, so a
// block vector pins the header rules without needing an emulator. Any
// implementation can build the same stub from the same JSON.
type scriptedMachine struct {
	script []scriptStep
	next   *int
	root   common.Hash
}

var _ machine.Machine = (*scriptedMachine)(nil)

func newScriptedMachine(root common.Hash, script []scriptStep) *scriptedMachine {
	n := 0
	return &scriptedMachine{script: script, next: &n, root: root}
}

func (s *scriptedMachine) AdvanceInput(_ context.Context, _ []byte, _ uint64) (*machine.AdvanceResult, error) {
	if *s.next >= len(s.script) {
		return nil, fmt.Errorf("the script ran out after %d inputs", *s.next)
	}
	step := s.script[*s.next]
	*s.next++
	switch step.Fail {
	case "cycleLimit":
		return nil, machine.ErrCycleLimit
	case "halted":
		return nil, machine.ErrHalted
	case "":
	default:
		return nil, fmt.Errorf("unknown scripted failure %q", step.Fail)
	}
	res := &machine.AdvanceResult{Accepted: step.Accepted, Cycles: step.Cycles}
	for _, e := range step.Outputs {
		data, err := hexutil.Decode(e.Data)
		if err != nil {
			return nil, err
		}
		res.Outputs = append(res.Outputs, machine.Output{Reason: e.Reason, Data: data})
	}
	// A rejected input rolls the machine back, so only an accepted one moves
	// the root. The fork is dropped either way; this keeps the stub honest.
	if step.Accepted && step.PostRoot != "" {
		s.root = common.HexToHash(step.PostRoot)
	}
	return res, nil
}

func (s *scriptedMachine) Inspect(context.Context, []byte, uint64) (*machine.InspectResult, error) {
	return nil, fmt.Errorf("the scripted machine answers no queries")
}

func (s *scriptedMachine) ReadMemory(context.Context, uint64, uint64) ([]byte, error) {
	return nil, fmt.Errorf("the scripted machine has no memory")
}

func (s *scriptedMachine) RootHash(context.Context) (common.Hash, error) { return s.root, nil }

func (s *scriptedMachine) Fork(context.Context) (machine.Machine, error) {
	return &scriptedMachine{script: s.script, next: s.next, root: s.root}, nil
}

func (s *scriptedMachine) Store(context.Context, string) error {
	return fmt.Errorf("the scripted machine cannot be stored")
}

func (s *scriptedMachine) Close(context.Context) error { return nil }

// ------------------------------------------------------------ header shapes

type headerJSON struct {
	ParentHash            string `json:"parentHash"`
	Sha3Uncles            string `json:"sha3Uncles"`
	Miner                 string `json:"miner"`
	StateRoot             string `json:"stateRoot"`
	TransactionsRoot      string `json:"transactionsRoot"`
	ReceiptsRoot          string `json:"receiptsRoot"`
	LogsBloom             string `json:"logsBloom"`
	Difficulty            string `json:"difficulty"`
	Number                uint64 `json:"number"`
	GasLimit              uint64 `json:"gasLimit"`
	GasUsed               uint64 `json:"gasUsed"`
	Timestamp             uint64 `json:"timestamp"`
	ExtraData             string `json:"extraData"`
	MixHash               string `json:"mixHash"`
	Nonce                 string `json:"nonce"`
	BaseFeePerGas         string `json:"baseFeePerGas"`
	WithdrawalsRoot       string `json:"withdrawalsRoot"`
	BlobGasUsed           uint64 `json:"blobGasUsed"`
	ExcessBlobGas         uint64 `json:"excessBlobGas"`
	ParentBeaconBlockRoot string `json:"parentBeaconBlockRoot"`
	RequestsHash          string `json:"requestsHash"`
}

func toHeaderJSON(h *types.Header) headerJSON {
	deref := func(p *common.Hash) string {
		if p == nil {
			return ""
		}
		return p.Hex()
	}
	derefU := func(p *uint64) uint64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return headerJSON{
		ParentHash:            h.ParentHash.Hex(),
		Sha3Uncles:            h.UncleHash.Hex(),
		Miner:                 h.Coinbase.Hex(),
		StateRoot:             h.Root.Hex(),
		TransactionsRoot:      h.TxHash.Hex(),
		ReceiptsRoot:          h.ReceiptHash.Hex(),
		LogsBloom:             hexs(h.Bloom.Bytes()),
		Difficulty:            dec(h.Difficulty),
		Number:                h.Number.Uint64(),
		GasLimit:              h.GasLimit,
		GasUsed:               h.GasUsed,
		Timestamp:             h.Time,
		ExtraData:             hexs(h.Extra),
		MixHash:               h.MixDigest.Hex(),
		Nonce:                 hexs(h.Nonce[:]),
		BaseFeePerGas:         dec(h.BaseFee),
		WithdrawalsRoot:       deref(h.WithdrawalsHash),
		BlobGasUsed:           derefU(h.BlobGasUsed),
		ExcessBlobGas:         derefU(h.ExcessBlobGas),
		ParentBeaconBlockRoot: deref(h.ParentBeaconRoot),
		RequestsHash:          deref(h.RequestsHash),
	}
}

// fromHeaderJSON rebuilds the header a vector describes, so the replay can
// hash it independently of the code that produced it.
func fromHeaderJSON(t *testing.T, j headerJSON) *types.Header {
	t.Helper()
	hash := func(s string) *common.Hash { h := common.HexToHash(s); return &h }
	u := func(v uint64) *uint64 { return &v }
	var nonce types.BlockNonce
	copy(nonce[:], unhex(t, j.Nonce))
	return &types.Header{
		ParentHash:       common.HexToHash(j.ParentHash),
		UncleHash:        common.HexToHash(j.Sha3Uncles),
		Coinbase:         common.HexToAddress(j.Miner),
		Root:             common.HexToHash(j.StateRoot),
		TxHash:           common.HexToHash(j.TransactionsRoot),
		ReceiptHash:      common.HexToHash(j.ReceiptsRoot),
		Bloom:            types.BytesToBloom(unhex(t, j.LogsBloom)),
		Difficulty:       undec(t, j.Difficulty),
		Number:           new(big.Int).SetUint64(j.Number),
		GasLimit:         j.GasLimit,
		GasUsed:          j.GasUsed,
		Time:             j.Timestamp,
		Extra:            unhex(t, j.ExtraData),
		MixDigest:        common.HexToHash(j.MixHash),
		Nonce:            nonce,
		BaseFee:          undec(t, j.BaseFeePerGas),
		WithdrawalsHash:  hash(j.WithdrawalsRoot),
		BlobGasUsed:      u(j.BlobGasUsed),
		ExcessBlobGas:    u(j.ExcessBlobGas),
		ParentBeaconRoot: hash(j.ParentBeaconBlockRoot),
		RequestsHash:     hash(j.RequestsHash),
	}
}

// ------------------------------------------------------------- chain params

// paramsJSON is the chain configuration a vector runs under: the genesis
// parameters of BLOCKS-SPEC §4.1 and the state-transition parameters of §4.2,
// which together are everything an implementation must be told.
type paramsJSON struct {
	ChainID            uint64 `json:"chainId"`
	GenesisTimestamp   uint64 `json:"genesisTimestamp"`
	GasLimit           uint64 `json:"gasLimit"`
	BaseFee            string `json:"baseFee"`
	MaxCyclesPerInput  uint64 `json:"maxCyclesPerInput"`
	AppContract        string `json:"appContract"`
	EIP1559Denominator uint64 `json:"eip1559Denominator"`
	EIP1559Elasticity  uint64 `json:"eip1559Elasticity"`
}

func (p paramsJSON) config(t *testing.T) Config {
	t.Helper()
	return Config{
		ChainID:            p.ChainID,
		GenesisTimestamp:   p.GenesisTimestamp,
		GasLimit:           p.GasLimit,
		BaseFee:            undec(t, p.BaseFee),
		MaxCyclesPerInput:  p.MaxCyclesPerInput,
		AppContract:        common.HexToAddress(p.AppContract),
		EIP1559Denominator: p.EIP1559Denominator,
		EIP1559Elasticity:  p.EIP1559Elasticity,
	}
}

func defaultParams() paramsJSON {
	return paramsJSON{
		ChainID:            901,
		GenesisTimestamp:   1_700_000_000,
		GasLimit:           30_000_000,
		BaseFee:            "1000000000",
		MaxCyclesPerInput:  1_000_000_000,
		AppContract:        common.Address{}.Hex(),
		EIP1559Denominator: DefaultEIP1559Denominator,
		EIP1559Elasticity:  DefaultEIP1559Elasticity,
	}
}

// ------------------------------------------------------------- §12.2 extraData

type extraDataFile struct {
	Description string          `json:"description"`
	Spec        string          `json:"spec"`
	Cases       []extraDataCase `json:"cases"`
}

type extraDataCase struct {
	Name string `json:"name"`
	// ChainDenominator and ChainElasticity are the chain's own defaults,
	// used when the attributes carry zeroed parameters.
	ChainDenominator uint64 `json:"chainDenominator"`
	ChainElasticity  uint64 `json:"chainElasticity"`
	Timestamp        uint64 `json:"timestamp"`
	// AttributeParams is op-node's 8-byte eip1559Params field.
	AttributeParams string `json:"attributeParams"`
	// ExtraData is the 9 header bytes, or absent when the input is invalid.
	ExtraData string `json:"extraData,omitempty"`
	Invalid   bool   `json:"invalid,omitempty"`
}

func TestConformanceExtraData(t *testing.T) {
	cases := []extraDataCase{
		{Name: "zeroed attribute parameters take the chain defaults", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1_700_000_002, AttributeParams: "0x0000000000000000"},
		{Name: "explicit attribute parameters are echoed", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1_700_000_004, AttributeParams: "0x0000006400000004"},
		{Name: "the chain defaults are not the OP defaults", ChainDenominator: 50, ChainElasticity: 10, Timestamp: 1_700_000_006, AttributeParams: "0x0000000000000000"},
		{Name: "the largest parameters that fit", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1_700_000_008, AttributeParams: "0xffffffffffffffff"},
		{Name: "a zero denominator with a nonzero elasticity is invalid", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1, AttributeParams: "0x0000000000000004", Invalid: true},
		{Name: "a zero elasticity with a nonzero denominator is invalid", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1, AttributeParams: "0x0000006400000000", Invalid: true},
		{Name: "parameters of the wrong length are invalid", ChainDenominator: 250, ChainElasticity: 6, Timestamp: 1, AttributeParams: "0x000000", Invalid: true},
	}
	for i := range cases {
		c := &cases[i]
		cfg := Config{ChainID: 901, EIP1559Denominator: c.ChainDenominator, EIP1559Elasticity: c.ChainElasticity}
		extra, err := cfg.extraData(c.Timestamp, unhex(t, c.AttributeParams))
		if c.Invalid {
			if err == nil {
				t.Errorf("%s: expected an error, got extraData %x", c.Name, extra)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if len(extra) != 9 || extra[0] != 0x00 {
			t.Fatalf("%s: extraData %x is not the 9-byte version-0 encoding", c.Name, extra)
		}
		c.ExtraData = hexs(extra)
	}
	built := extraDataFile{
		Description: "Header extraData from the Holocene EIP-1559 parameters (BLOCKS-SPEC §12.2). " +
			"Zeroed attribute parameters mean 'use the chain defaults'; a pair with exactly one zero is invalid.",
		Spec:  "docs/BLOCKS-SPEC.md#122-extradata",
		Cases: cases,
	}

	var stored extraDataFile
	vector(t, "blocks/extradata.json", built, &stored)
	for _, c := range stored.Cases {
		cfg := Config{ChainID: 901, EIP1559Denominator: c.ChainDenominator, EIP1559Elasticity: c.ChainElasticity}
		extra, err := cfg.extraData(c.Timestamp, unhex(t, c.AttributeParams))
		switch {
		case c.Invalid && err == nil:
			t.Errorf("%s: replay accepted invalid parameters", c.Name)
		case c.Invalid:
		case err != nil:
			t.Errorf("%s: replay failed: %v", c.Name, err)
		case hexs(extra) != c.ExtraData:
			t.Errorf("%s: replay produced %s, vector says %s", c.Name, hexs(extra), c.ExtraData)
		}
	}
}

// ------------------------------------------------------------- §6 genesis

type genesisFile struct {
	Description string        `json:"description"`
	Spec        string        `json:"spec"`
	Cases       []genesisCase `json:"cases"`
}

type genesisCase struct {
	Name   string     `json:"name"`
	Params paramsJSON `json:"params"`
	// MachineRoot is the root hash of the snapshot the node is handed.
	MachineRoot string     `json:"machineRoot"`
	Header      headerJSON `json:"header"`
	RLP         string     `json:"rlp"`
	Hash        string     `json:"hash"`
}

// genesisHeaderFor runs the real genesis construction against a machine that
// answers with the vector's root and nothing else.
func genesisHeaderFor(t *testing.T, p paramsJSON, machineRoot string) *types.Header {
	t.Helper()
	m := newScriptedMachine(common.HexToHash(machineRoot), nil)
	c, err := New(context.Background(), p.config(t), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.HeadBlock().Header
}

func TestConformanceGenesis(t *testing.T) {
	sparse := defaultParams()
	sparse.GenesisTimestamp = 0
	sparse.BaseFee = "1"
	sparse.GasLimit = 20_000_000
	sparse.EIP1559Denominator = 50
	sparse.EIP1559Elasticity = 10

	inputs := []struct {
		name        string
		params      paramsJSON
		machineRoot string
	}{
		{"devnet defaults", defaultParams(), hexs(crypto.Keccak256([]byte("op-cartesi conformance snapshot")))},
		{"non-default parameters and a zero timestamp", sparse, hexs(crypto.Keccak256([]byte("another snapshot")))},
		{"a zero machine root", defaultParams(), common.Hash{}.Hex()},
	}
	cases := make([]genesisCase, 0, len(inputs))
	for _, in := range inputs {
		h := genesisHeaderFor(t, in.params, in.machineRoot)
		encoded, err := rlp.EncodeToBytes(h)
		if err != nil {
			t.Fatal(err)
		}
		if h.Root != common.HexToHash(in.machineRoot) {
			t.Fatalf("%s: genesis stateRoot %s is not the machine root %s", in.name, h.Root, in.machineRoot)
		}
		cases = append(cases, genesisCase{
			Name: in.name, Params: in.params, MachineRoot: in.machineRoot,
			Header: toHeaderJSON(h), RLP: hexs(encoded), Hash: h.Hash().Hex(),
		})
	}
	built := genesisFile{
		Description: "The genesis block (BLOCKS-SPEC §6): a pure function of the §4.1 parameters and " +
			"the parked machine's root hash. Note that chainId does not enter it — two chains differing " +
			"only in chain id share a genesis hash, and the op-node handshake cannot tell them apart.",
		Spec:  "docs/BLOCKS-SPEC.md#6-the-genesis-block",
		Cases: cases,
	}

	var stored genesisFile
	vector(t, "blocks/genesis.json", built, &stored)
	for _, c := range stored.Cases {
		// Two independent checks: the header the vector describes must hash
		// to the recorded hash, and the implementation must build that same
		// header from the parameters alone.
		rebuilt := fromHeaderJSON(t, c.Header)
		if got := rebuilt.Hash().Hex(); got != c.Hash {
			t.Errorf("%s: the vector's header hashes to %s, not %s", c.Name, got, c.Hash)
		}
		encoded, err := rlp.EncodeToBytes(rebuilt)
		if err != nil {
			t.Fatal(err)
		}
		if hexs(encoded) != c.RLP {
			t.Errorf("%s: the vector's header RLP-encodes differently", c.Name)
		}
		built := genesisHeaderFor(t, c.Params, c.MachineRoot)
		if got := built.Hash().Hex(); got != c.Hash {
			t.Errorf("%s: built genesis hash %s, vector says %s", c.Name, got, c.Hash)
		}
	}
}

// --------------------------------------------------- §7 the EvmAdvance envelope

type evmAdvanceFile struct {
	Description     string           `json:"description"`
	Spec            string           `json:"spec"`
	Signature       string           `json:"signature"`
	Selector        string           `json:"selector"`
	SystemMsgSender string           `json:"systemMsgSender"`
	Cases           []evmAdvanceCase `json:"cases"`
}

type evmAdvanceContext struct {
	ChainID     uint64 `json:"chainId"`
	AppContract string `json:"appContract"`
	BlockNumber uint64 `json:"blockNumber"`
	Timestamp   uint64 `json:"blockTimestamp"`
	PrevRandao  string `json:"prevRandao"`
	FirstIndex  uint64 `json:"firstIndex"`
}

type evmAdvanceCase struct {
	Name    string            `json:"name"`
	Context evmAdvanceContext `json:"context"`
	// Offset is the transaction's position among the block's included
	// transactions; firstIndex + offset is the chain-wide index.
	Offset int    `json:"offset"`
	Index  uint64 `json:"index"`
	RawTx  string `json:"rawTx"`
	// MsgSender is what §7.2 puts in the envelope's third argument.
	MsgSender string `json:"msgSender"`
	Input     string `json:"input"`
}

// conformanceKey is anvil's first account, so a reader can reproduce the
// signatures below with any tool.
const conformanceKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func signedTx(t *testing.T, chainID uint64, data types.TxData) []byte {
	t.Helper()
	key, err := crypto.HexToECDSA(conformanceKey)
	if err != nil {
		t.Fatal(err)
	}
	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(chainID))
	tx, err := types.SignNewTx(key, signer, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func conformanceDeposit(t *testing.T, from common.Address) []byte {
	t.Helper()
	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte("conformance deposit source")),
		From:       from,
		To:         &to,
		Mint:       big.NewInt(1_000_000_000_000_000_000),
		Value:      big.NewInt(1_000_000_000_000_000_000),
		Gas:        100_000,
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestConformanceEvmAdvance(t *testing.T) {
	const chainID = 901
	to := common.HexToAddress("0xc0de000000000000000000000000000000000001")
	signer := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	l1Originator := common.HexToAddress("0x2222222222222222222222222222222222222222")

	eip1559 := signedTx(t, chainID, &types.DynamicFeeTx{
		ChainID: big.NewInt(chainID), Nonce: 0, To: &to, Value: big.NewInt(7),
		Gas: 100_000, GasFeeCap: big.NewInt(1_000_000_000), GasTipCap: new(big.Int),
		Data: []byte{0x06, 0x66, 0x1a, 0xbd},
	})
	legacy := signedTx(t, chainID, &types.LegacyTx{
		Nonce: 3, To: &to, Value: new(big.Int), Gas: 100_000,
		GasPrice: big.NewInt(1_000_000_000), Data: nil,
	})
	deposit := conformanceDeposit(t, l1Originator)

	base := evmAdvanceContext{
		ChainID: chainID, AppContract: common.Address{}.Hex(), BlockNumber: 1,
		Timestamp: 1_700_000_002, PrevRandao: hexs(crypto.Keccak256([]byte("randao"))), FirstIndex: 0,
	}
	deep := base
	deep.BlockNumber = 12
	deep.FirstIndex = 7
	deep.AppContract = common.HexToAddress("0x00000000000000000000000000000000000A9911").Hex()

	inputs := []struct {
		name      string
		ctx       evmAdvanceContext
		offset    int
		raw       []byte
		msgSender common.Address
	}{
		{"an EIP-1559 transaction: msgSender is the recovered signer", base, 0, eip1559, signer},
		{"a legacy transaction, later in the block", base, 2, legacy, signer},
		{"a deposit: msgSender is the L1 originator it carries", base, 1, deposit, l1Originator},
		{"a chain-wide index past the block's first", deep, 3, eip1559, signer},
		{"bytes that do not decode fall back to the system sender", base, 0, []byte("not a transaction"), SystemSenderAddress},
	}

	cases := make([]evmAdvanceCase, 0, len(inputs))
	for _, in := range inputs {
		ic := inputContext{
			ChainID:     in.ctx.ChainID,
			AppContract: common.HexToAddress(in.ctx.AppContract),
			BlockNumber: in.ctx.BlockNumber,
			Timestamp:   in.ctx.Timestamp,
			PrevRandao:  common.HexToHash(in.ctx.PrevRandao),
			FirstIndex:  in.ctx.FirstIndex,
		}
		if got := ic.msgSenderOf(in.raw); got != in.msgSender {
			t.Fatalf("%s: msgSender %s, want %s", in.name, got, in.msgSender)
		}
		input, err := ic.encodeInput(in.offset, in.raw)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, evmAdvanceCase{
			Name: in.name, Context: in.ctx, Offset: in.offset,
			Index:     in.ctx.FirstIndex + uint64(in.offset),
			RawTx:     hexs(in.raw),
			MsgSender: in.msgSender.Hex(),
			Input:     hexs(input),
		})
	}

	_, selector := evmAdvanceABI()
	built := evmAdvanceFile{
		Description: "The EvmAdvance envelope every transaction reaches the machine in " +
			"(BLOCKS-SPEC §7). The signatures are anvil's first account, so they are reproducible " +
			"with any tool; the deposit carries its own originator.",
		Spec:            "docs/BLOCKS-SPEC.md#7-input-framing--the-evmadvance-envelope",
		Signature:       EvmAdvanceSignature,
		Selector:        hexs(selector),
		SystemMsgSender: SystemSenderAddress.Hex(),
		Cases:           cases,
	}

	var stored evmAdvanceFile
	vector(t, "encodings/evmadvance.json", built, &stored)
	for _, c := range stored.Cases {
		ic := inputContext{
			ChainID:     c.Context.ChainID,
			AppContract: common.HexToAddress(c.Context.AppContract),
			BlockNumber: c.Context.BlockNumber,
			Timestamp:   c.Context.Timestamp,
			PrevRandao:  common.HexToHash(c.Context.PrevRandao),
			FirstIndex:  c.Context.FirstIndex,
		}
		raw := unhex(t, c.RawTx)
		if got := ic.msgSenderOf(raw).Hex(); got != c.MsgSender {
			t.Errorf("%s: replay msgSender %s, vector says %s", c.Name, got, c.MsgSender)
		}
		input, err := ic.encodeInput(c.Offset, raw)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if hexs(input) != c.Input {
			t.Errorf("%s: replay envelope differs from the vector", c.Name)
		}
	}
}

// ---------------------------------------------------- §5.4 the EvmCall envelope

type evmCallFile struct {
	Description string         `json:"description"`
	Spec        string         `json:"spec"`
	Signature   string         `json:"signature"`
	Selector    string         `json:"selector"`
	ReportTags  map[string]int `json:"reportTags"`
	Cases       []evmCallCase  `json:"cases"`
}

type evmCallCase struct {
	Name    string `json:"name"`
	ChainID uint64 `json:"chainId"`
	From    string `json:"from"`
	To      string `json:"to"`
	Value   string `json:"value"`
	Data    string `json:"data"`
	Payload string `json:"payload"`
}

func TestConformanceEvmCall(t *testing.T) {
	inputs := []struct {
		name    string
		chainID uint64
		from    string
		to      string
		value   string
		data    string
	}{
		{"a view call with no value", 901, "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266", "0xc0de000000000000000000000000000000000001", "0", "0x06661abd"},
		{"an absent caller is the zero address", 901, common.Address{}.Hex(), "0xc0de000000000000000000000000000000000001", "0", "0x"},
		{"a call carrying value", 42, "0x1111111111111111111111111111111111111111", "0x4200000000000000000000000000000000000010", "1000000000000000", "0xa9059cbb0000000000000000000000002222222222222222222222222222222222222222000000000000000000000000000000000000000000000000000000000000002a"},
	}
	cases := make([]evmCallCase, 0, len(inputs))
	for _, in := range inputs {
		payload, err := EncodeEvmCall(in.chainID, common.HexToAddress(in.from), common.HexToAddress(in.to),
			undec(t, in.value), unhex(t, in.data))
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, evmCallCase{
			Name: in.name, ChainID: in.chainID, From: in.from, To: in.to,
			Value: in.value, Data: in.data, Payload: hexs(payload),
		})
	}
	_, selector := evmCallABI()
	built := evmCallFile{
		Description: "The EvmCall envelope an eth_call reaches the guest's inspect entry in " +
			"(ENGINE-RPC-SPEC §5.4), and the report tags the guest answers with.",
		Spec:      "docs/ENGINE-RPC-SPEC.md#54-eth_call",
		Signature: EvmCallSignature,
		Selector:  hexs(selector),
		ReportTags: map[string]int{
			"app": int(ReportTagApp), "return": int(ReportTagReturn),
			"revert": int(ReportTagRevert), "fail": int(ReportTagFail),
		},
		Cases: cases,
	}

	var stored evmCallFile
	vector(t, "encodings/evmcall.json", built, &stored)
	for _, c := range stored.Cases {
		payload, err := EncodeEvmCall(c.ChainID, common.HexToAddress(c.From), common.HexToAddress(c.To),
			undec(t, c.Value), unhex(t, c.Data))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if hexs(payload) != c.Payload {
			t.Errorf("%s: replay payload differs from the vector", c.Name)
		}
	}
}

// ------------------------------------------- §5.3 guest events as notices

type evmLogFile struct {
	Description string `json:"description"`
	Spec        string `json:"spec"`
	Signature   string `json:"signature"`
	Selector    string `json:"selector"`
	// NoticeSelector is the Cartesi Notice(bytes) framing every output travels
	// under; an EvmLog is a notice whose payload is the call below.
	NoticeSelector string `json:"noticeSelector"`
	// OutputsEmitter and OutputEventTopic are what an output that is *not* an
	// EvmLog becomes in a receipt.
	OutputsEmitter   string       `json:"outputsEmitter"`
	OutputEventTopic string       `json:"outputEventTopic"`
	Cases            []evmLogCase `json:"cases"`
	// NotEvmLogs are outputs a decoder must refuse.
	NotEvmLogs []notCase `json:"notEvmLogs"`
}

type evmLogCase struct {
	Name    string   `json:"name"`
	Emitter string   `json:"emitter"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
	// Payload is the EvmLog call encoding; Output is it wrapped in a Notice.
	Payload string `json:"payload"`
	Output  string `json:"output"`
}

type notCase struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

// encodeEvmLogPayload is the guest's encoder (@op-cartesi/evm encodeEvmLog),
// which this package only ever decodes. It lives in the test because the node
// has no reason to produce one — but a vector does.
func encodeEvmLogPayload(t *testing.T, emitter common.Address, topics []common.Hash, data []byte) []byte {
	t.Helper()
	raw := make([][32]byte, len(topics))
	for i, topic := range topics {
		raw[i] = topic
	}
	packed, err := evmLogArgs.Pack(emitter, raw, data)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte{}, evmLogSelector...), packed...)
}

func wrapNotice(t *testing.T, payload []byte) []byte {
	t.Helper()
	packed, err := noticeArgs.Pack(payload)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte{}, noticeSelector...), packed...)
}

func TestConformanceEvmLog(t *testing.T) {
	transferTopic := crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	inputs := []struct {
		name    string
		emitter common.Address
		topics  []common.Hash
		data    []byte
	}{
		{
			"an ERC-20 Transfer from a token façade",
			common.HexToAddress("0x4200000000000000000000000000000000000010"),
			[]common.Hash{
				transferTopic,
				common.HexToHash("0x000000000000000000000000f39fd6e51aad88f6f4ce6ab8827279cfffb92266"),
				common.HexToHash("0x0000000000000000000000002222222222222222222222222222222222222222"),
			},
			common.BigToHash(big.NewInt(300)).Bytes(),
		},
		{
			"an anonymous event: no topics, no data",
			common.HexToAddress("0xc0de000000000000000000000000000000000001"),
			nil,
			nil,
		},
	}
	cases := make([]evmLogCase, 0, len(inputs))
	for _, in := range inputs {
		payload := encodeEvmLogPayload(t, in.emitter, in.topics, in.data)
		output := wrapNotice(t, payload)
		decoded, ok := ParseEvmLogOutput(output)
		if !ok {
			t.Fatalf("%s: the encoding does not decode as an EvmLog", in.name)
		}
		if decoded.Emitter != in.emitter || len(decoded.Topics) != len(in.topics) {
			t.Fatalf("%s: round trip lost the emitter or topics", in.name)
		}
		topics := make([]string, 0, len(in.topics))
		for _, topic := range in.topics {
			topics = append(topics, topic.Hex())
		}
		cases = append(cases, evmLogCase{
			Name: in.name, Emitter: in.emitter.Hex(), Topics: topics, Data: hexs(in.data),
			Payload: hexs(payload), Output: hexs(output),
		})
	}

	notLogs := []notCase{
		{"a plain notice", hexs(wrapNotice(t, []byte("hello")))},
		{"an unwrapped EvmLog payload", hexs(encodeEvmLogPayload(t, common.Address{}, nil, nil))},
		{"arbitrary bytes", hexs([]byte("not an output"))},
	}
	for _, n := range notLogs {
		if _, ok := ParseEvmLogOutput(unhex(t, n.Output)); ok {
			t.Fatalf("%s decoded as an EvmLog", n.Name)
		}
	}

	built := evmLogFile{
		Description: "Guest events on the provable channel: a Notice whose payload is EvmLog(...). " +
			"Receipt synthesis decodes it into a real log with the guest's own emitter and topics; " +
			"anything else keeps the raw CartesiOutput form (ENGINE-RPC-SPEC §5.3).",
		Spec:             "docs/ENGINE-RPC-SPEC.md#53-receipts",
		Signature:        EvmLogSignature,
		Selector:         hexs(evmLogSelector),
		NoticeSelector:   hexs(noticeSelector),
		OutputsEmitter:   OutputsEmitterAddress.Hex(),
		OutputEventTopic: OutputEventTopic.Hex(),
		Cases:            cases,
		NotEvmLogs:       notLogs,
	}

	var stored evmLogFile
	vector(t, "encodings/evmlog.json", built, &stored)
	for _, c := range stored.Cases {
		decoded, ok := ParseEvmLogOutput(unhex(t, c.Output))
		if !ok {
			t.Errorf("%s: replay failed to decode the vector's output", c.Name)
			continue
		}
		if decoded.Emitter.Hex() != c.Emitter {
			t.Errorf("%s: emitter %s, vector says %s", c.Name, decoded.Emitter, c.Emitter)
		}
		if len(decoded.Topics) != len(c.Topics) {
			t.Errorf("%s: %d topics, vector says %d", c.Name, len(decoded.Topics), len(c.Topics))
			continue
		}
		for i, topic := range decoded.Topics {
			if topic.Hex() != c.Topics[i] {
				t.Errorf("%s: topic %d is %s, vector says %s", c.Name, i, topic, c.Topics[i])
			}
		}
		if hexs(decoded.Data) != c.Data {
			t.Errorf("%s: data %s, vector says %s", c.Name, hexs(decoded.Data), c.Data)
		}
	}
	for _, n := range stored.NotEvmLogs {
		if _, ok := ParseEvmLogOutput(unhex(t, n.Output)); ok {
			t.Errorf("%s decoded as an EvmLog on replay", n.Name)
		}
	}
}

// -------------------------------------------------------- §11.2 withdrawals

type withdrawalFile struct {
	Description    string           `json:"description"`
	Spec           string           `json:"spec"`
	Signature      string           `json:"signature"`
	Selector       string           `json:"selector"`
	NoticeSelector string           `json:"noticeSelector"`
	MessagePasser  string           `json:"messagePasser"`
	Cases          []withdrawalCase `json:"cases"`
	NotWithdrawals []notCase        `json:"notWithdrawals"`
}

type withdrawalJSON struct {
	Nonce    string `json:"nonce"`
	Sender   string `json:"sender"`
	Target   string `json:"target"`
	Value    string `json:"value"`
	GasLimit string `json:"gasLimit"`
	Data     string `json:"data"`
}

type withdrawalCase struct {
	Name       string         `json:"name"`
	Withdrawal withdrawalJSON `json:"withdrawal"`
	// Payload is the Withdrawal call encoding; Output is it wrapped in a
	// Notice, which is what the machine emits.
	Payload string `json:"payload"`
	Output  string `json:"output"`
	// Hash is Hashing.hashWithdrawal; Slot is its sentMessages storage slot.
	Hash string `json:"hash"`
	Slot string `json:"slot"`
}

func (w withdrawalJSON) decode(t *testing.T) *Withdrawal {
	t.Helper()
	return &Withdrawal{
		Nonce:    undec(t, w.Nonce),
		Sender:   common.HexToAddress(w.Sender),
		Target:   common.HexToAddress(w.Target),
		Value:    undec(t, w.Value),
		GasLimit: undec(t, w.GasLimit),
		Data:     unhex(t, w.Data),
	}
}

// versionedNonce is OP's encodeVersionedNonce: version 1 in the top 16 bits,
// the chain-wide input index above a per-input ordinal.
func versionedNonce(inputIndex, ordinal uint64) *big.Int {
	n := new(big.Int).Lsh(big.NewInt(1), 240)
	n.Or(n, new(big.Int).Lsh(new(big.Int).SetUint64(inputIndex), 32))
	return n.Or(n, new(big.Int).SetUint64(ordinal))
}

func conformanceWithdrawals(t *testing.T) []struct {
	name          string
	w             *Withdrawal
	pinnedHash    string
	pinnedPayload string
} {
	t.Helper()
	return []struct {
		name          string
		w             *Withdrawal
		pinnedHash    string
		pinnedPayload string
	}{
		{
			name: "the guest's encoder vector: data, and an ordinal past zero",
			w: &Withdrawal{
				Nonce:  versionedNonce(42, 1),
				Sender: common.HexToAddress("0x1111111111111111111111111111111111111111"),
				Target: common.HexToAddress("0x2222222222222222222222222222222222222222"),
				Value:  big.NewInt(1_000_000_000_000_000), GasLimit: big.NewInt(100_000),
				Data: []byte{0xde, 0xad},
			},
			pinnedPayload: pinnedTSWithdrawalPayload,
		},
		{
			name: "portal vector 1: ether, no data",
			w: &Withdrawal{
				Nonce:  versionedNonce(0, 0),
				Sender: common.HexToAddress("0x1111111111111111111111111111111111111111"),
				Target: common.HexToAddress("0x2222222222222222222222222222222222222222"),
				Value:  big.NewInt(1e15), GasLimit: big.NewInt(100_000),
			},
			pinnedHash: pinnedSolidityW1Hash,
		},
		{
			name: "portal vector 2: a later input, with data",
			w: &Withdrawal{
				Nonce:  versionedNonce(1, 0),
				Sender: common.HexToAddress("0x3333333333333333333333333333333333333333"),
				Target: common.HexToAddress("0x4444444444444444444444444444444444444444"),
				Value:  big.NewInt(2e15), GasLimit: big.NewInt(100_000),
				Data: []byte{0xbe, 0xef},
			},
			pinnedHash: pinnedSolidityW2Hash,
		},
		{
			name: "a messenger-shaped withdrawal wrapping a relayMessage",
			w: &Withdrawal{
				Nonce:  versionedNonce(42, 0),
				Sender: common.HexToAddress("0x4200000000000000000000000000000000000007"),
				Target: common.HexToAddress("0x00000000000000000000000000000000000f00F7"),
				Value:  new(big.Int), GasLimit: big.NewInt(516_982),
				Data: unhex(t, pinnedRelayMessage),
			},
			pinnedHash: pinnedSolidityMessengerHash,
		},
	}
}

func TestConformanceWithdrawal(t *testing.T) {
	inputs := conformanceWithdrawals(t)
	cases := make([]withdrawalCase, 0, len(inputs))
	for _, in := range inputs {
		output, err := in.w.EncodeOutput()
		if err != nil {
			t.Fatal(err)
		}
		hash, err := in.w.Hash()
		if err != nil {
			t.Fatal(err)
		}
		payload, ok := unwrap(output, noticeSelector, noticeArgs)
		if !ok {
			t.Fatalf("%s: the encoded output is not a notice", in.name)
		}
		if in.pinnedPayload != "" && hexs(payload) != in.pinnedPayload {
			t.Fatalf("%s: the encoding drifted from the constant captured from the TypeScript guest:\n  go %s\n  ts %s",
				in.name, hexs(payload), in.pinnedPayload)
		}
		if in.pinnedHash != "" && hash.Hex() != in.pinnedHash {
			t.Fatalf("%s: withdrawal hash %s, but the Solidity suite pins %s", in.name, hash, in.pinnedHash)
		}
		if _, parsed := ParseWithdrawalOutput(output); !parsed {
			t.Fatalf("%s: the encoding does not parse back", in.name)
		}
		cases = append(cases, withdrawalCase{
			Name: in.name,
			Withdrawal: withdrawalJSON{
				Nonce: dec(in.w.Nonce), Sender: in.w.Sender.Hex(), Target: in.w.Target.Hex(),
				Value: dec(in.w.Value), GasLimit: dec(in.w.GasLimit), Data: hexs(in.w.Data),
			},
			Payload: hexs(payload), Output: hexs(output),
			Hash: hash.Hex(), Slot: withdrawalSlot(hash).Hex(),
		})
	}

	notWithdrawals := []notCase{
		{"a plain notice", hexs(wrapNotice(t, []byte("hello")))},
		{"an EvmLog notice", hexs(wrapNotice(t, encodeEvmLogPayload(t, common.Address{}, nil, nil)))},
		{"an unwrapped Withdrawal payload", pinnedTSWithdrawalPayload},
		{"arbitrary bytes", hexs([]byte("not an output"))},
	}
	for _, n := range notWithdrawals {
		if _, ok := ParseWithdrawalOutput(unhex(t, n.Output)); ok {
			t.Fatalf("%s parsed as a withdrawal", n.Name)
		}
	}

	built := withdrawalFile{
		Description: "Withdrawals as the guest emits them (BLOCKS-SPEC §11.2): a Notice wrapping a " +
			"Withdrawal call encoding, its Hashing.hashWithdrawal hash, and the sentMessages storage " +
			"slot that hash occupies in the withdrawal trie. Both the guest (@op-cartesi/evm) and the " +
			"node compute these, which is why they are a vector and not a constant in one of them.",
		Spec:           "docs/BLOCKS-SPEC.md#112-withdrawals",
		Signature:      WithdrawalSignature,
		Selector:       hexs(withdrawalSelector),
		NoticeSelector: hexs(noticeSelector),
		MessagePasser:  MessagePasserAddress.Hex(),
		Cases:          cases,
		NotWithdrawals: notWithdrawals,
	}

	var stored withdrawalFile
	vector(t, "encodings/withdrawal.json", built, &stored)
	for _, c := range stored.Cases {
		w := c.Withdrawal.decode(t)
		output, err := w.EncodeOutput()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if hexs(output) != c.Output {
			t.Errorf("%s: replay output differs from the vector", c.Name)
		}
		hash, err := w.Hash()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if hash.Hex() != c.Hash {
			t.Errorf("%s: replay hash %s, vector says %s", c.Name, hash, c.Hash)
		}
		if got := withdrawalSlot(hash).Hex(); got != c.Slot {
			t.Errorf("%s: replay slot %s, vector says %s", c.Name, got, c.Slot)
		}
		parsed, ok := ParseWithdrawalOutput(unhex(t, c.Output))
		if !ok {
			t.Errorf("%s: the vector's output does not parse", c.Name)
			continue
		}
		if again, err := parsed.Hash(); err != nil || again.Hex() != c.Hash {
			t.Errorf("%s: the parsed withdrawal hashes to %s", c.Name, again)
		}
	}
	for _, n := range stored.NotWithdrawals {
		if _, ok := ParseWithdrawalOutput(unhex(t, n.Output)); ok {
			t.Errorf("%s parsed as a withdrawal on replay", n.Name)
		}
	}
}

// ------------------------------------------------------ §10.2 the outputs tree

type outputsTreeFile struct {
	Description string `json:"description"`
	Spec        string `json:"spec"`
	Height      int    `json:"height"`
	// EmptyRoot is the root of the all-zero tree of full height, which a chain
	// that has produced no outputs commits to.
	EmptyRoot string            `json:"emptyRoot"`
	Cases     []outputsTreeCase `json:"cases"`
}

type outputsTreeCase struct {
	Name    string   `json:"name"`
	Outputs []string `json:"outputs"`
	// Leaves are keccak256 of each output, in order.
	Leaves []string `json:"leaves"`
	// RootAfter[i] is the tree root once outputs[0..i] have been appended.
	RootAfter []string `json:"rootAfter"`
	// Proofs are the co-paths against the final root — 63 siblings each, leaf
	// level first. Present only where the case is small enough to be readable.
	Proofs []outputProofJSON `json:"proofs,omitempty"`
}

type outputProofJSON struct {
	OutputIndex uint64   `json:"outputIndex"`
	Siblings    []string `json:"outputHashesSiblings"`
}

func TestConformanceOutputsTree(t *testing.T) {
	inputs := []struct {
		name      string
		outputs   [][]byte
		withProof bool
	}{
		{
			name:      "the Solidity suite's two outputs",
			outputs:   [][]byte{[]byte("cartesi output 0"), []byte("cartesi output 1")},
			withProof: true,
		},
		{
			name:      "an odd count, which pads the right side with zero subtrees",
			outputs:   [][]byte{[]byte("a"), []byte("b"), []byte("c")},
			withProof: true,
		},
		{
			name: "enough outputs to carry the frontier past two levels",
			outputs: func() [][]byte {
				out := make([][]byte, 9)
				for i := range out {
					out[i] = fmt.Appendf(nil, "output-%d", i)
				}
				return out
			}(),
		},
	}

	cases := make([]outputsTreeCase, 0, len(inputs))
	for _, in := range inputs {
		var leaves []common.Hash
		tree := NewOutputTree()
		c := outputsTreeCase{Name: in.name}
		for _, output := range in.outputs {
			leaf := OutputLeaf(output)
			leaves = append(leaves, leaf)
			tree = tree.Append(leaf)
			// The frontier arithmetic must agree with the slow,
			// level-by-level builder at every size.
			if want := naiveRoot(leaves, OutputTreeHeight); tree.Root() != want {
				t.Fatalf("%s: incremental root %s, reference root %s", in.name, tree.Root(), want)
			}
			c.Outputs = append(c.Outputs, hexs(output))
			c.Leaves = append(c.Leaves, leaf.Hex())
			c.RootAfter = append(c.RootAfter, tree.Root().Hex())
		}
		if in.withProof {
			for i := range leaves {
				proof, err := ProveOutput(leaves, uint64(i))
				if err != nil {
					t.Fatal(err)
				}
				if got := RootAfterReplacement(proof.Siblings, uint64(i), leaves[i]); got != tree.Root() {
					t.Fatalf("%s: proof for %d reproduces %s, not the root %s", in.name, i, got, tree.Root())
				}
				siblings := make([]string, 0, len(proof.Siblings))
				for _, s := range proof.Siblings {
					siblings = append(siblings, s.Hex())
				}
				c.Proofs = append(c.Proofs, outputProofJSON{OutputIndex: uint64(i), Siblings: siblings})
			}
		}
		cases = append(cases, c)
	}
	// The first case is the one the Solidity suite pins; if it moves, the
	// contracts break too.
	if got := cases[0].RootAfter[len(cases[0].RootAfter)-1]; got != pinnedSolidityOutputsRoot {
		t.Fatalf("outputs root %s, but the Solidity suite pins %s", got, pinnedSolidityOutputsRoot)
	}

	built := outputsTreeFile{
		Description: "The cumulative outputs Merkle tree (BLOCKS-SPEC §10.2): height 63, leaves " +
			"keccak256(output), parents keccak256(left‖right), the unfilled right side padded with " +
			"zero subtrees. It is Cartesi's own tree, so these roots are what " +
			"Application.validateOutput verifies against.",
		Spec:      "docs/BLOCKS-SPEC.md#102-the-tree",
		Height:    OutputTreeHeight,
		EmptyRoot: NewOutputTree().Root().Hex(),
		Cases:     cases,
	}

	var stored outputsTreeFile
	vector(t, "commitments/outputs-tree.json", built, &stored)
	if stored.EmptyRoot != NewOutputTree().Root().Hex() {
		t.Errorf("empty root %s, vector says %s", NewOutputTree().Root(), stored.EmptyRoot)
	}
	for _, c := range stored.Cases {
		var leaves []common.Hash
		tree := NewOutputTree()
		for i, output := range c.Outputs {
			leaf := OutputLeaf(unhex(t, output))
			if leaf.Hex() != c.Leaves[i] {
				t.Errorf("%s: leaf %d is %s, vector says %s", c.Name, i, leaf, c.Leaves[i])
			}
			leaves = append(leaves, leaf)
			tree = tree.Append(leaf)
			if tree.Root().Hex() != c.RootAfter[i] {
				t.Errorf("%s: root after %d outputs is %s, vector says %s", c.Name, i+1, tree.Root(), c.RootAfter[i])
			}
		}
		for _, p := range c.Proofs {
			siblings := make([]common.Hash, 0, len(p.Siblings))
			for _, s := range p.Siblings {
				siblings = append(siblings, common.HexToHash(s))
			}
			if got := RootAfterReplacement(siblings, p.OutputIndex, leaves[p.OutputIndex]); got != tree.Root() {
				t.Errorf("%s: the vector's proof for %d reproduces %s, not %s", c.Name, p.OutputIndex, got, tree.Root())
			}
		}
	}
}

// -------------------------------------------------- §11 the withdrawal trie

type passerTrieFile struct {
	Description     string `json:"description"`
	Spec            string `json:"spec"`
	OutputsRootSlot string `json:"outputsRootSlot"`
	// EmptyTrieRoot is the empty Ethereum trie. No block commits it: every
	// block's trie carries at least the outputs root slot.
	EmptyTrieRoot string           `json:"emptyTrieRoot"`
	Cases         []passerTrieCase `json:"cases"`
}

type passerTrieCase struct {
	Name  string           `json:"name"`
	Steps []passerTrieStep `json:"steps"`
	// Proofs are against the root after the last step.
	Proofs []storageProofJSON `json:"proofs"`
}

type passerTrieStep struct {
	// InsertWithdrawals are withdrawal hashes marked sent in this step,
	// inserted before the outputs root is written.
	InsertWithdrawals []string `json:"insertWithdrawals,omitempty"`
	OutputsRoot       string   `json:"outputsRoot"`
	Root              string   `json:"root"`
}

type storageProofJSON struct {
	Name string `json:"name"`
	Slot string `json:"slot"`
	// Value is the RLP-decoded storage value: 0x01 for a sent withdrawal, the
	// trimmed outputs root for the reserved slot, empty for an absent slot.
	Value string   `json:"value"`
	Nodes []string `json:"nodes"`
}

// verifyPasserProof is geth's own trie verifier — the algorithm
// SecureMerkleTrie implements in Solidity — run over a proof this package
// produced, so a proof is never written to a vector unverified.
func verifyPasserProof(t *testing.T, root common.Hash, slot common.Hash, nodes [][]byte) []byte {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	for _, node := range nodes {
		if err := db.Put(crypto.Keccak256(node), node); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := trie.VerifyProof(root, crypto.Keccak256(slot.Bytes()), db)
	if err != nil {
		t.Fatalf("proof for slot %s does not verify against %s: %v", slot, root, err)
	}
	if len(encoded) == 0 {
		return nil // an exclusion proof: the slot is empty
	}
	var value []byte
	if err := rlp.DecodeBytes(encoded, &value); err != nil {
		t.Fatalf("slot %s verifies to %x, which is not an RLP storage value: %v", slot, encoded, err)
	}
	return value
}

func TestConformancePasserTrie(t *testing.T) {
	withdrawals := conformanceWithdrawals(t)
	hashOf := func(i int) common.Hash {
		h, err := withdrawals[i].w.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	emptyOutputs := NewOutputTree().Root()
	twoOutputs := common.HexToHash(pinnedSolidityOutputsRoot)

	inputs := []struct {
		name  string
		steps []struct {
			inserts []common.Hash
			outputs common.Hash
		}
		probe []common.Hash
	}{
		{
			name: "genesis: the reserved slot alone, holding the empty tree's root",
			steps: []struct {
				inserts []common.Hash
				outputs common.Hash
			}{{nil, emptyOutputs}},
			probe: []common.Hash{OutputsRootSlot},
		},
		{
			name: "the Solidity fixture's single-slot trie: the reserved slot alone",
			steps: []struct {
				inserts []common.Hash
				outputs common.Hash
			}{{nil, twoOutputs}},
			probe: []common.Hash{OutputsRootSlot},
		},
		{
			name: "the Solidity suite's trie: two withdrawals and the outputs root",
			steps: []struct {
				inserts []common.Hash
				outputs common.Hash
			}{{[]common.Hash{hashOf(1), hashOf(2)}, twoOutputs}},
			probe: []common.Hash{OutputsRootSlot, withdrawalSlot(hashOf(1)), withdrawalSlot(hashOf(2)),
				withdrawalSlot(crypto.Keccak256Hash([]byte("never sent")))},
		},
		{
			name: "block by block: the trie is cumulative and the reserved slot is overwritten",
			steps: []struct {
				inserts []common.Hash
				outputs common.Hash
			}{
				{nil, emptyOutputs},
				{[]common.Hash{hashOf(1)}, NewOutputTree().Append(OutputLeaf([]byte("cartesi output 0"))).Root()},
				{[]common.Hash{hashOf(2)}, twoOutputs},
				{nil, twoOutputs},
			},
			probe: []common.Hash{OutputsRootSlot, withdrawalSlot(hashOf(1)), withdrawalSlot(hashOf(2))},
		},
	}

	cases := make([]passerTrieCase, 0, len(inputs))
	for _, in := range inputs {
		p := NewPasserTrie()
		c := passerTrieCase{Name: in.name}
		for _, step := range in.steps {
			inserts := make([]string, 0, len(step.inserts))
			for _, h := range step.inserts {
				if err := p.InsertWithdrawal(h); err != nil {
					t.Fatal(err)
				}
				inserts = append(inserts, h.Hex())
			}
			if err := p.SetOutputsRoot(step.outputs); err != nil {
				t.Fatal(err)
			}
			c.Steps = append(c.Steps, passerTrieStep{
				InsertWithdrawals: inserts, OutputsRoot: step.outputs.Hex(), Root: p.Root().Hex(),
			})
		}
		root := p.Root()
		for _, slot := range in.probe {
			nodes, err := p.Prove(slot)
			if err != nil {
				t.Fatal(err)
			}
			value, err := p.SlotValue(slot)
			if err != nil {
				t.Fatal(err)
			}
			// Every proof is checked by geth's verifier before it is written,
			// including the exclusion proofs.
			if verified := verifyPasserProof(t, root, slot, nodes); len(verified) > 0 && hexs(verified) != hexs(value) {
				t.Fatalf("%s: slot %s verifies to %x but the trie holds %x", in.name, slot, verified, value)
			}
			encoded := make([]string, 0, len(nodes))
			for _, node := range nodes {
				encoded = append(encoded, hexs(node))
			}
			name := "an absent withdrawal (exclusion proof)"
			switch {
			case slot == OutputsRootSlot:
				name = "the reserved outputs root slot"
			case len(value) > 0:
				name = "a sent withdrawal"
			}
			c.Proofs = append(c.Proofs, storageProofJSON{Name: name, Slot: slot.Hex(), Value: hexs(value), Nodes: encoded})
		}
		cases = append(cases, c)
	}
	// Both roots the Solidity suite hardcodes. If either moves, the contracts
	// break with this file rather than after it.
	rootOf := func(name string) string {
		for _, c := range cases {
			if c.Name == name {
				return c.Steps[len(c.Steps)-1].Root
			}
		}
		t.Fatalf("no case named %q", name)
		return ""
	}
	if got := rootOf("the Solidity fixture's single-slot trie: the reserved slot alone"); got != pinnedSolidityOutputsOnlyRoot {
		t.Fatalf("outputs-only root %s, but the Solidity fixture pins %s", got, pinnedSolidityOutputsOnlyRoot)
	}
	if got := rootOf("the Solidity suite's trie: two withdrawals and the outputs root"); got != pinnedSolidityWithdrawalsRoot {
		t.Fatalf("withdrawals root %s, but the Solidity suite pins %s", got, pinnedSolidityWithdrawalsRoot)
	}

	built := passerTrieFile{
		Description: "The withdrawal commitment (BLOCKS-SPEC §11): a real Ethereum storage trie over " +
			"the L2ToL1MessagePasser slots, secure-keyed, holding 0x01 per sent withdrawal and the " +
			"Cartesi outputs root at keccak256(\"op-cartesi.outputsMerkleRoot\"). Its root is the " +
			"header's withdrawalsRoot; every proof here was verified with geth's own verifier before " +
			"being written, which is the algorithm OptimismPortal's SecureMerkleTrie implements.",
		Spec:            "docs/BLOCKS-SPEC.md#11-the-withdrawal-commitment",
		OutputsRootSlot: OutputsRootSlot.Hex(),
		EmptyTrieRoot:   NewPasserTrie().Root().Hex(),
		Cases:           cases,
	}

	var stored passerTrieFile
	vector(t, "commitments/passer-trie.json", built, &stored)
	if stored.OutputsRootSlot != crypto.Keccak256Hash([]byte("op-cartesi.outputsMerkleRoot")).Hex() {
		t.Errorf("the vector's reserved slot is not keccak256 of the reserved name")
	}
	for _, c := range stored.Cases {
		p := NewPasserTrie()
		for i, step := range c.Steps {
			for _, h := range step.InsertWithdrawals {
				if err := p.InsertWithdrawal(common.HexToHash(h)); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.SetOutputsRoot(common.HexToHash(step.OutputsRoot)); err != nil {
				t.Fatal(err)
			}
			if p.Root().Hex() != step.Root {
				t.Errorf("%s: root after step %d is %s, vector says %s", c.Name, i, p.Root(), step.Root)
			}
		}
		for _, proof := range c.Proofs {
			nodes := make([][]byte, 0, len(proof.Nodes))
			for _, node := range proof.Nodes {
				nodes = append(nodes, unhex(t, node))
			}
			value := verifyPasserProof(t, p.Root(), common.HexToHash(proof.Slot), nodes)
			if hexs(value) != proof.Value {
				t.Errorf("%s: slot %s verifies to %x, vector says %s", c.Name, proof.Slot, value, proof.Value)
			}
		}
	}
}

// ---------------------------------------------------------- §8–§12 blocks

type blockFile struct {
	Description string           `json:"description"`
	Spec        string           `json:"spec"`
	Cases       []blockChainCase `json:"cases"`
}

type blockChainCase struct {
	Name               string          `json:"name"`
	Params             paramsJSON      `json:"params"`
	GenesisMachineRoot string          `json:"genesisMachineRoot"`
	GenesisHash        string          `json:"genesisHash"`
	Blocks             []blockStepJSON `json:"blocks"`
}

type attributesJSON struct {
	Timestamp             uint64   `json:"timestamp"`
	PrevRandao            string   `json:"prevRandao"`
	SuggestedFeeRecipient string   `json:"suggestedFeeRecipient"`
	ParentBeaconBlockRoot string   `json:"parentBeaconBlockRoot"`
	GasLimit              *uint64  `json:"gasLimit"`
	EIP1559Params         string   `json:"eip1559Params"`
	Transactions          []string `json:"transactions"`
	NoTxPool              bool     `json:"noTxPool"`
}

type blockStepJSON struct {
	Name       string         `json:"name"`
	Attributes attributesJSON `json:"attributes"`
	// PoolTransactions are queued at the sequencer's ingress before the
	// build; which of them survive is the build's decision (§8.2).
	PoolTransactions []string `json:"poolTransactions,omitempty"`
	// MachineResponses are consumed in call order, one per attempted input.
	MachineResponses []scriptStep `json:"machineResponses"`
	// IncludedTransactions is the block body the build settled on.
	IncludedTransactions []string   `json:"includedTransactions"`
	Header               headerJSON `json:"header"`
	Hash                 string     `json:"hash"`
	// OutputsRoot and OutputsCount are the cumulative tree as of this block —
	// what sits in the withdrawal trie's reserved slot.
	OutputsRoot  string `json:"outputsRoot"`
	OutputsCount uint64 `json:"outputsCount"`
}

func (a attributesJSON) attrs(t *testing.T) *engine.PayloadAttributes {
	t.Helper()
	beacon := common.HexToHash(a.ParentBeaconBlockRoot)
	txs := make([][]byte, 0, len(a.Transactions))
	for _, tx := range a.Transactions {
		txs = append(txs, unhex(t, tx))
	}
	return &engine.PayloadAttributes{
		Timestamp:             a.Timestamp,
		Random:                common.HexToHash(a.PrevRandao),
		SuggestedFeeRecipient: common.HexToAddress(a.SuggestedFeeRecipient),
		BeaconRoot:            &beacon,
		Transactions:          txs,
		NoTxPool:              a.NoTxPool,
		GasLimit:              a.GasLimit,
		EIP1559Params:         unhex(t, a.EIP1559Params),
	}
}

// runBlocks drives a chain through a case's steps the way op-node does —
// forkchoice with attributes, get the payload, import it, forkchoice again —
// against a machine that answers from the concatenated script.
func runBlocks(t *testing.T, c blockChainCase, steps []blockStepJSON) []*engine.ExecutableData {
	t.Helper()
	ctx := context.Background()
	var script []scriptStep
	for _, s := range steps {
		script = append(script, s.MachineResponses...)
	}
	m := newScriptedMachine(common.HexToHash(c.GenesisMachineRoot), script)
	pool := mempool.New(256)
	chain, err := New(ctx, c.Params.config(t), m, pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := chain.GenesisHash().Hex(); c.GenesisHash != "" && got != c.GenesisHash {
		t.Fatalf("%s: genesis hash %s, vector says %s", c.Name, got, c.GenesisHash)
	}

	out := make([]*engine.ExecutableData, 0, len(steps))
	head := chain.HeadBlock().Hash()
	for _, step := range steps {
		for _, tx := range step.PoolTransactions {
			if _, err := pool.Add(unhex(t, tx)); err != nil {
				t.Fatalf("%s/%s: queueing a pool transaction: %v", c.Name, step.Name, err)
			}
		}
		fc := engine.ForkchoiceStateV1{HeadBlockHash: head}
		res, err := chain.ForkchoiceUpdated(ctx, fc, step.Attributes.attrs(t))
		if err != nil {
			t.Fatalf("%s/%s: forkchoiceUpdated: %v", c.Name, step.Name, err)
		}
		if res.PayloadID == nil {
			t.Fatalf("%s/%s: no payload id", c.Name, step.Name)
		}
		envelope, ok := chain.Payload(*res.PayloadID)
		if !ok {
			t.Fatalf("%s/%s: the payload went missing", c.Name, step.Name)
		}
		data := envelope.ExecutionPayload
		status, err := chain.ImportPayload(ctx, data, envelope.ParentBeaconBlockRoot)
		if err != nil {
			t.Fatalf("%s/%s: importing the payload it just built: %v", c.Name, step.Name, err)
		}
		if status.Status != engine.VALID {
			t.Fatalf("%s/%s: the node refused its own payload: %s", c.Name, step.Name, status.Status)
		}
		head = data.BlockHash
		if _, err := chain.ForkchoiceUpdated(ctx, engine.ForkchoiceStateV1{
			HeadBlockHash: head, SafeBlockHash: head,
		}, nil); err != nil {
			t.Fatalf("%s/%s: advancing the head: %v", c.Name, step.Name, err)
		}
		out = append(out, data)
	}
	// The cumulative commitments are read off the chain rather than
	// recomputed, so a vector records what the node actually committed.
	for i, data := range out {
		tree, ok := chain.OutputTreeAt(data.BlockHash)
		if !ok {
			t.Fatalf("%s: no outputs accumulator for block %d", c.Name, i)
		}
		steps[i].OutputsRoot = tree.Root().Hex()
		steps[i].OutputsCount = tree.Count()
	}
	return out
}

func TestConformanceBlock(t *testing.T) {
	const chainID = 901
	params := defaultParams()
	toAddr := common.HexToAddress("0xc0de000000000000000000000000000000000001")
	l1Originator := common.HexToAddress("0x2222222222222222222222222222222222222222")
	feeRecipient := common.HexToAddress("0x4200000000000000000000000000000000000011")

	deposit := func(n uint64) []byte {
		to := toAddr
		tx := types.NewTx(&types.DepositTx{
			SourceHash: crypto.Keccak256Hash(fmt.Appendf(nil, "conformance deposit %d", n)),
			From:       l1Originator, To: &to,
			Mint:  big.NewInt(1_000_000_000_000_000_000),
			Value: big.NewInt(1_000_000_000_000_000_000),
			Gas:   100_000,
		})
		raw, err := tx.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	poolTx := func(nonce uint64) []byte {
		return signedTx(t, chainID, &types.DynamicFeeTx{
			ChainID: big.NewInt(chainID), Nonce: nonce, To: &toAddr, Value: new(big.Int),
			Gas: 100_000, GasFeeCap: big.NewInt(1_000_000_000), GasTipCap: new(big.Int),
			Data: fmt.Appendf(nil, "call %d", nonce),
		})
	}
	root := func(name string) string { return hexs(crypto.Keccak256([]byte(name))) }

	// The withdrawal one of the blocks emits, so the header's withdrawalsRoot
	// is exercised rather than only its reserved slot.
	w := conformanceWithdrawals(t)[1].w
	withdrawalOutput, err := w.EncodeOutput()
	if err != nil {
		t.Fatal(err)
	}
	plainNotice := wrapNotice(t, []byte("a plain notice"))

	attrs := func(ts uint64, randao string, gasLimit *uint64, txs ...[]byte) attributesJSON {
		encoded := make([]string, 0, len(txs))
		for _, tx := range txs {
			encoded = append(encoded, hexs(tx))
		}
		return attributesJSON{
			Timestamp: ts, PrevRandao: root(randao),
			SuggestedFeeRecipient: feeRecipient.Hex(),
			ParentBeaconBlockRoot: root("beacon " + randao),
			GasLimit:              gasLimit, EIP1559Params: "0x0000000000000000",
			Transactions: encoded,
		}
	}

	cases := []blockChainCase{
		{
			Name:               "the four admission rules, block by block",
			Params:             params,
			GenesisMachineRoot: root("conformance genesis"),
			Blocks: []blockStepJSON{
				{
					Name:       "one deposit, accepted, emitting nothing",
					Attributes: attrs(params.GenesisTimestamp+2, "randao-1", nil, deposit(1)),
					MachineResponses: []scriptStep{
						{Accepted: true, Cycles: 3_000, PostRoot: root("state-1")},
					},
				},
				{
					Name:             "a deposit emitting a withdrawal and a report, plus a mempool transaction emitting a notice",
					Attributes:       attrs(params.GenesisTimestamp+4, "randao-2", nil, deposit(2)),
					PoolTransactions: []string{hexs(poolTx(0))},
					MachineResponses: []scriptStep{
						{Accepted: true, Cycles: 12_500, PostRoot: root("state-2"), Outputs: []scriptEmit{
							{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: hexs(withdrawalOutput)},
							{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: hexs([]byte("charged the sender"))},
						}},
						{Accepted: true, Cycles: 4_000, PostRoot: root("state-3"), Outputs: []scriptEmit{
							{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: hexs(plainNotice)},
						}},
					},
				},
				{
					Name:             "a rejected deposit stays in the block; a rejected mempool transaction is excluded",
					Attributes:       attrs(params.GenesisTimestamp+6, "randao-3", nil, deposit(3)),
					PoolTransactions: []string{hexs(poolTx(1))},
					MachineResponses: []scriptStep{
						// Rejected: the outputs it emitted before rejecting are
						// dropped, the report is kept, the gas is still charged.
						{Accepted: false, Cycles: 2_000, Outputs: []scriptEmit{
							{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: hexs(plainNotice)},
							{Reason: machine.CmioYieldAutomaticReasonTxReport, Data: hexs([]byte("nonce too low"))},
						}},
						{Accepted: false, Cycles: 1_500},
					},
				},
			},
		},
		{
			Name:               "gasUsed is capped at the block gas limit",
			Params:             params,
			GenesisMachineRoot: root("capped genesis"),
			Blocks: []blockStepJSON{
				{
					Name: "a deposit whose mcycles exceed the limit at CyclesPerGas",
					Attributes: attrs(params.GenesisTimestamp+2, "randao-cap",
						func() *uint64 { v := uint64(1_000_000); return &v }(), deposit(1)),
					MachineResponses: []scriptStep{
						{Accepted: true, Cycles: 5_000_000_000, PostRoot: root("capped state")},
					},
				},
			},
		},
	}

	for i := range cases {
		c := &cases[i]
		payloads := runBlocks(t, *c, c.Blocks)
		for j, data := range payloads {
			step := &c.Blocks[j]
			beacon := common.HexToHash(step.Attributes.ParentBeaconBlockRoot)
			header, err := c.Params.config(t).headerFromPayload(data, &beacon)
			if err != nil {
				t.Fatal(err)
			}
			step.Header = toHeaderJSON(header)
			step.Hash = data.BlockHash.Hex()
			step.IncludedTransactions = make([]string, 0, len(data.Transactions))
			for _, tx := range data.Transactions {
				step.IncludedTransactions = append(step.IncludedTransactions, hexs(tx))
			}
		}
		c.GenesisHash = c.Blocks[0].Header.ParentHash
	}

	built := blockFile{
		Description: "Whole blocks (BLOCKS-SPEC §8–§12), driven the way op-node drives them. " +
			"The machine answers from `machineResponses`, consumed one per attempted input in call " +
			"order, so a vector pins the header rules with no emulator: an implementation replays it " +
			"against the same stub and must reach the same block hash.",
		Spec:  "docs/BLOCKS-SPEC.md#8-executing-a-block",
		Cases: cases,
	}

	var stored blockFile
	vector(t, "blocks/block.json", built, &stored)
	for _, c := range stored.Cases {
		steps := append([]blockStepJSON(nil), c.Blocks...)
		payloads := runBlocks(t, c, steps)
		for j, data := range payloads {
			want := c.Blocks[j]
			if data.BlockHash.Hex() != want.Hash {
				t.Errorf("%s/%s: block hash %s, vector says %s", c.Name, want.Name, data.BlockHash, want.Hash)
			}
			if data.GasUsed != want.Header.GasUsed {
				t.Errorf("%s/%s: gasUsed %d, vector says %d", c.Name, want.Name, data.GasUsed, want.Header.GasUsed)
			}
			if data.WithdrawalsRoot == nil || data.WithdrawalsRoot.Hex() != want.Header.WithdrawalsRoot {
				t.Errorf("%s/%s: withdrawalsRoot %v, vector says %s", c.Name, want.Name, data.WithdrawalsRoot, want.Header.WithdrawalsRoot)
			}
			if len(data.Transactions) != len(want.IncludedTransactions) {
				t.Errorf("%s/%s: %d transactions included, vector says %d",
					c.Name, want.Name, len(data.Transactions), len(want.IncludedTransactions))
			}
			if steps[j].OutputsRoot != want.OutputsRoot || steps[j].OutputsCount != want.OutputsCount {
				t.Errorf("%s/%s: outputs commitment %s/%d, vector says %s/%d", c.Name, want.Name,
					steps[j].OutputsRoot, steps[j].OutputsCount, want.OutputsRoot, want.OutputsCount)
			}
			// The header the vector records must hash to the same block.
			if got := fromHeaderJSON(t, want.Header).Hash().Hex(); got != want.Hash {
				t.Errorf("%s/%s: the vector's header hashes to %s, not %s", c.Name, want.Name, got, want.Hash)
			}
		}
	}
}

// ------------------------------------------------------- §13 payload import

type importFile struct {
	Description string       `json:"description"`
	Spec        string       `json:"spec"`
	Setup       importSetup  `json:"setup"`
	Cases       []importCase `json:"cases"`
}

// importSetup is the chain a verifier starts from: genesis, and the machine
// answers re-executing the payload consumes. Each case runs against a fresh
// chain built from exactly this, so a VALID import cannot affect the next.
type importSetup struct {
	Params             paramsJSON   `json:"params"`
	GenesisMachineRoot string       `json:"genesisMachineRoot"`
	GenesisHash        string       `json:"genesisHash"`
	MachineResponses   []scriptStep `json:"machineResponses"`
}

type importCase struct {
	Name string `json:"name"`
	// Rule names the BLOCKS-SPEC §13 check this case exercises. It is
	// documentation: error messages are deliberately unspecified
	// (ENGINE-RPC-SPEC §9.2), so only the status is normative.
	Rule                  string      `json:"rule"`
	Payload               payloadJSON `json:"payload"`
	ParentBeaconBlockRoot string      `json:"parentBeaconBlockRoot"`
	Status                string      `json:"status"`
}

// payloadJSON is engine_newPayloadV4's execution payload. Pointer fields are
// the ones whose *absence* is itself a case.
type payloadJSON struct {
	ParentHash      string                 `json:"parentHash"`
	FeeRecipient    string                 `json:"feeRecipient"`
	StateRoot       string                 `json:"stateRoot"`
	ReceiptsRoot    string                 `json:"receiptsRoot"`
	LogsBloom       string                 `json:"logsBloom"`
	PrevRandao      string                 `json:"prevRandao"`
	BlockNumber     uint64                 `json:"blockNumber"`
	GasLimit        uint64                 `json:"gasLimit"`
	GasUsed         uint64                 `json:"gasUsed"`
	Timestamp       uint64                 `json:"timestamp"`
	ExtraData       string                 `json:"extraData"`
	BaseFeePerGas   *string                `json:"baseFeePerGas"`
	BlockHash       string                 `json:"blockHash"`
	Transactions    []string               `json:"transactions"`
	Withdrawals     *[]withdrawalEntryJSON `json:"withdrawals"`
	BlobGasUsed     uint64                 `json:"blobGasUsed"`
	ExcessBlobGas   uint64                 `json:"excessBlobGas"`
	WithdrawalsRoot *string                `json:"withdrawalsRoot"`
}

// withdrawalEntryJSON is a consensus-layer withdrawal. This chain's payloads
// always carry an empty list — the field exists so a vector can express the
// non-empty case §13.2 rejects.
type withdrawalEntryJSON struct {
	Index          uint64 `json:"index"`
	ValidatorIndex uint64 `json:"validatorIndex"`
	Address        string `json:"address"`
	Amount         uint64 `json:"amount"`
}

func toPayloadJSON(d *engine.ExecutableData) payloadJSON {
	txs := make([]string, 0, len(d.Transactions))
	for _, tx := range d.Transactions {
		txs = append(txs, hexs(tx))
	}
	p := payloadJSON{
		ParentHash: d.ParentHash.Hex(), FeeRecipient: d.FeeRecipient.Hex(),
		StateRoot: d.StateRoot.Hex(), ReceiptsRoot: d.ReceiptsRoot.Hex(),
		LogsBloom: hexs(d.LogsBloom), PrevRandao: d.Random.Hex(),
		BlockNumber: d.Number, GasLimit: d.GasLimit, GasUsed: d.GasUsed,
		Timestamp: d.Timestamp, ExtraData: hexs(d.ExtraData),
		BlockHash: d.BlockHash.Hex(), Transactions: txs,
		BlobGasUsed: derefOr(d.BlobGasUsed), ExcessBlobGas: derefOr(d.ExcessBlobGas),
	}
	if d.BaseFeePerGas != nil {
		baseFee := dec(d.BaseFeePerGas)
		p.BaseFeePerGas = &baseFee
	}
	if d.WithdrawalsRoot != nil {
		root := d.WithdrawalsRoot.Hex()
		p.WithdrawalsRoot = &root
	}
	if d.Withdrawals != nil {
		entries := make([]withdrawalEntryJSON, 0, len(d.Withdrawals))
		for i, w := range d.Withdrawals {
			entries = append(entries, withdrawalEntryJSON{
				Index: uint64(i), ValidatorIndex: uint64(w.Validator),
				Address: w.Address.Hex(), Amount: w.Amount,
			})
		}
		p.Withdrawals = &entries
	}
	return p
}

func derefOr(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func (p payloadJSON) executable(t *testing.T) *engine.ExecutableData {
	t.Helper()
	u := func(v uint64) *uint64 { return &v }
	d := &engine.ExecutableData{
		ParentHash: common.HexToHash(p.ParentHash), FeeRecipient: common.HexToAddress(p.FeeRecipient),
		StateRoot: common.HexToHash(p.StateRoot), ReceiptsRoot: common.HexToHash(p.ReceiptsRoot),
		LogsBloom: unhex(t, p.LogsBloom), Random: common.HexToHash(p.PrevRandao),
		Number: p.BlockNumber, GasLimit: p.GasLimit, GasUsed: p.GasUsed,
		Timestamp: p.Timestamp, ExtraData: unhex(t, p.ExtraData),
		BlockHash:   common.HexToHash(p.BlockHash),
		BlobGasUsed: u(p.BlobGasUsed), ExcessBlobGas: u(p.ExcessBlobGas),
	}
	if p.BaseFeePerGas != nil {
		d.BaseFeePerGas = undec(t, *p.BaseFeePerGas)
	}
	if p.WithdrawalsRoot != nil {
		root := common.HexToHash(*p.WithdrawalsRoot)
		d.WithdrawalsRoot = &root
	}
	if p.Withdrawals != nil {
		d.Withdrawals = make([]*types.Withdrawal, 0, len(*p.Withdrawals))
		for _, w := range *p.Withdrawals {
			d.Withdrawals = append(d.Withdrawals, &types.Withdrawal{
				Index: w.Index, Validator: w.ValidatorIndex,
				Address: common.HexToAddress(w.Address), Amount: w.Amount,
			})
		}
	}
	for _, tx := range p.Transactions {
		d.Transactions = append(d.Transactions, unhex(t, tx))
	}
	return d
}

func TestConformanceImport(t *testing.T) {
	ctx := context.Background()
	params := defaultParams()
	genesisRoot := hexs(crypto.Keccak256([]byte("import genesis")))
	postRoot := hexs(crypto.Keccak256([]byte("import state")))
	l1Originator := common.HexToAddress("0x2222222222222222222222222222222222222222")
	beacon := crypto.Keccak256Hash([]byte("import beacon"))

	responses := []scriptStep{{Accepted: true, Cycles: 9_000, PostRoot: postRoot, Outputs: []scriptEmit{
		{Reason: machine.CmioYieldAutomaticReasonTxOutput, Data: hexs(wrapNotice(t, []byte("an output")))},
	}}}
	setup := importSetup{Params: params, GenesisMachineRoot: genesisRoot, MachineResponses: responses}

	// Build the payload every case starts from, on a chain of its own.
	builderCase := blockChainCase{
		Name: "import base", Params: params, GenesisMachineRoot: genesisRoot,
	}
	steps := []blockStepJSON{{
		Name: "base",
		Attributes: attributesJSON{
			Timestamp: params.GenesisTimestamp + 2, PrevRandao: hexs(crypto.Keccak256([]byte("import randao"))),
			SuggestedFeeRecipient: common.Address{}.Hex(), ParentBeaconBlockRoot: beacon.Hex(),
			EIP1559Params: "0x0000000000000000",
			Transactions:  []string{hexs(conformanceDeposit(t, l1Originator))},
		},
		MachineResponses: responses,
	}}
	base := runBlocks(t, builderCase, steps)[0]

	cfg := params.config(t).withDefaults()
	// rehash reconstructs the header a mutated payload commits to and stamps
	// its own hash, so a case exercises the rule it names rather than tripping
	// the block-hash check first.
	rehash := func(d *engine.ExecutableData) *engine.ExecutableData {
		header, err := cfg.headerFromPayload(d, &beacon)
		if err != nil {
			// The mutation is structural: no header, so leave the hash alone
			// and let §13's structural checks reject it.
			return d
		}
		d.BlockHash = header.Hash()
		return d
	}
	// clone is a deep-enough copy that a mutation cannot leak between cases.
	clone := func() *engine.ExecutableData {
		c := *base
		c.Transactions = append([][]byte(nil), base.Transactions...)
		c.LogsBloom = append([]byte(nil), base.LogsBloom...)
		c.Withdrawals = []*types.Withdrawal{}
		root := *base.WithdrawalsRoot
		c.WithdrawalsRoot = &root
		c.BaseFeePerGas = new(big.Int).Set(base.BaseFeePerGas)
		return &c
	}

	type mutation struct {
		name   string
		rule   string
		status string
		mutate func(*engine.ExecutableData) payloadJSON
	}
	plain := func(f func(*engine.ExecutableData)) func(*engine.ExecutableData) payloadJSON {
		return func(d *engine.ExecutableData) payloadJSON { f(d); return toPayloadJSON(rehash(d)) }
	}

	mutations := []mutation{
		{"the payload as built", "§13 execution: everything agrees", "VALID",
			func(d *engine.ExecutableData) payloadJSON { return toPayloadJSON(d) }},
		{"an unknown parent is SYNCING, not INVALID", "§13.10", "SYNCING",
			plain(func(d *engine.ExecutableData) { d.ParentHash = crypto.Keccak256Hash([]byte("no such block")) })},
		{"a block hash that does not match the header", "§13.8", "INVALID",
			func(d *engine.ExecutableData) payloadJSON {
				d.BlockHash = crypto.Keccak256Hash([]byte("wrong"))
				return toPayloadJSON(d)
			}},
		{"a state root the machine did not reach", "§13.12", "INVALID",
			plain(func(d *engine.ExecutableData) { d.StateRoot = crypto.Keccak256Hash([]byte("wrong state")) })},
		{"a gas figure the cycles do not support", "§13.13", "INVALID",
			plain(func(d *engine.ExecutableData) { d.GasUsed = d.GasUsed + 1 })},
		{"a withdrawal commitment the outputs do not produce", "§13.14", "INVALID",
			plain(func(d *engine.ExecutableData) {
				root := crypto.Keccak256Hash([]byte("wrong commitment"))
				d.WithdrawalsRoot = &root
			})},
		{"a non-empty withdrawals list", "§13.2", "INVALID",
			plain(func(d *engine.ExecutableData) { d.Withdrawals = []*types.Withdrawal{{}} })},
		{"an absent withdrawals list", "§13.2", "INVALID",
			plain(func(d *engine.ExecutableData) { d.Withdrawals = nil })},
		{"an absent withdrawals root", "§13.4", "INVALID",
			plain(func(d *engine.ExecutableData) { d.WithdrawalsRoot = nil })},
		{"an absent base fee", "§13.3", "INVALID",
			plain(func(d *engine.ExecutableData) { d.BaseFeePerGas = nil })},
		{"a logs bloom of the wrong length", "§13.1", "INVALID",
			plain(func(d *engine.ExecutableData) { d.LogsBloom = []byte{0x00} })},
		{"extraData whose EIP-1559 parameters are zeroed", "§13.5", "INVALID",
			plain(func(d *engine.ExecutableData) { d.ExtraData = make([]byte, 9) })},
		{"a number that is not the parent's plus one", "§13.11", "INVALID",
			plain(func(d *engine.ExecutableData) { d.Number = d.Number + 1 })},
		{"a timestamp that does not advance past the parent", "§13.11", "INVALID",
			plain(func(d *engine.ExecutableData) { d.Timestamp = params.GenesisTimestamp })},
	}

	cases := make([]importCase, 0, len(mutations))
	for _, m := range mutations {
		cases = append(cases, importCase{
			Name: m.name, Rule: m.rule, Payload: m.mutate(clone()),
			ParentBeaconBlockRoot: beacon.Hex(), Status: m.status,
		})
	}
	setup.GenesisHash = base.ParentHash.Hex()

	built := importFile{
		Description: "engine_newPayloadV4's validation (BLOCKS-SPEC §13), one payload per rule. " +
			"Only the status is normative — error messages are deliberately unspecified " +
			"(ENGINE-RPC-SPEC §9.2) — and `rule` names the check for the reader. Every case runs " +
			"against a fresh chain at genesis, so a VALID import cannot influence the next. The " +
			"argument-level rules of §13.6–§13.7 (an absent parentBeaconBlockRoot, non-empty blob " +
			"hashes or execution requests) are rejected a layer up, in the RPC service.",
		Spec:  "docs/BLOCKS-SPEC.md#13-importing-a-payload",
		Setup: setup,
		Cases: cases,
	}

	var stored importFile
	vector(t, "blocks/import.json", built, &stored)
	for _, c := range stored.Cases {
		m := newScriptedMachine(common.HexToHash(stored.Setup.GenesisMachineRoot), stored.Setup.MachineResponses)
		verifier, err := New(ctx, stored.Setup.Params.config(t), m, nil)
		if err != nil {
			t.Fatal(err)
		}
		root := common.HexToHash(c.ParentBeaconBlockRoot)
		status, err := verifier.ImportPayload(ctx, c.Payload.executable(t), &root)
		if err != nil {
			t.Errorf("%s: import returned an error rather than a status: %v", c.Name, err)
			continue
		}
		if string(status.Status) != c.Status {
			reason := ""
			if status.ValidationError != nil {
				reason = " (" + *status.ValidationError + ")"
			}
			t.Errorf("%s: status %s%s, vector says %s", c.Name, status.Status, reason, c.Status)
		}
	}
}
