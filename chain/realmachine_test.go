package chain

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tuler/op-cartesi/machine"
	"github.com/tuler/op-cartesi/mempool"
)

// These tests build real L2 blocks on a real Cartesi Machine, which is the
// milestone the whole shim exists for. They are skipped unless a stored
// machine is provided:
//
//	OP_CARTESI_TEST_SNAPSHOT=/path/to/stored/machine go test ./chain/
//
// The snapshot's guest must consume Cartesi EvmAdvance inputs, which is what a
// stock guest-tools rootfs does.
const realMachineEnv = "OP_CARTESI_TEST_SNAPSHOT"

func newRealChain(t *testing.T) *Chain {
	t.Helper()
	snapshot := os.Getenv(realMachineEnv)
	if snapshot == "" {
		t.Skipf("set %s to a stored machine directory to run against a real machine", realMachineEnv)
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
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	ctx := context.Background()
	var remote *machine.Remote
	deadline := time.Now().Add(20 * time.Second)
	for {
		remote, err = machine.DialRemote(ctx, "http://"+addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("emulator never became reachable: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := remote.Load(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := remote.CheckReady(ctx); err != nil {
		t.Fatalf("stored machine is not ready for input: %v", err)
	}

	c, err := New(ctx, testConfig(), remote, mempool.New(16))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The chain's genesis state root must be the stored machine's own root hash,
// with nothing run in between. That is what makes genesis a property of the
// snapshot rather than of how a particular node started up — two operators
// handed the same template must compute the same genesis block, and so the
// same rollup config, without having to agree on a boot procedure.
func TestGenesisIsTheStoredMachineRoot(t *testing.T) {
	ctx := context.Background()
	c := newRealChain(t)

	head, ok := c.machineAt(c.GenesisHash())
	if !ok {
		t.Fatal("no machine for genesis")
	}
	remote, ok := head.(*machine.Remote)
	if !ok {
		t.Skip("not running against a remote machine")
	}
	stored, err := remote.RootHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.BlockByNumber(0).Header.Root; got != stored {
		t.Fatalf("genesis state root %s, stored machine root %s", got, stored)
	}
	t.Logf("genesis state root is the snapshot's own root %s", stored)
}

// TestRealMachineBuildsBlocks is the end-to-end milestone: the sequencer path
// drives a real Cartesi Machine, and the resulting state root is the machine's
// own Merkle root rather than anything the shim invented.
func TestRealMachineBuildsBlocks(t *testing.T) {
	c := newRealChain(t)
	genesisRoot := c.HeadBlock().Header.Root
	if genesisRoot == (common.Hash{}) {
		t.Fatal("genesis state root is zero; the machine reported nothing")
	}

	var stateRoots, outputsRoots []common.Hash
	for i := range 2 {
		env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, string(rune('a'+i)))}, true))
		p := env.ExecutionPayload
		stateRoots = append(stateRoots, common.Hash(p.StateRoot))
		outputsRoots = append(outputsRoots, *p.WithdrawalsRoot)
		if p.GasUsed == 0 {
			t.Errorf("block %d: no gas, so the machine consumed no cycles", i+1)
		}
	}

	if stateRoots[0] == genesisRoot {
		t.Error("the machine's state did not change after the first block")
	}
	if stateRoots[0] == stateRoots[1] {
		t.Error("the machine's state did not change between blocks")
	}
	if outputsRoots[0] == outputsRoots[1] {
		t.Error("the outputs commitment did not advance; the guest emitted nothing")
	}
	t.Logf("state roots: %s -> %s", stateRoots[0], stateRoots[1])
	t.Logf("outputs roots: %s -> %s", outputsRoots[0], outputsRoots[1])
}

// The host's outputs commitment must equal the one the guest maintains. This
// is the check that would catch the input envelope or the tree drifting out of
// agreement with Cartesi's own implementation.
func TestRealMachineOutputsRootAgrees(t *testing.T) {
	ctx := context.Background()
	c := newRealChain(t)

	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "one")}, true))
	hostTree, ok := c.OutputTreeAt(env.ExecutionPayload.BlockHash)
	if !ok {
		t.Fatal("no accumulator for the block")
	}

	head, ok := c.machineAt(env.ExecutionPayload.BlockHash)
	if !ok {
		t.Fatal("no machine snapshot for the block")
	}
	remote, ok := head.(*machine.Remote)
	if !ok {
		t.Skip("not running against a remote machine")
	}
	guestRoot, err := remote.GuestOutputsRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if guestRoot == (common.Hash{}) {
		t.Skip("this guest does not report an outputs root")
	}
	if guestRoot != hostTree.Root() {
		t.Fatalf("guest committed %s, host computed %s", guestRoot, hostTree.Root())
	}
	t.Logf("guest and host agree on outputs root %s after %d outputs", guestRoot, hostTree.Count())
}

// A verifier that only re-executes the payload must reach the same state root
// as the sequencer that built it — on a real machine, with the real input
// envelope.
func TestRealMachineVerifierAgrees(t *testing.T) {
	ctx := context.Background()
	sequencer := newRealChain(t)
	verifier := newRealChain(t)

	if sequencer.GenesisHash() != verifier.GenesisHash() {
		t.Fatal("two nodes booting the same snapshot disagree on genesis")
	}

	env := buildBlock(sequencer, t, attrsOn(sequencer.HeadBlock(), [][]byte{depositTx(t, "shared")}, true))
	st, err := verifier.ImportPayload(ctx, env.ExecutionPayload, env.ParentBeaconBlockRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != engine.VALID {
		t.Fatalf("verifier rejected the sequencer's block: %s (%v)", st.Status, st.ValidationError)
	}
	t.Logf("verifier re-executed to the same state root %s", env.ExecutionPayload.StateRoot)
}

// Inspect must answer against the real machine without disturbing the chain.
func TestRealMachineInspect(t *testing.T) {
	ctx := context.Background()
	c := newRealChain(t)
	buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "one")}, true))

	head := c.HeadBlock()
	before := head.Header.Root

	res, err := c.Inspect(ctx, head.Hash(), []byte("query"))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	t.Logf("inspect accepted=%v cycles=%d reports=%d", res.Accepted, res.Cycles, len(res.Reports))

	if c.HeadBlock().Header.Root != before {
		t.Error("inspect changed the chain's state root")
	}
	// The chain must still build on top of the inspected head.
	buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{depositTx(t, "two")}, true))
	if c.HeadBlock().NumberU64() != 2 {
		t.Fatalf("head at %d after inspect, want 2", c.HeadBlock().NumberU64())
	}
}
