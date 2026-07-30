package machine

import (
	"context"
	"math/big"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// These tests drive a real cartesi-jsonrpc-machine server, which is how the
// wire format in jsonrpc.go is pinned to an emulator release rather than
// assumed. They are skipped unless a machine snapshot is provided:
//
//	OP_CARTESI_TEST_SNAPSHOT=/path/to/stored/machine go test ./machine/
//
// The snapshot must be a rolling template — a machine whose guest consumes
// CMIO inputs — stored by `cartesi-machine --store=<dir>`.
const snapshotEnv = "OP_CARTESI_TEST_SNAPSHOT"

// startServer launches an emulator server on a free port and loads the
// snapshot into it.
func startServer(t *testing.T) *Remote {
	t.Helper()
	snapshot := os.Getenv(snapshotEnv)
	if snapshot == "" {
		t.Skipf("set %s to a stored machine directory to run the emulator tests", snapshotEnv)
	}
	binary, err := exec.LookPath("cartesi-jsonrpc-machine")
	if err != nil {
		t.Skip("cartesi-jsonrpc-machine is not on PATH")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()

	cmd := exec.Command(binary, "--server-address="+addr)
	// The server's output is discarded rather than inherited: forked child
	// servers inherit these descriptors and outlive the parent, so wiring them
	// to the test's stderr leaves the test binary waiting on I/O after the
	// tests have already passed.
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	ctx := context.Background()
	var remote *Remote
	deadline := time.Now().Add(20 * time.Second)
	for {
		remote, err = DialRemote(ctx, "http://"+addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("emulator server never became reachable: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if loaded, err := remote.Loaded(ctx); err != nil {
		t.Fatal(err)
	} else if loaded {
		t.Fatal("a fresh server should hold no machine")
	}
	if err := remote.Load(ctx, snapshot); err != nil {
		t.Fatalf("loading %s: %v", snapshot, err)
	}
	if loaded, err := remote.Loaded(ctx); err != nil || !loaded {
		t.Fatalf("machine not loaded after machine.load (err=%v)", err)
	}
	return remote
}

// evmAdvance ABI-encodes an input the way Cartesi's guest tools expect to
// receive it, so a stock guest-tools rootfs can decode it.
//
// Note this is the encoding the *guest* expects, not the one op-cartesi's
// chain currently feeds: the chain passes raw Ethereum transaction bytes, on
// the assumption that the guest decodes them itself. Bridging those two is
// guest-side work, and it is why this test encodes explicitly.
func evmAdvance(t *testing.T, index uint64, payload []byte) []byte {
	t.Helper()
	uint256Ty, _ := abi.NewType("uint256", "", nil)
	addressTy, _ := abi.NewType("address", "", nil)
	bytesTy, _ := abi.NewType("bytes", "", nil)
	args := abi.Arguments{
		{Type: uint256Ty}, // chainId
		{Type: addressTy}, // appContract
		{Type: addressTy}, // msgSender
		{Type: uint256Ty}, // blockNumber
		{Type: uint256Ty}, // blockTimestamp
		{Type: uint256Ty}, // prevRandao
		{Type: uint256Ty}, // index
		{Type: bytesTy},   // payload
	}
	packed, err := args.Pack(
		big.NewInt(901),
		common.HexToAddress("0x0000000000000000000000000000000000000001"),
		common.HexToAddress("0x0000000000000000000000000000000000000002"),
		big.NewInt(1), big.NewInt(1700000000), big.NewInt(0),
		new(big.Int).SetUint64(index),
		payload,
	)
	if err != nil {
		t.Fatal(err)
	}
	selector := crypto.Keccak256([]byte("EvmAdvance(uint256,address,address,uint256,uint256,uint256,uint256,bytes)"))[:4]
	return append(selector, packed...)
}

// emptyOutputsTreeRoot is the root of an empty Cartesi outputs Merkle tree.
// The guest reports its own outputs root in the manual yield that ends an
// input, and this constant is what chain.NewOutputTree().Root() computes
// independently on the host. Pinning them here is what proves the host-side
// tree in chain/outputtree.go really is Cartesi's tree; it is written out
// rather than imported because chain depends on this package, not the reverse.
const emptyOutputsTreeRoot = "0x0a162946e56158bac0673e6dd3bdfdc1e4a0e7744a120fdb640050c8d7abe1c6"

// The guest's own outputs commitment must match the one the host computes. If
// these ever diverge, the chain would publish a withdrawals root that no
// Cartesi output proof could satisfy.
func TestRemoteOutputsRootMatchesHostTree(t *testing.T) {
	ctx := context.Background()
	remote := startServer(t)
	if err := remote.EnsureReady(ctx, 10_000_000_000); err != nil {
		t.Fatal(err)
	}

	// Parked at its first input yield, the guest has produced no outputs, so
	// it reports the empty tree — which must be the root the host computes.
	atBoot, err := remote.GuestOutputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if atBoot == (common.Hash{}) {
		t.Skip("this guest does not report an outputs root")
	}
	if atBoot.Hex() != emptyOutputsTreeRoot {
		t.Fatalf("guest reports empty outputs root %s, host computes %s", atBoot, emptyOutputsTreeRoot)
	}

	// After an input that emits an output, the guest's root must move.
	res, err := remote.AdvanceInput(ctx, evmAdvance(t, 0, []byte("first")), 10_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if res.GuestOutputsRoot == atBoot {
		t.Error("the guest's outputs root did not change after emitting an output")
	}
	t.Logf("guest outputs root: empty %s -> after one output %s", atBoot, res.GuestOutputsRoot)
}

// The machine must boot to its first input yield and report a stable root
// hash. This exercises machine.run, machine.read_register and
// machine.get_root_hash against the real server.
func TestRemoteBootsAndReportsRootHash(t *testing.T) {
	ctx := context.Background()
	remote := startServer(t)

	if err := remote.EnsureReady(ctx, 10_000_000_000); err != nil {
		t.Fatalf("booting to first input yield: %v", err)
	}
	root, err := remote.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if root == (common.Hash{}) {
		t.Fatal("root hash is zero")
	}
	// Reading it again must not change it: get_root_hash is a pure read.
	again, err := remote.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != root {
		t.Fatalf("root hash changed between reads: %s then %s", root, again)
	}
	t.Logf("booted machine root hash %s", root)
}

// Feeding an input must advance the state and surface the guest's emissions.
// This is the full CMIO advance loop — send_cmio_response with a revert hash,
// then receive_cmio_request for each yield.
func TestRemoteAdvanceInput(t *testing.T) {
	ctx := context.Background()
	remote := startServer(t)
	if err := remote.EnsureReady(ctx, 10_000_000_000); err != nil {
		t.Fatal(err)
	}
	before, err := remote.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}

	res, err := remote.AdvanceInput(ctx, evmAdvance(t, 0, []byte("hello machine")), 10_000_000_000)
	if err != nil {
		t.Fatalf("advancing: %v", err)
	}
	if !res.Accepted {
		t.Error("the guest rejected the input")
	}
	if res.Cycles == 0 {
		t.Error("no cycles consumed")
	}
	after, err := remote.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("state root did not change after consuming an input")
	}

	var outputs, reports int
	for _, e := range res.Outputs {
		switch {
		case e.IsProvable():
			outputs++
		case e.IsReport():
			reports++
		}
	}
	t.Logf("advance consumed %d cycles, emitted %d outputs and %d reports", res.Cycles, outputs, reports)
	if outputs == 0 && reports == 0 {
		t.Error("the guest emitted nothing; the app may not be running")
	}
}

// Forking must produce an independent machine: advancing the fork leaves the
// parent untouched. This is the property the chain's block building and reorg
// handling rest on.
func TestRemoteForkIsolation(t *testing.T) {
	ctx := context.Background()
	parent := startServer(t)
	if err := parent.EnsureReady(ctx, 10_000_000_000); err != nil {
		t.Fatal(err)
	}
	before, err := parent.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}

	child, err := parent.Fork(ctx)
	if err != nil {
		t.Fatalf("forking: %v", err)
	}
	defer child.Close(ctx)

	childRoot, err := child.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if childRoot != before {
		t.Fatalf("fork started at %s, want the parent's %s", childRoot, before)
	}
	if _, err := child.AdvanceInput(ctx, evmAdvance(t, 0, []byte("only in the fork")), 10_000_000_000); err != nil {
		t.Fatal(err)
	}

	parentAfter, err := parent.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter != before {
		t.Error("advancing the fork changed the parent's state")
	}
	forkAfter, err := child.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if forkAfter == before {
		t.Error("the fork's state did not advance")
	}
}

// The same inputs from the same state must produce the same root hash on
// independent machines. Without this the chain cannot be verified by anyone.
func TestRemoteDeterminism(t *testing.T) {
	ctx := context.Background()
	remote := startServer(t)
	if err := remote.EnsureReady(ctx, 10_000_000_000); err != nil {
		t.Fatal(err)
	}

	run := func() common.Hash {
		fork, err := remote.Fork(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer fork.Close(ctx)
		for i := range 2 {
			if _, err := fork.AdvanceInput(ctx, evmAdvance(t, uint64(i), []byte("deterministic")), 10_000_000_000); err != nil {
				t.Fatal(err)
			}
		}
		root, err := fork.RootHash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}

	first, second := run(), run()
	if first != second {
		t.Fatalf("same inputs produced different roots: %s vs %s", first, second)
	}
	t.Logf("two independent runs agreed on %s", first)
}

// Close must shut down a fork's server process but leave the operator's own
// server running.
func TestRemoteCloseOnlyShutsDownForks(t *testing.T) {
	ctx := context.Background()
	remote := startServer(t)

	if err := remote.Close(ctx); err != nil {
		t.Fatalf("closing the dialed server errored: %v", err)
	}
	if loaded, err := remote.Loaded(ctx); err != nil {
		t.Fatalf("the operator's server was shut down by Close: %v", err)
	} else if !loaded {
		t.Error("the operator's machine was unloaded")
	}

	fork, err := remote.Fork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.Close(ctx); err != nil {
		t.Fatalf("closing a fork: %v", err)
	}
	// The fork's server is gone, so any further call must fail.
	if _, err := fork.RootHash(ctx); err == nil {
		t.Error("the fork's server is still answering after Close")
	} else if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "EOF") &&
		!strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "reset") {
		t.Logf("fork closed with: %v", err)
	}
}
