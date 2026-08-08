package machine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

// Remote drives a cartesi-jsonrpc-machine server (the emulator's remote
// machine protocol), pinned to the protocol of machine-emulator 0.21.0
// (JSON-RPC "get_version" reports 0.7.0; the 0.21.0-test prereleases this
// client was developed against reported 0.6.x).
//
// The wire format was established by probing a running server, not by reading
// its rpc.discover schema: in the test prereleases the schema advertised
// methods the build did not implement (machine.read_register, is_empty). The
// 0.21.0 release reconciled the two — the schema now says machine.read_reg
// and machine.is_empty exists — so the probed surface and the published one
// finally agree. Byte buffers and hashes are base64, registers are read
// through machine.read_reg, and cmio requests are fetched with an explicit
// length. One method tightened between the prereleases and the release:
// send_cmio_response takes the revert root hash only on advance-state
// responses — required there, verified against the machine's own root — and
// refuses it on every other kind (see sendCmioResponse).
//
// Snapshots map onto the server's "fork" method, which spawns a copy-on-write
// child server, so Fork and Close are process lifecycle operations on the
// emulator side.
type Remote struct {
	endpoint string
	client   *http.Client
	reqID    atomic.Uint64
	// owned is false for the server the operator started and handed to us.
	// Closing that one would kill their process, so only forks — which this
	// package spawned — are shut down by Close.
	owned bool
}

var _ Machine = (*Remote)(nil)

// maxCmioReadLength caps a single cmio data fetch. Requests larger than this
// are re-fetched at their exact length, so the cap only bounds the common case
// rather than the maximum output size.
const maxCmioReadLength = 1 << 21 // 2 MiB

// DialRemote connects to a cartesi-jsonrpc-machine server. The server may or
// may not have a machine loaded yet; use Load or check Loaded.
func DialRemote(ctx context.Context, endpoint string) (*Remote, error) {
	r := &Remote{endpoint: strings.TrimSuffix(endpoint, "/"), client: &http.Client{}}
	var version struct {
		Major uint32 `json:"major"`
		Minor uint32 `json:"minor"`
		Patch uint32 `json:"patch"`
	}
	if err := r.call(ctx, "get_version", nil, &version); err != nil {
		return nil, fmt.Errorf("machine server unreachable at %s: %w", endpoint, err)
	}
	return r, nil
}

// Load makes the server load a stored machine from a directory, which is how a
// snapshot produced by `cartesi-machine --store=<dir>` becomes the chain's
// genesis state.
func (r *Remote) Load(ctx context.Context, directory string) error {
	err := r.call(ctx, "machine.load", map[string]any{"directory": directory}, nil)
	// A server holds exactly one machine and will not replace it, so this is
	// what a stale server from a previous run looks like. Saying so beats
	// leaving the operator to work out what "machine exists" means about a
	// server they thought was theirs.
	if err != nil && strings.Contains(err.Error(), "machine exists") {
		return fmt.Errorf("the machine server at %s already holds a machine, and a server holds only one: it is left over from an earlier run, so stop it and start a fresh one", r.endpoint)
	}
	return err
}

// Loaded reports whether the server currently holds a machine.
//
// The natural method is machine.is_empty, implemented since the 0.21.0
// release (the test prereleases advertised it but answered method-not-found).
// The probe below is kept anyway: it works on every 0.21 build, and asking
// for the root hash verifies the machine actually answers a real read rather
// than merely existing. A server with no machine answers a distinct
// "no machine" error.
func (r *Remote) Loaded(ctx context.Context) (bool, error) {
	var hash binary
	err := r.call(ctx, "machine.get_root_hash", nil, &hash)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "no machine") {
		return false, nil
	}
	return false, err
}

type rpcRequest struct {
	Version string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r *Remote) call(ctx context.Context, method string, params any, result any) error {
	body, err := json.Marshal(rpcRequest{Version: "2.0", ID: r.reqID.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var rr rpcResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("%s: bad response: %w", method, err)
	}
	if rr.Error != nil {
		return fmt.Errorf("%s: %s (code %d)", method, rr.Error.Message, rr.Error.Code)
	}
	if result != nil {
		if err := json.Unmarshal(rr.Result, result); err != nil {
			return fmt.Errorf("%s: bad result: %w", method, err)
		}
	}
	return nil
}

// binary is a byte slice carried as a base64 string, which is how the emulator
// encodes both Base64String and Base64Hash.
type binary []byte

func (b *binary) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("expected base64, got %q: %w", s, err)
	}
	*b = decoded
	return nil
}

func (b binary) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(b))
}

// breakReason is the InterpreterBreakReason enum machine.run returns.
type breakReason string

const (
	breakFailed         breakReason = "failed"
	breakHalted         breakReason = "halted"
	breakYieldedManual  breakReason = "yielded_manually"
	breakYieldedAuto    breakReason = "yielded_automatically"
	breakYieldedSoft    breakReason = "yielded_softly"
	breakReachedTarget  breakReason = "reached_target_mcycle"
	breakConsoleOutput  breakReason = "console_output"
	breakConsoleInput   breakReason = "console_input"
	breakMcycleOverflow breakReason = "mcycle_overflow"
)

func (r *Remote) run(ctx context.Context, mcycleEnd uint64) (breakReason, error) {
	var br breakReason
	err := r.call(ctx, "machine.run", map[string]any{"mcycle_end": mcycleEnd}, &br)
	return br, err
}

// readReg reads one machine register.
//
// The method is machine.read_reg — the name the 0.21.0 release both
// implements and advertises. (The test prereleases' schema said
// machine.read_register while implementing only this name, which is why
// every method here was probed against a running server rather than taken
// from the schema.)
func (r *Remote) readReg(ctx context.Context, reg string) (uint64, error) {
	var v uint64
	err := r.call(ctx, "machine.read_reg", map[string]any{"reg": reg}, &v)
	return v, err
}

// readMcycle reads the cycle counter.
func (r *Remote) readMcycle(ctx context.Context) (uint64, error) {
	return r.readReg(ctx, "mcycle")
}

type cmioRequest struct {
	Cmd             uint8  `json:"cmd"`
	Reason          uint16 `json:"reason"`
	AvailableLength uint64 `json:"available_length"`
	Data            binary `json:"data"`
}

// receiveCmioRequest fetches the pending request. The server truncates the
// data to the requested length and reports the true size, so an emission
// larger than the default cap is fetched again at its exact length rather than
// being silently cut short.
func (r *Remote) receiveCmioRequest(ctx context.Context) (*cmioRequest, error) {
	var req cmioRequest
	if err := r.call(ctx, "machine.receive_cmio_request", map[string]any{"length": maxCmioReadLength}, &req); err != nil {
		return nil, err
	}
	if req.AvailableLength > uint64(len(req.Data)) {
		if err := r.call(ctx, "machine.receive_cmio_request", map[string]any{"length": req.AvailableLength}, &req); err != nil {
			return nil, err
		}
	}
	return &req, nil
}

// sendCmioResponse hands the guest a response, and 0.21.0 is strict about the
// revert_root_hash parameter: an advance-state response REQUIRES it — the
// state the machine returns to if the guest rejects the input, which is what
// makes a rejection leave no trace — and the machine checks it equals its own
// current root hash, so it is a cross-check binding the host's view to the
// machine's state rather than a free choice. Every other response kind
// (inspect-state, GIO) must OMIT it: the machine refuses the call otherwise
// ("revert root hash is only accepted for advance-state responses"), because
// it never records a revert point for them. A nil revertRootHash omits the
// parameter.
func (r *Remote) sendCmioResponse(ctx context.Context, reason uint16, data []byte, revertRootHash *common.Hash) error {
	params := map[string]any{
		"reason": reason,
		"data":   base64.StdEncoding.EncodeToString(data),
	}
	if revertRootHash != nil {
		params["revert_root_hash"] = base64.StdEncoding.EncodeToString(revertRootHash.Bytes())
	}
	return r.call(ctx, "machine.send_cmio_response", params, nil)
}

// storeSharing is the sharing mode Store uses, and it is not a free choice.
//
// The emulator's three modes describe how a stored machine's address ranges
// relate to backing files, and only "all" writes the machine as it is *now*:
// "none" and "config" write the contents of the backing stores the machine was
// loaded from, so a machine that has consumed inputs since is stored at the
// state it was loaded at. That failure is silent — the call succeeds, the
// files look right, and the checkpoint reloads to a stale root. TestRemoteStore
// pins it.
//
// The name suggests the stored copy would go on being written to as execution
// continues, which would make it useless as a checkpoint. It does not: the
// stored directory is independent once written.
const storeSharing = "all"

// Store writes the machine's state to a directory that machine.load can later
// read. The receiver is unaffected and stays usable.
func (r *Remote) Store(ctx context.Context, directory string) error {
	return r.call(ctx, "machine.store", map[string]any{
		"directory": directory,
		"sharing":   storeSharing,
	}, nil)
}

// CheckReady verifies the loaded machine is already parked at a manual yield
// waiting for its first input, which is the state `cartesi-machine --store`
// leaves it in and the state Cartesi Rollups templates are distributed in.
//
// The node deliberately does not boot the machine itself. If it did, the
// genesis state root would depend on how the node ran the boot rather than on
// the snapshot alone, and two operators booting the same template with
// different cycle budgets could disagree about genesis. With the machine
// stored after boot, genesis is simply the stored machine's own root hash.
//
// It reads no more than it needs to: iflags_Y says whether the machine is
// yielded at all, and the pending request says which yield it is.
func (r *Remote) CheckReady(ctx context.Context) error {
	yielded, err := r.readReg(ctx, "iflags_Y")
	if err != nil {
		return err
	}
	if yielded == 0 {
		return fmt.Errorf("the stored machine is not parked at an input yield — store it with `cartesi-machine ... --store=<dir>`, which runs to the first yield, rather than with --max-mcycle=0")
	}
	req, err := r.receiveCmioRequest(ctx)
	if err != nil {
		return err
	}
	if req.Reason != CmioYieldManualReasonRxAccepted {
		return fmt.Errorf("the stored machine is yielded, but waiting on reason %d rather than an input (%d)", req.Reason, CmioYieldManualReasonRxAccepted)
	}
	return nil
}

// yieldLoop runs the machine after an input has been delivered, collecting the
// automatic yields until the guest parks at a manual yield. It is shared by
// AdvanceInput and Inspect, which differ only in how they classify emissions.
func (r *Remote) yieldLoop(ctx context.Context, start, maxCycles uint64, emit func(*cmioRequest), manual func(*cmioRequest)) (accepted bool, cycles uint64, err error) {
	for {
		br, err := r.run(ctx, start+maxCycles)
		if err != nil {
			return false, 0, err
		}
		switch br {
		case breakYieldedAuto:
			req, err := r.receiveCmioRequest(ctx)
			if err != nil {
				return false, 0, err
			}
			emit(req)
		case breakYieldedSoft, breakConsoleOutput, breakConsoleInput:
			continue
		case breakYieldedManual:
			req, err := r.receiveCmioRequest(ctx)
			if err != nil {
				return false, 0, err
			}
			end, err := r.readMcycle(ctx)
			if err != nil {
				return false, 0, err
			}
			manual(req)
			return req.Reason == CmioYieldManualReasonRxAccepted, end - start, nil
		case breakHalted:
			return false, 0, ErrHalted
		case breakReachedTarget, breakMcycleOverflow:
			return false, 0, ErrCycleLimit
		default:
			return false, 0, fmt.Errorf("unexpected break reason %q", br)
		}
	}
}

func (r *Remote) AdvanceInput(ctx context.Context, input []byte, maxCycles uint64) (*AdvanceResult, error) {
	start, err := r.readMcycle(ctx)
	if err != nil {
		return nil, err
	}
	// The pre-input root hash: the state a rejection reverts to, and — since
	// 0.21.0 — a value the machine verifies against its own root before
	// accepting the input at all.
	revert, err := r.RootHash(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.sendCmioResponse(ctx, CmioRxRequestAdvanceState, input, &revert); err != nil {
		return nil, err
	}
	res := &AdvanceResult{}
	accepted, cycles, err := r.yieldLoop(ctx, start, maxCycles, func(req *cmioRequest) {
		switch req.Reason {
		case CmioYieldAutomaticReasonTxOutput, CmioYieldAutomaticReasonTxReport:
			res.Outputs = append(res.Outputs, Output{Reason: req.Reason, Data: req.Data})
		}
	}, func(req *cmioRequest) {
		// The guest reports its own outputs Merkle root in the manual yield
		// that ends an input. It is not used as the commitment — the host
		// computes that itself so verifiers can re-derive it — but it is a
		// direct cross-check that the two agree.
		if len(req.Data) == len(res.GuestOutputsRoot) {
			copy(res.GuestOutputsRoot[:], req.Data)
		}
	})
	if err != nil {
		return nil, err
	}
	res.Accepted, res.Cycles = accepted, cycles
	return res, nil
}

// Inspect sends the query as an inspect-state CMIO request and collects the
// reports the guest emits in reply. The machine is left wherever the query
// took it, so the caller must be holding a fork it intends to discard.
func (r *Remote) Inspect(ctx context.Context, query []byte, maxCycles uint64) (*InspectResult, error) {
	start, err := r.readMcycle(ctx)
	if err != nil {
		return nil, err
	}
	// No revert_root_hash: 0.21.0 refuses one on anything but an advance
	// response, since the machine records no revert point for an inspect —
	// the rollback is the caller discarding its fork.
	if err := r.sendCmioResponse(ctx, CmioRxRequestInspectState, query, nil); err != nil {
		return nil, err
	}
	res := &InspectResult{}
	accepted, cycles, err := r.yieldLoop(ctx, start, maxCycles, func(req *cmioRequest) {
		// Inspect answers in reports; a tx-output during inspect would be a
		// guest bug, since nothing it emits can be proven.
		if req.Reason == CmioYieldAutomaticReasonTxReport {
			res.Reports = append(res.Reports, req.Data)
		}
	}, func(*cmioRequest) {})
	if err != nil {
		return nil, err
	}
	res.Accepted, res.Cycles = accepted, cycles
	return res, nil
}

// GuestOutputsRoot reads the outputs Merkle root the guest is reporting at the
// manual yield it is currently parked on. Guests built with Cartesi's guest
// tools publish it there; guests that do not will return a zero hash.
//
// The chain does not use this as its commitment — it computes that itself, so
// verifiers can re-derive it from re-execution — but it is a direct check that
// the host's tree and the guest's agree.
func (r *Remote) GuestOutputsRoot(ctx context.Context) (common.Hash, error) {
	req, err := r.receiveCmioRequest(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	if req.Cmd != CmioYieldCommandManual || len(req.Data) != common.HashLength {
		return common.Hash{}, nil
	}
	return common.BytesToHash(req.Data), nil
}

// ReadMemory copies bytes out of the loaded machine. The method is
// machine.read_memory {address, length}, the result base64 bytes — the same
// encoding style as every other buffer here. It executes nothing: the server
// memcpys from the machine's address space (in 0.21 it even reads across
// range boundaries, zero-filling gaps), and requests are processed
// sequentially, so a read against a parked machine is consistent by
// construction.
func (r *Remote) ReadMemory(ctx context.Context, address, length uint64) ([]byte, error) {
	var b binary
	if err := r.call(ctx, "machine.read_memory", map[string]any{"address": address, "length": length}, &b); err != nil {
		return nil, err
	}
	if uint64(len(b)) != length {
		return nil, fmt.Errorf("read_memory returned %d bytes, want %d", len(b), length)
	}
	return b, nil
}

// accountsDriveLabel is the label the accounts drive is declared under
// (docs/ACCOUNTS-DRIVE-SPEC.md §4), and accountsDriveDefaultStart the spec's
// recommended well-known start address — the classic PMA_DRIVE_START, which
// devnet/build-snapshot.ts pins explicitly.
const (
	accountsDriveLabel        = "accounts"
	accountsDriveDefaultStart = uint64(0x80000000000000)
	// abiDriveLabel is the ABI drive's label (docs/ABI-DRIVE-SPEC.md §5).
	abiDriveLabel = "abi"
)

// AccountsDriveStart discovers the accounts drive's start address, by label
// from the machine's initial config. found is false when the machine's config
// declares no drive labeled "accounts".
//
// The config route is best-effort: machine.get_initial_config's flash_drive
// entries carry label and start in 0.21, but unlike the hot-path methods in
// this file its shape is not pinned by a test against every guest, so a
// failure falls back to the spec's well-known start instead of erroring. The
// fallback is safe to guess with: a wrong address is caught downstream by the
// drive's magic check, so at worst "config unreadable" degrades into "drive
// unreadable", never into misread balances.
func (r *Remote) AccountsDriveStart(ctx context.Context) (start uint64, found bool, err error) {
	start, found = r.driveStart(ctx, accountsDriveLabel)
	if !found && !r.configKnown(ctx) {
		// No config to consult: fall back to the spec's well-known start.
		return accountsDriveDefaultStart, true, nil
	}
	return start, found, nil
}

// AbiDriveStart discovers the ABI drive (docs/ABI-DRIVE-SPEC.md) by its
// label. Unlike the accounts drive it has no well-known fallback address:
// a machine whose config does not declare it simply has none.
func (r *Remote) AbiDriveStart(ctx context.Context) (start uint64, found bool, err error) {
	start, found = r.driveStart(ctx, abiDriveLabel)
	return start, found, nil
}

func (r *Remote) configKnown(ctx context.Context) bool {
	var cfg struct{}
	return r.call(ctx, "machine.get_initial_config", nil, &cfg) == nil
}

func (r *Remote) driveStart(ctx context.Context, label string) (start uint64, found bool) {
	var cfg struct {
		FlashDrive []struct {
			Label string `json:"label"`
			Start uint64 `json:"start"`
		} `json:"flash_drive"`
	}
	if err := r.call(ctx, "machine.get_initial_config", nil, &cfg); err != nil {
		return 0, false
	}
	for _, d := range cfg.FlashDrive {
		if d.Label == label {
			return d.Start, true
		}
	}
	return 0, false
}

// MerkleProof is machine.get_proof's answer: the aligned range
// [TargetAddress, TargetAddress + 2^Log2TargetSize) of the machine's address
// space hashes to TargetHash, and folding TargetHash with the
// Log2RootSize − Log2TargetSize SiblingHashes reproduces RootHash — the same
// root the chain publishes as a block's stateRoot. Siblings are relayed in
// the order the emulator returns them.
type MerkleProof struct {
	TargetAddress  uint64
	Log2TargetSize uint64
	TargetHash     common.Hash
	Log2RootSize   uint64
	RootHash       common.Hash
	SiblingHashes  []common.Hash
}

// GetProof asks the server to prove an aligned power-of-two range of the
// machine's address space against its current root hash. The method is
// machine.get_proof {address, log2_target_size}, hashes base64 — the same
// call style as every other method here. Like ReadMemory it executes
// nothing; the proof machinery is incremental, so the cost is rehashing
// pages dirtied since the last tree update, not the whole tree.
func (r *Remote) GetProof(ctx context.Context, address, log2TargetSize uint64) (*MerkleProof, error) {
	var res struct {
		TargetAddress  uint64   `json:"target_address"`
		Log2TargetSize uint64   `json:"log2_target_size"`
		TargetHash     binary   `json:"target_hash"`
		Log2RootSize   uint64   `json:"log2_root_size"`
		RootHash       binary   `json:"root_hash"`
		SiblingHashes  []binary `json:"sibling_hashes"`
	}
	if err := r.call(ctx, "machine.get_proof", map[string]any{
		"address":          address,
		"log2_target_size": log2TargetSize,
	}, &res); err != nil {
		return nil, err
	}
	if len(res.TargetHash) != common.HashLength || len(res.RootHash) != common.HashLength {
		return nil, fmt.Errorf("get_proof: hash lengths %d/%d, want %d", len(res.TargetHash), len(res.RootHash), common.HashLength)
	}
	proof := &MerkleProof{
		TargetAddress:  res.TargetAddress,
		Log2TargetSize: res.Log2TargetSize,
		TargetHash:     common.BytesToHash(res.TargetHash),
		Log2RootSize:   res.Log2RootSize,
		RootHash:       common.BytesToHash(res.RootHash),
		SiblingHashes:  make([]common.Hash, 0, len(res.SiblingHashes)),
	}
	for i, s := range res.SiblingHashes {
		if len(s) != common.HashLength {
			return nil, fmt.Errorf("get_proof: sibling %d has %d bytes, want %d", i, len(s), common.HashLength)
		}
		proof.SiblingHashes = append(proof.SiblingHashes, common.BytesToHash(s))
	}
	return proof, nil
}

func (r *Remote) RootHash(ctx context.Context) (common.Hash, error) {
	var b binary
	if err := r.call(ctx, "machine.get_root_hash", nil, &b); err != nil {
		return common.Hash{}, err
	}
	if len(b) != common.HashLength {
		return common.Hash{}, fmt.Errorf("root hash has %d bytes, want %d", len(b), common.HashLength)
	}
	return common.BytesToHash(b), nil
}

func (r *Remote) Fork(ctx context.Context) (Machine, error) {
	var res struct {
		Address string `json:"address"`
		PID     uint64 `json:"pid"`
	}
	if err := r.call(ctx, "fork", nil, &res); err != nil {
		return nil, err
	}
	addr := res.Address
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Remote{endpoint: addr, client: r.client, owned: true}, nil
}

// Close shuts down a forked server. The server the operator started is left
// running: this package did not spawn it and must not take it down.
func (r *Remote) Close(ctx context.Context) error {
	if !r.owned {
		return nil
	}
	return r.call(ctx, "shutdown", nil, nil)
}
