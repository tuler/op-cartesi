package chain

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// bankMachineEnv points at a machine whose guest keeps a balance ledger and
// credits OP deposits into it — devnet/bank-app.sh:
//
//	SNAPSHOT_DIR=/tmp/bank GUEST_APP=$PWD/devnet/bank-app.sh ./devnet/build-snapshot.sh
//	OP_CARTESI_TEST_BANK_SNAPSHOT=/tmp/bank go test ./chain/
//
// It is separate from OP_CARTESI_TEST_SNAPSHOT because the other real-machine
// tests only need a guest that consumes inputs, while these need one that
// means something by them.
const bankMachineEnv = "OP_CARTESI_TEST_BANK_SNAPSHOT"

func newBankChain(t *testing.T) *Chain {
	t.Helper()
	if os.Getenv(bankMachineEnv) == "" {
		t.Skipf("set %s to a stored machine running devnet/bank-app.sh", bankMachineEnv)
	}
	t.Setenv(realMachineEnv, os.Getenv(bankMachineEnv))
	return newRealChain(t)
}

// ethDeposit is the L2 transaction op-node derives from an L1
// TransactionDeposited log: an ETH deposit mints `amount` and hands it to the
// recipient.
func ethDeposit(t *testing.T, source string, to common.Address, amount *big.Int) []byte {
	t.Helper()
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte(source)),
		From:       common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"),
		To:         &to,
		Mint:       amount,
		Value:      amount,
		Gas:        1_000_000,
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// balanceOf asks the guest for an address's balance the way a user would: an
// eth_call, which the chain answers by running inspect on a discarded fork.
func balanceOf(t *testing.T, c *Chain, at common.Hash, addr common.Address) *big.Int {
	t.Helper()
	res, err := c.Inspect(context.Background(), at, addr.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted {
		t.Fatal("the guest rejected the balance query")
	}
	var out []byte
	for _, r := range res.Reports {
		out = append(out, r...)
	}
	return new(big.Int).SetBytes(out)
}

// unwrapNotice strips Cartesi's Notice(bytes) envelope: a 4-byte selector,
// then the ABI head (offset, length) and the padded payload.
func unwrapNotice(t *testing.T, output []byte) []byte {
	t.Helper()
	if len(output) < 4+64 {
		t.Fatalf("output is %d bytes, too short to be a notice", len(output))
	}
	body := output[4:]
	offset := new(big.Int).SetBytes(body[:32]).Uint64()
	length := new(big.Int).SetBytes(body[offset : offset+32]).Uint64()
	start := offset + 32
	if uint64(len(body)) < start+length {
		t.Fatalf("notice claims %d payload bytes but only %d follow", length, uint64(len(body))-start)
	}
	return body[start : start+length]
}

// TestDepositIsCreditedInGuest is milestone 1's first half: a deposit that
// arrives from L1 is credited by the execution layer itself. The balance lives
// in the machine, so it is committed to by the state root — the shim holds no
// ledger of its own and could not fake this.
func TestDepositIsCreditedInGuest(t *testing.T) {
	c := newBankChain(t)
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	oneEther := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	if got := balanceOf(t, c, c.HeadBlock().Hash(), alice); got.Sign() != 0 {
		t.Fatalf("alice starts with %s, want 0", got)
	}

	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		ethDeposit(t, "first", alice, oneEther),
	}, true))

	if got := balanceOf(t, c, env.ExecutionPayload.BlockHash, alice); got.Cmp(oneEther) != 0 {
		t.Fatalf("after a 1 ETH deposit alice holds %s, want %s", got, oneEther)
	}

	// A second deposit must add to the first rather than replace it, which is
	// what proves the ledger is state and not a reply computed per input.
	env = buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		ethDeposit(t, "second", alice, oneEther),
	}, true))

	want := new(big.Int).Mul(oneEther, big.NewInt(2))
	if got := balanceOf(t, c, env.ExecutionPayload.BlockHash, alice); got.Cmp(want) != 0 {
		t.Fatalf("after two 1 ETH deposits alice holds %s, want %s", got, want)
	}
}

// The credit must also be provable, not just readable: the guest emits a
// notice for it, so it enters the outputs tree and therefore the block's
// withdrawals root. A verifier re-executing the block re-derives that root, so
// a sequencer cannot credit an account without the chain committing to it.
func TestDepositCreditIsCommittedTo(t *testing.T) {
	c := newBankChain(t)
	bob := common.HexToAddress("0x000000000000000000000000000000000000b0b0")
	amount := big.NewInt(12345)

	before := *c.HeadBlock().Header.WithdrawalsHash
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		ethDeposit(t, "bob", bob, amount),
	}, true))

	if *env.ExecutionPayload.WithdrawalsRoot == before {
		t.Fatal("the outputs commitment did not move; the credit was not committed to")
	}

	receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(receipts))
	}
	if len(receipts[0].Logs) != 1 {
		t.Fatalf("the deposit produced %d logs, want the one notice", len(receipts[0].Logs))
	}
	// A log carries the output exactly as the machine emitted it, which for a
	// notice is Cartesi's own envelope: the Notice(bytes) selector, then the
	// ABI encoding of the payload. Unwrapping it here rather than in the chain
	// is deliberate — the chain commits to the bytes the guest produced, and
	// interpreting them is the reader's business.
	data := unwrapNotice(t, receipts[0].Logs[0].Data)
	// The payload is address ‖ amount ‖ new balance, each a 32-byte word.
	if len(data) != 96 {
		t.Fatalf("notice payload is %d bytes, want 96", len(data))
	}
	if got := common.BytesToAddress(data[:32]); got != bob {
		t.Errorf("notice credits %s, want %s", got, bob)
	}
	if got := new(big.Int).SetBytes(data[32:64]); got.Cmp(amount) != 0 {
		t.Errorf("notice amount %s, want %s", got, amount)
	}
	if got := new(big.Int).SetBytes(data[64:]); got.Cmp(amount) != 0 {
		t.Errorf("notice balance %s, want %s", got, amount)
	}
}
