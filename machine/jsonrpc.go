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
// machine protocol), pinned to the protocol of machine-emulator 0.21
// (JSON-RPC "get_version" reports 0.6.x).
//
// The wire format was established by probing a running server, not by reading
// its rpc.discover schema: in 0.21.0-test7 the schema advertises methods the
// build does not implement (machine.read_register, is_empty), so it cannot be
// trusted on its own. Byte buffers and hashes are base64, registers are read
// through machine.read_reg, cmio requests are fetched with an explicit length,
// and responses carry the root hash to revert to on rejection.
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
// The natural method for this is is_empty, which the server advertises in its
// rpc.discover schema but does not implement in 0.21.0-test7 (it answers
// method-not-found). So the check probes with a harmless machine read
// instead: a server with no machine answers a distinct "no machine" error.
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

// readMcycle reads the cycle counter.
//
// The method is machine.read_reg. The server's rpc.discover schema advertises
// machine.read_register instead, but 0.21.0-test7 does not implement that name
// — the schema is ahead of the build in a few places, so every method here was
// probed against a running server rather than taken from the schema.
func (r *Remote) readMcycle(ctx context.Context) (uint64, error) {
	var v uint64
	err := r.call(ctx, "machine.read_reg", map[string]any{"reg": "mcycle"}, &v)
	return v, err
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

// sendCmioResponse hands the guest its input. revertRootHash is the state the
// emulator returns to if the guest ends up rejecting this input, which is what
// makes a rejection leave no trace.
func (r *Remote) sendCmioResponse(ctx context.Context, reason uint16, data []byte, revertRootHash common.Hash) error {
	return r.call(ctx, "machine.send_cmio_response", map[string]any{
		"revert_root_hash": base64.StdEncoding.EncodeToString(revertRootHash.Bytes()),
		"reason":           reason,
		"data":             base64.StdEncoding.EncodeToString(data),
	}, nil)
}

// EnsureReady runs the machine (through Linux boot, for a stored template
// captured before boot) until it parks at a manual yield waiting for its first
// input. It must be called once before the machine is handed to the chain.
func (r *Remote) EnsureReady(ctx context.Context, maxCycles uint64) error {
	start, err := r.readMcycle(ctx)
	if err != nil {
		return err
	}
	for {
		br, err := r.run(ctx, start+maxCycles)
		if err != nil {
			return err
		}
		switch br {
		case breakYieldedManual:
			return nil
		case breakYieldedAuto, breakYieldedSoft, breakConsoleOutput, breakConsoleInput:
			continue
		case breakHalted:
			return ErrHalted
		case breakReachedTarget:
			return ErrCycleLimit
		default:
			return fmt.Errorf("unexpected break reason %q during boot", br)
		}
	}
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
	revert, err := r.RootHash(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.sendCmioResponse(ctx, CmioRxRequestAdvanceState, input, revert); err != nil {
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
	revert, err := r.RootHash(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.sendCmioResponse(ctx, CmioRxRequestInspectState, query, revert); err != nil {
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
