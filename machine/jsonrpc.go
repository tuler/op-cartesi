package machine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
)

// Remote drives a cartesi-jsonrpc-machine server (the emulator's remote
// machine protocol). Snapshots map onto the server's "fork" method, which
// spawns a copy-on-write child server, so Fork/Close are process lifecycle
// operations on the emulator side.
//
// Wire-format note: hash and byte-buffer encodings are decoded tolerantly
// (0x-hex, bare hex, or base64) because they have varied across emulator
// releases; pin and integration-test against the emulator version you deploy.
type Remote struct {
	endpoint string
	client   *http.Client
	reqID    atomic.Uint64
}

var _ Machine = (*Remote)(nil)

// DialRemote connects to a cartesi-jsonrpc-machine server that already has a
// machine loaded. endpoint is the server's HTTP base URL, e.g.
// "http://127.0.0.1:6000".
func DialRemote(ctx context.Context, endpoint string) (*Remote, error) {
	r := &Remote{endpoint: strings.TrimSuffix(endpoint, "/"), client: &http.Client{}}
	var version any
	if err := r.call(ctx, "get_version", nil, &version); err != nil {
		return nil, fmt.Errorf("machine server unreachable at %s: %w", endpoint, err)
	}
	return r, nil
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

// binary is a byte slice decoded tolerantly from hex or base64 JSON strings.
type binary []byte

func (b *binary) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	decoded, err := decodeBinaryString(s)
	if err != nil {
		return err
	}
	*b = decoded
	return nil
}

func decodeBinaryString(s string) ([]byte, error) {
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		return hex.DecodeString(h)
	}
	if b, err := hex.DecodeString(s); err == nil && len(s) > 0 && len(s)%2 == 0 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("undecodable binary string %q", s)
}

// breakReason normalizes the machine.run result across emulator releases
// (plain string vs {"break_reason": ...} object).
type breakReason string

const (
	breakHalted        breakReason = "halted"
	breakYieldedManual breakReason = "yielded_manually"
	breakYieldedAuto   breakReason = "yielded_automatically"
	breakYieldedSoft   breakReason = "yielded_softly"
	breakReachedTarget breakReason = "reached_target_mcycle"
	breakFailed        breakReason = "failed"
)

func (br *breakReason) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*br = breakReason(strings.ToLower(s))
		return nil
	}
	var obj struct {
		BreakReason string `json:"break_reason"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*br = breakReason(strings.ToLower(obj.BreakReason))
	return nil
}

func (r *Remote) run(ctx context.Context, mcycleEnd uint64) (breakReason, error) {
	var br breakReason
	err := r.call(ctx, "machine.run", map[string]any{"mcycle_end": mcycleEnd}, &br)
	return br, err
}

func (r *Remote) readMcycle(ctx context.Context) (uint64, error) {
	var v uint64
	err := r.call(ctx, "machine.read_reg", map[string]any{"reg": "mcycle"}, &v)
	return v, err
}

type cmioRequest struct {
	Cmd    uint8  `json:"cmd"`
	Reason uint16 `json:"reason"`
	Data   binary `json:"data"`
}

func (r *Remote) receiveCmioRequest(ctx context.Context) (*cmioRequest, error) {
	var req cmioRequest
	err := r.call(ctx, "machine.receive_cmio_request", nil, &req)
	return &req, err
}

func (r *Remote) sendCmioResponse(ctx context.Context, reason uint16, data []byte) error {
	return r.call(ctx, "machine.send_cmio_response", map[string]any{
		"reason": reason,
		"data":   base64.StdEncoding.EncodeToString(data),
	}, nil)
}

// EnsureReady runs the machine (e.g. through Linux boot) until it parks at a
// manual yield waiting for its first input. It must be called once before the
// machine is handed to the chain.
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
		case breakYieldedAuto, breakYieldedSoft:
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

func (r *Remote) AdvanceInput(ctx context.Context, input []byte, maxCycles uint64) (*AdvanceResult, error) {
	start, err := r.readMcycle(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.sendCmioResponse(ctx, CmioRxRequestAdvanceState, input); err != nil {
		return nil, err
	}
	res := &AdvanceResult{}
	for {
		br, err := r.run(ctx, start+maxCycles)
		if err != nil {
			return nil, err
		}
		switch br {
		case breakYieldedAuto:
			req, err := r.receiveCmioRequest(ctx)
			if err != nil {
				return nil, err
			}
			switch req.Reason {
			case CmioYieldAutomaticReasonTxOutput, CmioYieldAutomaticReasonTxReport:
				res.Outputs = append(res.Outputs, Output{Reason: req.Reason, Data: req.Data})
			}
		case breakYieldedSoft:
			continue
		case breakYieldedManual:
			req, err := r.receiveCmioRequest(ctx)
			if err != nil {
				return nil, err
			}
			res.Accepted = req.Reason == CmioYieldManualReasonRxAccepted
			end, err := r.readMcycle(ctx)
			if err != nil {
				return nil, err
			}
			res.Cycles = end - start
			return res, nil
		case breakHalted:
			return nil, ErrHalted
		case breakReachedTarget:
			return nil, ErrCycleLimit
		default:
			return nil, fmt.Errorf("unexpected break reason %q", br)
		}
	}
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
	return &Remote{endpoint: addr, client: r.client}, nil
}

func (r *Remote) Close(ctx context.Context) error {
	return r.call(ctx, "shutdown", nil, nil)
}
