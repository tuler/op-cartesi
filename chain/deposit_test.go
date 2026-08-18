package chain

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// ledgerMachineEnv points at a machine running the devnet guest — the routed
// guest of demo/ (docs/EVM-COMPAT.md), which keeps its ledger on the
// accounts drive and routes every input by its `to` address:
//
//	./scripts/build-snapshot.ts
//	OP_CARTESI_TEST_LEDGER_SNAPSHOT=./demo/.cartesi/image go test ./chain/
//
// It is separate from OP_CARTESI_TEST_SNAPSHOT because the other real-machine
// tests only need a guest that consumes inputs, while these need one that
// means something by them.
const ledgerMachineEnv = "OP_CARTESI_TEST_LEDGER_SNAPSHOT"

func newLedgerChain(t *testing.T) *Chain {
	t.Helper()
	if os.Getenv(ledgerMachineEnv) == "" {
		t.Skipf("set %s to a stored machine running the routed guest of demo/", ledgerMachineEnv)
	}
	t.Setenv(realMachineEnv, os.Getenv(ledgerMachineEnv))
	return newRealChain(t)
}

// The routed guest's standard addresses and genesis parameters, as the
// devnet snapshot bakes them (demo/ defaults, matching devnet/lib/env.ts).
var (
	guestConfigAddress = common.HexToAddress("0xc751000000000000000000000000000000000002")
	guestOwner         = common.HexToAddress("0x90F79bf6EB2c4f870365E785982E1f101E93b906")
)

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

// registerPortalDeposit is an owner configuration input: a deposit from the
// guest owner's EOA to the config contract carrying
// registerPortal(uint8 kind, address portal) — how the devnet's
// deploy-outputs.ts tells the guest which L1 contracts to credit deposits
// from. It is also the one input a fresh routed guest answers with a
// provable output (the registration notice), which several real-machine
// tests lean on.
func registerPortalDeposit(t *testing.T, source string, kind uint8, portal common.Address) []byte {
	t.Helper()
	uint8Ty, err := abi.NewType("uint8", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	addressTy, err := abi.NewType("address", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := (abi.Arguments{{Type: uint8Ty}, {Type: addressTy}}).Pack(kind, portal)
	if err != nil {
		t.Fatal(err)
	}
	selector := crypto.Keccak256([]byte("registerPortal(uint8,address)"))[:4]
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte(source)),
		From:       guestOwner,
		To:         &guestConfigAddress,
		Mint:       new(big.Int),
		Value:      new(big.Int),
		Gas:        1_000_000,
		Data:       append(selector, packed...),
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// aliasL1 applies OP's L1-to-L2 contract aliasing, which is how a portal's
// deposits arrive at the guest.
func aliasL1(addr common.Address) common.Address {
	offset, _ := new(big.Int).SetString("1111000000000000000000000000000000001111", 16)
	aliased := new(big.Int).Add(addr.Big(), offset)
	return common.BigToAddress(aliased) // BigToAddress truncates to 160 bits
}

// erc20PortalDeposit is what OPERC20Portal's escrow becomes on L2: a deposit
// from the aliased portal whose data is InputEncoding's packed
// token ‖ beneficiary ‖ amount. The `to` is deliberately not anything the
// guest knows — a registered portal routes by sender (EVM-COMPAT §6).
func erc20PortalDeposit(t *testing.T, source string, portal, token, beneficiary common.Address, amount *big.Int) []byte {
	t.Helper()
	appContract := common.HexToAddress("0x00000000000000000000000000000000000a99c0")
	data := append(append(append([]byte{}, token.Bytes()...), beneficiary.Bytes()...), common.BigToHash(amount).Bytes()...)
	from := aliasL1(portal)
	tx := types.NewTx(&types.DepositTx{
		SourceHash: crypto.Keccak256Hash([]byte(source)),
		From:       from,
		To:         &appContract,
		Mint:       new(big.Int),
		Value:      new(big.Int),
		Gas:        1_000_000,
		Data:       data,
	})
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// l2TokenAddress is the exported derivation, under the name the assertions
// grew up with.
func l2TokenAddress(l1Token common.Address) common.Address {
	return L2TokenAddress(l1Token)
}

// etherBalanceAt reads an account's native balance the way eth_getBalance
// does: straight off the accounts drive, no execution, no dialect.
func etherBalanceAt(t *testing.T, c *Chain, at common.Hash, addr common.Address) *big.Int {
	t.Helper()
	_, balance, err := c.AccountAt(context.Background(), at, addr)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

// erc20BalanceOf asks the token façade the way eth_call now does: the
// EvmCall envelope around balanceOf, answered with a return-tagged report.
func erc20BalanceOf(t *testing.T, c *Chain, at common.Hash, l1Token, holder common.Address) *big.Int {
	t.Helper()
	data := append(crypto.Keccak256([]byte("balanceOf(address)"))[:4], common.LeftPadBytes(holder.Bytes(), 32)...)
	envelope, err := EncodeEvmCall(c.Config().ChainID, holder, l2TokenAddress(l1Token), nil, data)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Inspect(context.Background(), at, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted {
		t.Fatalf("the guest rejected the balanceOf call: reports %x", res.Reports)
	}
	if len(res.Reports) != 1 || len(res.Reports[0]) < 1 || res.Reports[0][0] != ReportTagReturn {
		t.Fatalf("balanceOf answered with reports %x, want one return-tagged report", res.Reports)
	}
	return new(big.Int).SetBytes(res.Reports[0][1:])
}

// TestDepositIsCreditedInGuest is milestone 1's first half: a deposit that
// arrives from L1 is credited by the execution layer itself. The balance
// lives on the guest's accounts drive, so it is committed to by the state
// root — the shim holds no ledger of its own and could not fake this — and
// read back the way eth_getBalance reads it, one machine memory read.
func TestDepositIsCreditedInGuest(t *testing.T) {
	c := newLedgerChain(t)
	alice := common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	oneEther := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	if got := etherBalanceAt(t, c, c.HeadBlock().Hash(), alice); got.Sign() != 0 {
		t.Fatalf("alice starts with %s, want 0", got)
	}

	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		ethDeposit(t, "first", alice, oneEther),
	}, true))

	if got := etherBalanceAt(t, c, env.ExecutionPayload.BlockHash, alice); got.Cmp(oneEther) != 0 {
		t.Fatalf("after a 1 ETH deposit alice holds %s, want %s", got, oneEther)
	}

	// A second deposit must add to the first rather than replace it, which is
	// what proves the ledger is state and not a reply computed per input.
	env = buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		ethDeposit(t, "second", alice, oneEther),
	}, true))

	want := new(big.Int).Mul(oneEther, big.NewInt(2))
	if got := etherBalanceAt(t, c, env.ExecutionPayload.BlockHash, alice); got.Cmp(want) != 0 {
		t.Fatalf("after two 1 ETH deposits alice holds %s, want %s", got, want)
	}
}

// The token path must also be provable, not just readable: the registration
// and the portal credit each emit an EvmLog notice, so they enter the
// outputs tree and therefore the block's withdrawals root. A verifier
// re-executing the block re-derives that root, so a sequencer cannot credit
// an account without the chain committing to it.
func TestPortalDepositIsCommittedTo(t *testing.T) {
	c := newLedgerChain(t)
	portal := common.HexToAddress("0x0000000000000000000000000000000000907a70")
	token := common.HexToAddress("0x000000000000000000000000000000000070ce20")
	bob := common.HexToAddress("0x000000000000000000000000000000000000b0b0")
	amount := big.NewInt(12345)

	// 1. The owner registers the portal; the registration is consensus
	// state, so it travels as a notice and moves the outputs commitment.
	before := *c.HeadBlock().Header.WithdrawalsHash
	env := buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		registerPortalDeposit(t, "register", 1, portal),
	}, true))
	if *env.ExecutionPayload.WithdrawalsRoot == before {
		t.Fatal("the outputs commitment did not move; the registration was not committed to")
	}

	// 2. A deposit from the (aliased) portal credits the beneficiary and
	// emits the mint Transfer event — an EvmLog notice from the token's
	// derived façade address.
	env = buildBlock(c, t, attrsOn(c.HeadBlock(), [][]byte{
		erc20PortalDeposit(t, "deposit", portal, token, bob, amount),
	}, true))

	receipts := c.BlockReceipts(env.ExecutionPayload.BlockHash)
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(receipts))
	}
	if len(receipts[0].Logs) != 1 {
		t.Fatalf("the deposit produced %d logs, want the one Transfer notice", len(receipts[0].Logs))
	}
	// Receipt synthesis decodes the guest's EvmLog notice into a real log —
	// the guest's own emitter, topics and data — so what a wallet or indexer
	// reads here is an ordinary Transfer event. The chain still commits to
	// the raw bytes the guest produced (they are the outputs-tree leaf);
	// only the receipt view is decoded, and nothing about it is consensus.
	log := receipts[0].Logs[0]
	emitter, topics, data := log.Address, log.Topics, log.Data
	if want := l2TokenAddress(token); emitter != want {
		t.Errorf("event emitter %s, want the derived façade %s", emitter, want)
	}
	transferTopic := common.BytesToHash(crypto.Keccak256([]byte("Transfer(address,address,uint256)")))
	if len(topics) != 3 || topics[0] != transferTopic {
		t.Fatalf("topics %v, want Transfer(from,to,value)", topics)
	}
	if topics[1] != (common.Hash{}) {
		t.Errorf("Transfer from %s, want the zero address (a mint)", topics[1])
	}
	if got := common.BytesToAddress(topics[2].Bytes()); got != bob {
		t.Errorf("Transfer to %s, want %s", got, bob)
	}
	if got := new(big.Int).SetBytes(data); got.Cmp(amount) != 0 {
		t.Errorf("Transfer value %s, want %s", got, amount)
	}

	// 3. And the credit is readable back through the façade, the way
	// eth_call reads it.
	if got := erc20BalanceOf(t, c, env.ExecutionPayload.BlockHash, token, bob); got.Cmp(amount) != 0 {
		t.Errorf("balanceOf(bob) = %s, want %s", got, amount)
	}

	// 4. eth_getCode's markers come off the real drives: the config
	// built-in and the demo app contract from the ABI drive the guest wrote
	// at boot, the token's façade from the registry the deposit just grew.
	at := env.ExecutionPayload.BlockHash
	codeCases := []struct {
		name string
		addr common.Address
		want []byte
	}{
		{"config built-in", guestConfigAddress, CodeMarker(CodeKindSystem)},
		{"counter app", common.HexToAddress("0xc0de000000000000000000000000000000000001"), CodeMarker(CodeKindApp)},
		{"token façade", l2TokenAddress(token), CodeMarker(CodeKindToken)},
	}
	for _, tc := range codeCases {
		code, err := c.CodeAt(context.Background(), at, tc.addr)
		if err != nil {
			t.Fatalf("CodeAt(%s): %v", tc.name, err)
		}
		if !bytes.Equal(code, tc.want) {
			t.Errorf("CodeAt(%s) = %x, want %x", tc.name, code, tc.want)
		}
	}
}
