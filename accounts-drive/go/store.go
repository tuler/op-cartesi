package drive

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
)

// Store is the byte-level access the library needs. Offsets are relative to
// the drive's first byte. Implementations decide what backs it: a byte slice,
// a device file inside the guest, or a remote machine's memory on the host.
type Store interface {
	ReadAt(off uint64, p []byte) error
	WriteAt(off uint64, p []byte) error
}

// ErrReadOnly is returned by stores that cannot write (host-side reads).
var ErrReadOnly = errors.New("store is read-only")

// MemStore is an in-memory drive image: the whole drive as one byte slice.
type MemStore struct{ B []byte }

// NewMemStore allocates a zeroed drive image of the given length.
func NewMemStore(length uint64) *MemStore { return &MemStore{B: make([]byte, length)} }

func (m *MemStore) bounds(off uint64, n int) error {
	if off+uint64(n) > uint64(len(m.B)) || off+uint64(n) < off {
		return fmt.Errorf("access [%d,%d) outside drive of %d bytes", off, off+uint64(n), len(m.B))
	}
	return nil
}

func (m *MemStore) ReadAt(off uint64, p []byte) error {
	if err := m.bounds(off, len(p)); err != nil {
		return err
	}
	copy(p, m.B[off:])
	return nil
}

func (m *MemStore) WriteAt(off uint64, p []byte) error {
	if err := m.bounds(off, len(p)); err != nil {
		return err
	}
	copy(m.B[off:], p)
	return nil
}

// FileStore accesses the drive through a file — in the guest, the raw device
// (/dev/pmemN). Guests on a pmem-backed drive MUST call Sync before finishing
// each input (spec §10): file I/O goes through the guest kernel's page cache,
// and un-synced bytes are not in machine state at the yield.
type FileStore struct{ F *os.File }

// OpenFileStore opens a device or image file read-write.
func OpenFileStore(path string) (*FileStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &FileStore{F: f}, nil
}

func (s *FileStore) ReadAt(off uint64, p []byte) error {
	_, err := s.F.ReadAt(p, int64(off))
	if err == io.EOF && len(p) == 0 {
		return nil
	}
	return err
}

func (s *FileStore) WriteAt(off uint64, p []byte) error {
	_, err := s.F.WriteAt(p, int64(off))
	return err
}

// Sync flushes written bytes to the device.
func (s *FileStore) Sync() error { return s.F.Sync() }

// ReadMemoryStore reads the drive out of a live Cartesi Machine through the
// cartesi-jsonrpc-machine protocol's machine.read_memory — a pure state read
// that never executes the machine. It is read-only, and per spec §11 it must
// only be used against a quiescent (parked or stored) machine.
type ReadMemoryStore struct {
	// Endpoint is the machine server's JSON-RPC URL.
	Endpoint string
	// Base is the drive's start address in the machine's address space.
	Base   uint64
	Client *http.Client
	reqID  atomic.Uint64
}

func (r *ReadMemoryStore) ReadAt(off uint64, p []byte) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": r.reqID.Add(1), "method": "machine.read_memory",
		"params": map[string]uint64{"address": r.Base + off, "length": uint64(len(p))},
	})
	if err != nil {
		return err
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Post(r.Endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var rr struct {
		Result string `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rr); err != nil {
		return fmt.Errorf("machine.read_memory: bad response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("machine.read_memory: %s (code %d)", rr.Error.Message, rr.Error.Code)
	}
	data, err := base64.StdEncoding.DecodeString(rr.Result)
	if err != nil {
		return fmt.Errorf("machine.read_memory: %w", err)
	}
	if len(data) != len(p) {
		return fmt.Errorf("machine.read_memory: got %d bytes, want %d", len(data), len(p))
	}
	copy(p, data)
	return nil
}

func (r *ReadMemoryStore) WriteAt(uint64, []byte) error { return ErrReadOnly }
