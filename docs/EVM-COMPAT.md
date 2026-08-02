# EVM compatibility at the ABI boundary: contract addresses, routed to native guest code

**Status: proposed design.** This document is the research and the design it
leads to; a normative companion spec (the routing standard's byte-level
contract, in the mold of ACCOUNTS-DRIVE-SPEC.md) follows on adoption. It
addresses the gap between what the chain's transport already is — ordinary
signed Ethereum transactions, addressed to twenty-byte addresses — and what
the guest does with it: a private dialect no EVM tool can speak.

The one-line summary: give the guest a **routing standard** — every
transaction and every call is dispatched on its `to` address, through a table
fixed at snapshot build, to **native guest code**. ERC-20 "contracts" are
façades over the accounts drive; native token transfers are ledger
operations; "smart contracts" are handlers — statically linked into the
router, dynamically loaded, or external processes — free to do anything Linux
on RISC-V allows. The EVM's *interface* is adopted wholesale: transaction
types, ABI calldata, events, revert data, `eth_call`. The EVM's *execution
model* — bytecode, gas-per-opcode, storage tries, dynamic deployment — is
deliberately not. Wallets and tooling get a chain they already know how to
talk to; the guest keeps the freedom that is the whole point of a Cartesi
execution layer.

## 1. The problem, precisely

The chain's wire format has been Ethereum-shaped from the start, for a
structural reason: blocks must stay parseable by stock op-node and
op-batcher (README, "Layout"). So every input is already a signed Ethereum
transaction with a sender, a `to`, a value, and calldata. What happens on
the guest side of that boundary is another story:

- **The guest speaks three private dialects.** Withdrawals are calldata
  tagged `"w"` and `"t"`; owner configuration is deposit payloads tagged
  `"p"` and `"f"`; balance queries are raw 20- or 40-byte inspect payloads
  whose encoding is the bank app's own invention (ACCOUNTS.md §1 already
  calls this out). None of it is expressible from a wallet, a block
  explorer, ethers.js, or `cast` without a custom script per operation —
  which is exactly what `devnet/*.sh` is.
- **The `to` address means nothing.** `send-l2-tx.sh` sends every payload
  to the zero address, because the guest never reads `to`. A transaction's
  one universal routing field — the thing every wallet, indexer and SDK is
  built around — is dead weight.
- **Wallets cannot send transactions the guest accepts.** The guest
  implements only the legacy sighash and rejects every EIP-2718 typed
  envelope. Headers carry a base fee (the chain is London-active from
  genesis), so MetaMask and modern `cast` default to EIP-1559 type-2
  transactions — which the guest rejects. The devnet works because
  `send-l2-tx.sh` forces `--legacy`; a user with a wallet does not get
  that flag.
- **Tokens are invisible.** A deposited ERC-20 exists as a registry entry
  and sparse-table records, but there is no address to add to a wallet, no
  `balanceOf` to call, no `Transfer` event for an indexer. `eth_call`
  returns a bare 32-byte hex string in an encoding only `balance.sh` knows.
- **Receipts carry Cartesi outputs, not events.** Logs are synthesized as
  `CartesiOutput(uint64,bytes)` from a single system address
  (`chain/receipts.go`) — the right call for provability, but nothing
  downstream of an ABI decoder can interpret them.

Meanwhile the reuse argument that shaped the guest has inverted. The
`EvmAdvance` envelope was adopted so a **stock machine-guest-tools rootfs
and existing Cartesi applications run unmodified** (DESIGN §3). That was the
right first move — it got the chain sequencing without writing a guest — but
this chain stopped running a stock guest the moment it enforced nonces: the
guest now recovers senders itself, keeps a ledger the host reads directly,
and implements consensus rules the envelope knows nothing about. What
remains of guest-tools in the loop is its *programming model* — one
application, one opaque payload, `msgSender` as the authority, a `rollup`
CLI popen per emission — and that model is now the obstacle: it has no
place for "this transaction is addressed to the token contract" because it
has no notion of more than one addressee.

The fix is not to make the dialects prettier. It is to adopt the interface
the transport has been carrying all along.

## 2. The chain is already half contract-shaped

Three observations, all visible in traffic the guest receives today:

1. **Every block opens with a transaction addressed to a predeploy.**
   op-node's L1-attributes deposit is addressed to `0x4200…0015` (the OP
   `L1Block` predeploy) with ABI-shaped calldata (`setL1BlockValues…`),
   from the canonical depositor address. The current guest pattern-matches
   it into the record-and-accept arm; a router would deliver it to a
   handler registered at that address, which is precisely what op-geth
   does with it.
2. **Portal deposits are already routed by address — implicitly.**
   A Cartesi portal deposit arrives as a deposit transaction whose `to` is
   the application contract and whose authentication is the (aliased)
   sender. The guest dispatches on `deposit.from`; routing on `to` first
   and sender second is the same information, organized the way every
   other Ethereum system organizes it.
3. **Standard-bridge deposits arrive addressed to `0x4200…0007`** with two
   layers of ABI (`relayMessage(…finalizeBridgeERC20…)`) that DESIGN §7d
   already contemplates the guest decoding by hand. "Decode two ABI layers
   addressed to a known contract address" *is* contract routing, done once,
   inline, without a name.

DESIGN §8's Level 0 — "design the input envelope as `(app_id, payload)`
from day one, with a supervisor dispatching by app id" — is the same
recommendation from the other direction. This design is Level 0 built, with
one improvement: instead of inventing an app-id scheme, use the one the
transport already carries and the entire ecosystem already indexes on.
**The app id is the address.**

## 3. The model

A **handler** is native guest code registered at a twenty-byte address. The
**router** is the guest's main loop: it owns the CMIO protocol, performs
transaction-level enforcement, and dispatches each input and each inspect
query to the handler registered at its `to` address. The handler table is
fixed when the snapshot is built — it is machine state, covered by the
genesis root like every other consensus parameter — and there is no
dynamic deployment (deliberately; §13).

For a transaction (advance):

```
raw tx ──parse──▶ {sender, to, value, calldata, nonce, …}
        (deposit | legacy | 2930 | 1559)
   │
   ├─ enforcement (router, uniform): signature, chain id, nonce, fee cover
   │
   ├─ value transfer: debit sender, credit `to`   (native ether, the drive)
   │
   └─ dispatch on `to`:
        handler registered  → handler.advance(ctx)   → ACCEPT | REVERT | REJECT
        no handler          → plain transfer; calldata ignored (Ethereum's rule)
```

For a call (`eth_call` → inspect): the same dispatch, into the handler's
view entry, whose return bytes become `eth_call`'s return value and whose
revert becomes a proper `execution reverted` with data (§7).

Ether stays native: balances live in the accounts-drive account record,
`msg.value` moves them, `eth_getBalance` reads them — nothing changes below
this design. ERC-20 balances stay in the sparse table; what is new is that
each registered token gets an *address*, and the router maps calls on that
address onto the drive (§9). The accounts drive spec is untouched — this
design layers strictly above it, and every host fast path and exit proof of
ACCOUNTS.md survives byte-for-byte.

What the EVM's interface means here, precisely: the **transaction formats**
(legacy, 2930, 1559 — §5), the **addressing model** (`to`-routing,
`msg.sender`, `msg.value`), the **ABI convention** for the built-in
handlers' calldata and return data, **events** as receipts logs (§8), and
**revert data**. What is deliberately not adopted: bytecode and its gas
schedule, `eth_getStorageAt`/MPT proofs (impossible here for ACCOUNTS.md
§9's reasons), CREATE/CREATE2, and contract-to-contract calls as a general
mechanism (§10 has the v1 rule). A handler is not sandboxed bytecode; it is
native code the chain operator shipped in the snapshot, trusted the way a
predeploy is trusted.

## 4. The fate of EvmAdvance and machine-guest-tools

The user-visible complaint — "the EvmAdvance wrapping is not making sense"
— bundles three separable things, and they deserve different fates:

1. **The wire encoding**: `EvmAdvance(chainId, appContract, msgSender,
   blockNumber, blockTimestamp, prevRandao, index, payload)` around each
   raw transaction, built by `chain/input.go`, reconstructed by verifiers.
2. **The guest implementation**: libcmt and the `rollup` CLI — the rootfs
   tooling that decodes the envelope, owns the yield protocol, and
   maintains the outputs accumulator.
3. **The programming model**: one application, `msgSender` as the
   authority, opaque payload, dispatch by payload prefix.

**Keep (1), for now.** The envelope is consensus wire format, implemented
and tested on both the build and verify paths, and every field is derivable
from the block header — its marginal cost is a few hundred bytes of framing
per input that never leave the machine boundary. Replacing it buys no
capability: the router must decode *something* to learn the block context
(number, timestamp, prevRandao — the machine has no other way to know), and
an ABI-encoded envelope is as good a carrier as any. Two further reasons to
not churn it: Dave's machine model (`MachineInstance::new_rollups_advanced_until`,
DESIGN §7e) consumes rollups-style advance inputs, so staying
encoding-compatible keeps Dave's commitment builder and node machinery
reusable; and the outputs side of the same guest-tools contract — vouchers
as `Voucher(address,uint256,bytes)`, notices as `Notice(bytes)` — is pinned
by `LibOutputValidityProof` and the L1 executor, so the emission encodings
*cannot* move regardless.

What changes is the envelope's **authority**. The routing standard
normatively demotes it to transport framing:

- `payload` is the entire raw transaction (already true).
- `msgSender` is advisory and MUST NOT be used for authentication. The
  authority is the recovered signature for user transactions and the
  deposit's own `from` for deposits — the rule the guest already lives by;
  the standard just writes it down.
- `chainId` is advisory; the router pins the chain id as a genesis
  parameter (like `OWNER`) and rejects transactions signed for any other
  chain. Today that pinning lives only in the mempool's signer — a
  courtesy filter — so a sequencer could include a foreign chain's
  transaction and the guest would execute it. Routing closes that.
- `appContract` matters only as the voucher execution context on L1.
- `blockNumber`, `blockTimestamp`, `prevRandao`, `index` are the block
  context, taken at face value (they are header-derived and
  verifier-reconstructed; lying about them is lying about the block).

A slimmer envelope — `(index, blockNumber, timestamp, prevRandao, payload)`
— remains a cheap consensus change while the chain is a devnet, and is
worth folding into whatever hard fork first touches the wire for other
reasons. It is not worth its own fork: the win is aesthetic.

**Drop (2) and (3).** The routed guest owns its CMIO loop directly — no
`rollup` popen per emission, no libcmt dispatch. That transfers two duties
the standard must name explicitly, because the chain already depends on
them:

- **The outputs accumulator.** libcmt maintains the Cartesi outputs Merkle
  root incrementally and reports it in the TX buffer at each accepted
  yield; `machine/remote_test.go` pins that behavior, the realmachine
  tests check host and guest agree, and DESIGN §7e requirement 2 makes the
  in-machine root settlement-critical (`DaveConsensus._validateOutputTree`
  proves exactly that leaf). The router MUST maintain the same accumulator
  and write the same buffer.
- **The yield protocol**: outputs and reports as automatic yields, the
  accept/reject manual yields whose rollback semantics the whole
  enforcement design leans on, and inspect requests. Same protocol, new
  implementer.

There is a large fringe benefit hiding in this replacement. The current
guest pays for pure-Lua secp256k1 recovery through an emulated CPU — the
module's own header calls it the dominant cycle cost per transaction. The
router is the natural moment to go native: a C or Rust router linking a
real secp256k1 implementation cuts per-transaction cycles by orders of
magnitude, and cycles are this chain's gas *and* its future proving cost
(DESIGN §5, Plan B2). The accounts-drive C and Rust libraries already exist
and pass the same golden vectors as the Lua one, so the ledger side moves
with it.

## 5. The router pipeline

### Parsing and enforcement

The router accepts four transaction shapes and rejects the rest:

| First byte | Type | Fate |
|---|---|---|
| `0x7e` | OP deposit | routed; enforcement exempt (L1-origin authentication) |
| `0xc0`–`0xff` | legacy RLP list | routed; legacy or EIP-155 sighash |
| `0x01` | EIP-2930 | routed; typed sighash |
| `0x02` | EIP-1559 | routed; typed sighash |
| `0x03`, `0x04` | blob, set-code | reject (no blob market, no EVM code to set) |
| anything else | not a transaction | record-and-accept, as today (a malformed input must never halt the machine, and rejecting it would roll back a message a later guest might understand) |

Typed-transaction support is not optional polish; it is the point. Wallets
send type-2 on any chain whose headers carry a base fee, which this chain's
do from genesis. The mempool already admits them (`Pool.Add` uses
`UnmarshalBinary` and go-ethereum's signer — only the guest says no), so
the entire change is guest-side sighash coverage: for type-2,
`keccak(0x02 ‖ rlp([chainId, nonce, maxPriorityFee, maxFee, gas, to,
value, data, accessList]))` with `v` as y-parity. Access lists are parsed
and ignored (there is no state to warm). `send-l2-tx.sh` loses `--legacy`.

Enforcement is exactly ACCOUNTS.md v1, relocated from the app into the
router and applied uniformly before any dispatch: recover the sender,
require the transaction's chain id to equal the genesis chain id, require
the nonce to equal the account record's, require the balance to cover the
flat fee (and now also the transaction's `value`). Gas fields are parsed
for form and otherwise unread until there is a fee market to read them
with — the flat fee remains what is charged, and `eth_gasPrice` remains
honest about that.

### Value, then dispatch

An accepted transaction's `value` moves before the handler runs: debit
sender, credit `to` — whether `to` is an EOA or a handler. A handler whose
manifest entry is not marked `payable` refuses value (revert), which is
Ethereum's rule for contracts without a payable path. `to = nil`
(contract creation) rejects: there is no deployment.

Deposits follow OP's own semantics, slightly more faithfully than today:
`mint` is credited to the deposit's `from` unconditionally, then the
transaction executes as a call from `from` to `to` carrying `value`. For a
simple ether deposit the observable result is what the current guest
produces (recipient credited), but the rule is uniform, and it preserves
OP's guarantee that a deposit's mint survives even when its call fails
(§ below). The lockbox caveat of DESIGN §7c/§7d is untouched: crediting
OP-path ether does not make it voucher-reachable.

### Three outcomes, not two

Today an input either accepts or rejects, and rejection rolls back the
machine — including the nonce bump and the fee. That has a consequence
worth naming before it is load-bearing: **a transaction that fails is
free**, and can be re-included forever (its nonce was never consumed, its
fee never charged). Harmless while the fee is zero; a block-space DoS the
moment it is not. Ethereum's answer is that a reverted transaction still
consumes its nonce and pays for its gas. The router adopts it with a
three-outcome model:

- **ACCEPT** — handler succeeded. Commit everything: handler's ledger
  writes, value transfer, nonce bump, fee debit, outputs.
- **REVERT(data)** — application-level failure, Ethereum-shaped. The
  router rolls back the handler's ledger writes and the value transfer,
  but **keeps the nonce bump and the fee debit**, finishes the input as
  *accepted* (the machine must not roll back — the nonce write is state),
  emits no outputs from the handler, and reports the revert data. The
  receipt gets `status: 0` and the shim surfaces the revert data on
  `eth_call` and simulation paths.
- **REJECT** — consensus-mandated refusal: the accounts drive is at its
  load limit, a balance would overflow its declared width, the registry is
  full (the exact conditions ACCOUNTS-DRIVE-SPEC §6.3/§8 requires answered
  by rejecting the input), or the transaction failed enforcement in the
  first place. The input finishes rejected and the machine rolls back
  wholesale, as today. Deposits keep REJECT-only semantics — they have no
  nonce or fee to charge, and their failure mode (the deposit-stuck
  caveat) is unchanged.

REVERT requires the router to be able to undo a handler's ledger effects
without a machine rollback. That is a small write-journal over the ledger
API: handlers mutate the drive only through the router (§10), the router
records each op's before-image within the input, and REVERT replays them
backwards. The journal covers the drive; it does not cover a handler's
*private* state (its own files), so the handler contract is explicit: a
handler that has mutated private state may not return REVERT — it either
completes (ACCEPT) or escalates (REJECT, which rolls back everything). The
built-in handlers (§9) keep no private state that outlives an input except
counters written at ACCEPT, so they satisfy this trivially.

A handler that crashes, exceeds its manifest bounds, or breaks the
handler protocol (§10) is treated as REJECT — deterministic, and the
machine's rollback contains whatever mess it made. It never halts the
machine: a halted machine is a halted chain, the doctrine `bank-app.sh`
already carries.

## 6. The address map

Addresses come in four families, all fixed at snapshot build:

**Adopted OP predeploys — only where the semantics genuinely hold.**
`L1Block` at `0x4200…0015` is the one clear case: the chain already
delivers the attributes deposit to that address every block, the handler
stores the decoded values, and the standard view methods (`number()`,
`timestamp()`, `hash()`, `basefee()`, `sequenceNumber()`, …) serve them.
Tooling that reads L1 context on OP chains starts working, and — more
useful — *guest applications gain L1 context* they currently have no
access to. `L2ToL1MessagePasser` (`0x4200…0016`) is the deliberate
non-adoption: implementing its ABI would invite OP tooling into a
withdrawal flow that dead-ends at `proveWithdrawalTransaction`'s MPT proof
(DESIGN §7). A predeploy that half-works is worse than one that is absent;
withdrawals get their own contract with no OP costume. The same reasoning
defers the messenger/standard-bridge pair (`0x4200…0007`/`…0010`): their
deposit halves are implementable, their withdrawal halves are not, and
DESIGN §7d already chose Cartesi portals. Revisit only if standard-bridge
deposit traffic actually matters.

**The system namespace.** Reserved prefix `0x43617274657369` (ASCII
`"Cartesi"`, seven bytes) + eleven zero bytes + index:

| Address | Contract | Role |
|---|---|---|
| `0x4361727465736900…00` | Router registry | `handlerAt(address)`, `handlers()` — discovery, and the source for `eth_getCode` |
| `0x4361727465736900…01` | Bridge | `withdrawEther(address to)` payable; `withdrawERC20(address token, address to, uint256 amount)` |
| `0x4361727465736900…02` | Config | owner-gated: `setFee(uint256)`, `registerPortal(uint8,address)`, `registerToken(address,uint8,string,string,uint8)`, `setTokenMetadata(…)` |

No one can sign for these addresses (a private key with a chosen 20-byte
address is a 2^160 preimage search), so squatting is not a concern; the
prefix is legibility, not security.

**Token façades, at derived addresses.** Each registered token is served
at `address = last20(keccak256("ctsi.erc20.v1" ‖ l1Token))` —
deterministic from the L1 token address alone, so a wallet can be
configured before the first deposit and two chains bridging the same token
agree on the address. The router keeps the reverse map (derived address →
registry id) in memory, rebuilt from the registry at boot and extended on
registration. Calls to the derived address of a token never registered
find no handler and return empty — the same answer an EOA gives.

**Genesis-parameter addresses.** The Cartesi-portal receiver is registered
at the application contract address (a config value, like `OWNER`), because
that is where portal deposits are already addressed. Its calldata is
`InputEncoding`'s packed format, not ABI — which is fine, because *the
router routes and the handler owns its calldata*. ABI is the convention of
the built-in family, not a router-enforced rule; a handler speaking a
packed format, or protobuf, or anything else, is a first-class citizen.

**`eth_getCode`.** Tools probe code to distinguish contracts from EOAs.
The shim serves a one-byte marker `0xfe` (the INVALID opcode — honest to
anything that tries to execute it) for every routed address, and empty
bytes otherwise, reading the routed set from the router registry view.
Not consensus; just courtesy.

## 7. Calls: `eth_call` over inspect, and the simulation hook

Inspect payloads get a canonical envelope, symmetric with the advance
side:

```
EvmCall(uint256 chainId, address from, address to, uint256 value, bytes data)
```

ABI-encoded with its 4-byte selector. The shim's `eth_call` builds it from
the request (`CallArgs` grows `From` and `Value`, defaulting to zero); the
router dispatches on `to` into the handler's **view entry**: same context
shape as advance, ledger access read-only, no outputs, returning
`RETURN(data)` or `REVERT(data)`. Block context comes from the parked
machine's last-seen envelope, which for the machine at block N *is* block
N's context. On RETURN the report carries the return data and the inspect
accepts — so `balanceOf(address)` comes back as a clean ABI word and
ethers.js decodes it. On REVERT the inspect rejects and the report carries
the revert payload (`Error(string)`-encoded where the built-ins produce
it); the shim maps that to the standard JSON-RPC revert error with `data`,
so `require`-style messages surface in tools verbatim.

Inspect payloads that do not begin with the `EvmCall` selector fall
through to a manifest-designated fallback handler, which is the migration
path for app-private query dialects (and `cartesi_inspect` remains the raw
passthrough it is today).

The same envelope, under a second selector — `EvmSimulate`, same fields —
runs the *advance* path on the inspect fork with enforcement skipped
(no signature, no nonce, no fee): the discarded-fork simulation entry
point that `engineapi/eth.go` explicitly names as the missing piece for a
real `eth_estimateGas`. The host already measures an inspect's cycles, so
estimation is the shim converting measured cycles at `CyclesPerGas` with
a safety margin — no new guest reporting needed. v1 reserves the
selector; wiring `eth_estimateGas` to it can land with the fee-market
work it belongs to.

## 8. Events: notices that decode as logs

Receipts currently synthesize one log shape: `CartesiOutput(uint64,bytes)`
from a system address. The doctrine behind it holds — only provable
emissions may become logs, and reports must never (DESIGN §7). Events
therefore ride the provable channel: a handler emits an event as a
**notice** whose payload is

```
EvmLog(address emitter, bytes32[] topics, bytes data)
```

ABI-encoded with selector. At the outputs level it is a perfectly ordinary
`Notice(bytes)` — the outputs tree, the on-chain verifier, and every
existing proof are oblivious. At receipt-synthesis time the shim tries the
`EvmLog` decode: on success the receipt gets a real `types.Log` with the
emitter as `address` and the declared topics — so a token transfer
produces an actual `Transfer(address,address,uint256)` log that MetaMask,
indexers and `cast logs` recognize — and on failure it falls back to the
`CartesiOutput` form unchanged. Nothing here is consensus: `receiptsRoot`
and the bloom stay empty, so the encoding can still move (the deliberate
ordering of DESIGN §7 — serve receipts before committing them).

Two consequences worth stating. Every event is a permanent leaf in the
cumulative outputs tree — capacity 2^63, cost a few keccaks per emission,
both fine — and every event is therefore **provable on L1 against any
later proposal**, which is strictly more than an Ethereum log gives its
consumers. Provable payment receipts fall out for free.

`eth_getLogs` over any real range wants an index the store does not have;
that is an indexing task, not a design question, and it is out of scope
here.

## 9. The built-in contract family

What ships in the router, against the drive the chain already has:

**Native ether.** No contract at all: `value` on any transaction,
enforced and journaled by the router; `eth_getBalance` and
`eth_getTransactionCount` keep reading the account record exactly as
ACCOUNTS.md §6.2 built them.

**The ERC-20 façade** (one handler, instantiated per registered token by
the address map):

| Method | Backing |
|---|---|
| `balanceOf(address)` | sparse-table read (or wide column — profile-agnostic through the accounts-drive library) |
| `transfer(address,uint256)` | debit `msg.sender`'s holding, credit recipient's, emit `Transfer` |
| `totalSupply()` | handler-maintained counter (credits minus debits per token) — private state, since only `eth_call` reads it; the drive spec stays closed |
| `decimals()`, `name()`, `symbol()` | registry metadata: supplied by `registerToken`, owner-settable later via `setTokenMetadata`; auto-registered (first-seen) tokens serve loud placeholders (`"UNREGISTERED-<id>"`, 0 decimals) until the owner sets truth — a wrong-decimals default misprices real balances in every wallet, which is worse than an ugly one |
| `approve` / `allowance` / `transferFrom` | **deferred.** Every v1 money path debits `msg.sender` (transfers, bridge withdrawals), which the signature already authorizes. Allowances exist to let third parties pull, and v1 has no third-party pullers. When one arrives, the allowance table is handler-private state — only `eth_call` ever reads an allowance, so it does not belong in the drive spec |

Deposit credits emit `Transfer(0x0 → recipient)` (the mint convention
indexers expect), withdrawal burns emit `Transfer(sender → 0x0)`.

**The bridge.** `withdrawEther(address to)` payable: the router has
already moved `msg.value` to the bridge's own balance; the handler debits
itself (destroying the ether on L2) and emits the voucher paying `to` —
the same voucher shape, tree, and L1 execution path as today (DESIGN
§7c). `withdrawERC20(address token, address to, uint256 amount)`: debits
`msg.sender`'s holding of the token (named by its L2 façade address,
resolved through the registry to the L1 address the voucher must call)
and emits the `transfer(to, amount)` voucher. This quietly fixes a v1
looseness ACCOUNTS.md chose to keep: withdrawals now debit the
authenticated sender, not an arbitrary payee named in calldata.

**Config.** The owner tags (`"p"`, `"f"`) become ABI methods on the config
address, authenticated by sender identity — which now works identically
whether the owner speaks via an L1 deposit (today's only path) or an
ordinary signed L2 transaction (new, and cheaper). `registerToken` gains
the metadata parameters the façade serves.

**L1Block** and the **portal receiver**, as in §6.

## 10. Handlers: linkage, capability, failure

The manifest — a file in the snapshot, e.g.
`/etc/cartesi-router/handlers.json`, covered by the genesis root — maps
each address to `{kind, entry, flags}`. Three kinds, matching the three
linkage models:

1. **Built-in**: compiled into the router; a table of function pointers.
   The system family (§9) lives here. Fastest; a fault here is a router
   fault, which is accepted — the router is the platform.
2. **Shared object**: a `.so` in the rootfs, `dlopen`ed at boot, same C
   ABI as built-ins (an entry point receiving a context/vtable struct).
   For operator-trusted applications that ship separately from the router
   build but want in-process cost.
3. **External**: an executable (spawned per input) or a long-lived service
   (a Unix socket, spawned at boot), speaking a length-prefixed request/
   response protocol that mirrors the C ABI. The default for application
   code: an in-process handler's segfault kills the guest, and a dead
   guest is a halted chain — process isolation turns "handler crashed"
   into "input rejected, deterministically" (§5). The cost is IPC and
   spawn cycles inside an emulated CPU, which is real but bounded, and an
   app can graduate to kind 2 when it has earned the trust.

The handler interface, conceptually (the normative spec pins the bytes):
the advance entry receives `{sender, to, value, calldata, block context,
input index, deposit?, mint}` plus a capability handle for: ledger
operations (journaled, §5), `emitEvent(topics, data)`,
`emitVoucher(destination, value, payload)`, `emitReport(bytes)`. The view
entry receives the same minus value and emissions, plus
`return(data)`/`revert(data)`.

**Capability rule.** In-process code can physically touch anything, but
the API draws the line the manifest enforces for external handlers and
audits for the rest: a handler moves **its own** balances freely (the
contract-holds-funds model — exactly how the bridge burns), receives
value and tokens like any address, and does not touch third-party
accounts. The built-ins that must move `msg.sender`'s assets (token
`transfer`, bridge withdrawals) are the trusted core — that is what
"built-in" means. A manifest flag can grant an application handler the
privileged ledger, as an explicit, genesis-visible operator choice.
Permissionless pulling is what `approve` is for, when it is needed.

**Determinism** is inherited, not legislated: everything inside a Cartesi
machine is bit-deterministic, clocks included, and the machine has no
entropy the guest could smuggle in. The one real rule is the drive-spec
one the router already owns: all drive bytes current at every yield
(ACCOUNTS-DRIVE-SPEC §10), which the router performs once, centrally,
after the journal commits — handlers never sync, and out-of-process
handlers never see the device at all.

## 11. What changes where

| Layer | Change |
|---|---|
| **Consensus / wire** | **Nothing.** EvmAdvance unchanged, block format unchanged, outputs tree and voucher encodings unchanged, op-node/op-batcher/op-proposer untouched, L1 contracts untouched. |
| **Guest** | The router (native, reference implementation of the standard) replaces `bank-app.sh`: CMIO loop, outputs accumulator, typed-tx sighash, enforcement, journal, manifest dispatch, built-in family. The accounts drive and its libraries: unchanged. |
| **Shim** | `eth_call` builds `EvmCall` (CallArgs grows `From`/`Value`) and maps rejection to revert-with-data; receipts try the `EvmLog` decode; `eth_getCode` serves markers from the registry view; mempool already passes typed txs; `eth_estimateGas` unchanged until `EvmSimulate` is wired. |
| **Devnet** | `build-snapshot.sh` ships the router; the dialect scripts collapse into standard tooling — `cast send $TOKEN "transfer(address,uint256)" …`, `cast call $TOKEN "balanceOf(address)" …`, `cast send $BRIDGE "withdrawEther(address)" --value …` — and `send-l2-tx.sh` drops `--legacy`. Scripts getting shorter is the acceptance test. |
| **Tests** | The realmachine suite keeps its role with the new guest; `test-guest.lua`'s enforcement vectors (sighash, ecrecover, nonce) port to the router's language; golden accounts-drive vectors already cover the ledger. |

## 12. Alternatives considered

**A real EVM in the guest** (revm/evmone compiled for riscv64): maximal
compatibility — Solidity deploys and all — but it abandons the premise.
Native code is the point of this chain, and an EVM drags in the gas
schedule, the state trie, and the deployment model, i.e. everything
ACCOUNTS.md §4 and DESIGN §8 kept out. The honest place for an EVM is
DESIGN §8 Level 2: *one handler* owning a sub-space of addresses,
additive under this same routing standard, if a product case ever wants
it. Routing makes that possible later without making it foundational now.

**ABI on the single app** (recode the bank tags as ABI methods on one
address): gets `cast send` ergonomics, loses everything address-shaped —
no per-token contracts for wallets, no `L1Block`, no event emitters, and
the single-app model is the actual complaint.

**Fake contracts in the shim** (answer `balanceOf` host-side from the
drive, synthesize events host-side): tempting because the host can read
the drive — but it moves semantics outside the proven state transition,
the exact move DESIGN §7e forbids for anything a referee might one day
adjudicate. The guest is the execution layer; the shim serves what the
guest did.

**A new envelope now**: §4. The model dies; the bytes stay.

## 13. Deliberately out of scope

- **Dynamic deployment.** All handlers ship in the snapshot; adding one is
  a machine-template upgrade, governed like any prestate change (exactly
  ACCOUNTS.md §8's answer for drive geometry). DESIGN §8 Level 1 — code as
  a transaction — remains the designed-for future: the manifest becomes
  writable state, and nothing else in this standard moves.
- **Cross-handler calls.** v1 handlers compose through the ledger API and
  through users' transactions, not through each other. Synchronous native
  calls are cheap and DESIGN §8 names them as the composability prize, but
  reentrancy and capability rules deserve their own document, not a
  paragraph here.
- **Fee market.** This design supplies the missing simulation entry and
  keeps the flat fee; pricing is its own work (ACCOUNTS.md §9 said the
  same).
- **Allowances, `eth_getLogs` indexing, receipts-root commitment** — named
  above, deferred above.

## 14. Security notes

- **Enforcement is unchanged in substance** — signature, chain-id pin,
  nonce, fee — relocated into native code and applied before any handler
  runs. The envelope's `msgSender` stays untrusted (§4).
- **Failed transactions stop being free** (§5): REVERT consumes nonce and
  fee, closing the re-inclusion loop that flat-fee enforcement would
  otherwise leave open.
- **Address collisions**: derived façade addresses and the system
  namespace are unreachable by keyed accounts short of a 2^160 preimage;
  the registry view makes the routed set auditable.
- **Handler blast radius**: out-of-process by default; crash → REJECT;
  in-process reserved for the platform's own code and explicit operator
  grants (§10). `MaxCyclesPerInput` bounds every input regardless of what
  its handler does.
- **The journal is consensus code.** REVERT's partial rollback is part of
  the state transition function and must be bit-identical across
  implementations; it goes in the normative spec with golden vectors,
  like the drive.

## 15. Roadmap

1. **Freeze the standard.** The normative companion: envelope field
   authority (§4), transaction admission and sighashes (§5), the
   three-outcome model and journal semantics (§5), `EvmCall`/`EvmSimulate`
   /`EvmLog` encodings (§7–8), address derivations (§6), the handler ABI
   and manifest (§10) — with golden vectors in the accounts-drive style.
2. **The reference router**, native (Rust or C — the accounts-drive
   libraries and a vendorable secp256k1 exist for both): CMIO loop,
   outputs accumulator, enforcement, journal, and the built-in family
   (ledger, façade, bridge, config, L1Block, portal receiver). Port the
   `test-guest.lua` enforcement vectors.
3. **The shim half**: `EvmCall` in `eth_call` + revert mapping, `EvmLog`
   receipts, `eth_getCode`, `CallArgs.From/Value`.
4. **Devnet swap**: router into `build-snapshot.sh`, dialect scripts
   replaced by `cast` one-liners, realmachine suite green.
5. **Later, in this order of pull**: `EvmSimulate` under
   `eth_estimateGas` (with fee work), token metadata polish, allowances
   when a puller exists, envelope slimming at the next wire-touching
   fork, dynamic manifest (Level 1) when a second team wants in.

## References

- This repo: DESIGN.md §3 (envelope decision), §7 (outputs/receipts
  doctrine), §7c–d (bridge custody), §7e (what settlement requires of the
  guest), §8 (multi-app levels); ACCOUNTS.md (the drive, enforcement,
  exhaustion economics); ACCOUNTS-DRIVE-SPEC.md (the ledger this design
  routes onto).
- OP predeploys (`L1Block`, the attributes deposit, address aliasing):
  https://specs.optimism.io/protocol/predeploys.html ·
  https://specs.optimism.io/protocol/deposits.html
- Transaction types: EIP-2718, EIP-155, EIP-2930, EIP-1559.
- Cartesi guest tools (the duties the router assumes: yield protocol,
  outputs accumulator, TX-buffer root): `cartesi/machine-guest-tools`;
  `cartesi/dave` `DaveConsensus._validateOutputTree` (why the in-machine
  root matters).
