# EVM compatibility at the ABI boundary: contract addresses, routed to native guest code

**Status: adopted on the devnet.** This document is the research and the
design it leads to; a normative companion spec (the routing standard's
byte-level contract, in the mold of ACCOUNTS-DRIVE-SPEC.md) remains to be
written. The router, the journal, and the built-in contract family of
§5–§10 are implemented and tested in
[`app/`](../app/README.md) — TypeScript on `@deroll/cmio`,
with viem supplying transaction parsing, recovery and ABI plumbing — and
that guest **is the devnet guest**: `build-snapshot.ts` builds it, the
devnet scripts speak its bridge and façades, and the shim's `eth_call`
speaks §7's envelope. The Lua bank app it replaced is retired. It
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
        handler registered  → handler.advance(ctx)   → ACCEPT | REVERT | FAIL
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

### Outcomes: applications never roll the machine back

Today an input either accepts or rejects, and rejection rolls back the
machine — including the nonce bump and the fee. That has a consequence
worth naming before it is load-bearing: **a transaction that fails is
free**, and can be re-included forever (its nonce was never consumed, its
fee never charged). Harmless while the fee is zero; a block-space DoS the
moment it is not. Ethereum's answer is that a reverted transaction still
consumes its nonce and pays for its gas.

The router adopts it, and pushes it one step further than Ethereum needs
to: **an input that passed enforcement always advances the machine.** What
the application did decides only how much of the input is kept, never
whether the machine rolls back. Four outcomes, and an application may
choose any of the first three:

| Outcome | Ledger + value transfer | Outputs | Nonce + fee | Machine |
|---|---|---|---|---|
| **ACCEPT** | kept | emitted | charged | advances |
| **REVERT**(data) | rolled back | dropped | charged | advances |
| **FAIL**(data) | kept | emitted | charged | advances |
| **REJECT** | — | — | — | **rolls back** |

REVERT and FAIL both report their data and both charge the sender; they
differ only in whether the ledger is undone underneath the handler. The
report tag tells them apart on the wire (`0x02` revert, `0x03` fail),
because they mean opposite things to whoever reads the receipt: a revert
is safe to treat as "nothing happened", a fail is not.

**REJECT is the router's alone, and it means one thing: the charge cannot
be recorded.** That is either enforcement failure (bad signature, wrong
nonce, insufficient balance — nothing ran), or a deposit (no nonce or fee
to keep; the deposit-stuck caveat is unchanged), or the nonce bump and fee
debit themselves being refused by the drive. Nothing a handler does
reaches it: a handler that throws reverts, and a handler that *crashes*
reverts, so a broken application costs its sender a nonce and a fee
instead of handing them a free retry. It still never halts the machine —
a halted machine is a halted chain, the doctrine the Lua guest
established and this design keeps.

*Deviation, flagged deliberately:* ACCOUNTS-DRIVE-SPEC §6.3, and the
width rules of §7–§8, say a guest MUST **reject the input** when a table
is at its load limit or a balance would overflow its declared width. The router reverts instead whenever
there is a sender to charge. The clause's mechanism is honored — the
insert does not happen, and the rollback leaves the drive bit-identical to
before the input; only the remedy differs, and what is committed is the
charge alone, an in-place update to a record that already exists. Where no
such record exists the charge genuinely cannot be written, and that is the
REJECT above. The drive spec needs amending to match.

### What rolls back, and who does it

REVERT requires the router to undo a handler's ledger effects without a
machine rollback. That is a small write-journal over the ledger API:
handlers mutate the drive only through the router (§10), the router
records each op's before-image within the input, and REVERT replays them
backwards.

The journal's remit is the **ledger** — the accounts drive and the in-RAM
state that shadows it. It does not reach an application's own RAM, and
does not need to, because the machine already covers the other two cases:

| | What undoes it |
|---|---|
| Ledger, on REVERT | the journal |
| Application RAM, on REJECT | the machine — builder and verifier alike run each input on a fork and discard it unless it accepted (`chain/chain.go`) |
| Application RAM, during inspect | the machine — queries run on a fork that is thrown away (`chain/inspect.go`) |
| Application RAM, on REVERT | **nothing** |

So there is exactly one case where an application's own state outlives a
failure, and it is the one the application controls: it chose to throw.
Hence the rule, which replaces the older "a handler that has mutated
private state may not REVERT":

> A handler that has written nothing of its own returns **REVERT**. One
> that has already written returns **FAIL**.

Ordering a handler so that every fallible step precedes its first private
write keeps it in REVERT territory, which is the safer half — and unlike
Ethereum this costs nothing to arrange, because there are no
cross-contract calls: one input is one handler is one function body, with
no untrusted callee that can revert underneath it. The built-in handlers
(§9) keep no private state that outlives an input, so they only ever
ACCEPT or REVERT.

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

**The system namespace.** `0xC751` (leet CTSI) ‖ 16 zero bytes ‖ `uint16`
index — 65,536 slots, mirroring OP's `0x4200…xxxx` shape:

| Address | Contract | Role |
|---|---|---|
| `0xC75100…0000` | Router registry | `handlerAt(address)`, `handlers()`, `l2TokenOf(address)` — discovery, and the source for `eth_getCode` |
| `0xC75100…0001` | Bridge | `withdrawEther(address to)` payable; `withdrawERC20(address token, address to, uint256 amount)` |
| `0xC75100…0002` | Config | owner-gated: `setFee(uint256)`, `registerPortal(uint8,address)`, `registerToken(address,uint8,string,string,uint8)`, `setTokenMetadata(…)` |

The namespace rule, stated once and normatively: **the reservation is the
full pattern, and the zero run is what carries it.** No one can sign for a
specific system address (a 2^160 preimage), and grinding any address into
the `prefix ‖ zero-run` shape is ~2^144 — that is the forgery barrier, the
same way `0x42` plus sixteen zero bytes is OP's. The two brand bytes are
legibility only: prefix-grinding alone is 2^16, so nothing — no UI badge,
no spec rule, no router logic — may treat a bare `0xC751` prefix match as
meaning anything. Authority is exact membership in the manifest. (A short
brand prefix also pays its way where it is actually seen: wallets truncate
addresses to roughly `0xC75100…0001`, so the brand survives truncation,
which the ASCII-`"Cartesi"` alternative did not.)

**Token façades, at derived addresses.** Each registered token is served
at `address = last20(keccak256("ctsi.erc20.v1" ‖ l1Token))` —
computable from the L1 token address alone, before the first deposit.
The router keeps the reverse map (derived address → registry id) in
memory, rebuilt from the registry at boot and extended on registration.
Calls to the derived address of a token never registered find no handler
and return empty — the same answer an EOA gives.

The obvious alternative — serve the façade at the *same* address the
token has on L1 — deserves an honest burial, because it would work
today: this chain has no deployment mechanism, so no one can claim any
address; a collision with the manifest is a 2^160 preimage; and the
router would not even need the reverse map, since the registry already
keys tokens by their L1 address. The case against it is forward-looking,
and it carries weight because the façade address is consensus-sticky —
it is the token's wallet-visible identity, baked into the state
transition, so changing it later means migrating every holder.

- **The L1 address is the deployer's claim.** On EVM chains a contract
  address belongs to whoever can replay its deployment — CREATE and
  CREATE2 make it a cross-chain claim, which is how Permit2, Safe and
  Multicall3 sit at one address everywhere. The most predictable event
  in a bridged token's life is the bridged→native migration (every
  ecosystem's USDC.e story): the issuer eventually arrives wanting to
  issue natively, at the address their replay rights name. A façade
  squatting it forecloses the one clean outcome, and relocating a
  façade breaks every wallet holding it.
- **This chain's own roadmap revives claimability.** "No deployment" is
  a v1 statement, not an architectural one: §13 keeps DESIGN §8's
  Level 1 (code as a transaction) and Level 2 (an EVM interpreter as
  one handler, owning a sub-space of addresses) open. The moment either
  lands — CREATE2 replay inside an EVM handler, or manifest-governed
  native issuance — L1 contract addresses become claimable here, and
  every same-address façade is a pre-existing squatter whose holders
  cannot be cheaply moved.
- **The façade is not that contract.** It serves a subset ABI over the
  drive (v1 defers even `approve`), and for a fee-on-transfer or
  rebasing token its semantics diverge outright. A distinct address is
  the honest signal that this is a bridged representation with the
  bridge's semantics — the same design language as OP's sender
  aliasing: the same twenty bytes on two chains are different
  principals unless a key or a replay right spans both, and the
  façade's controller is this chain, not the L1 deployer.
- **An invariant by construction, not by probability.** First-seen
  registration lets user input mint routes: a deposit makes a new
  address live in the routing table. Same-address mapping would place
  those routes at depositor-chosen locations — any L1 address they can
  deploy a contract to — while derivation confines them to the
  unforgeable image of a tagged keccak. "A registration can never
  collide with the manifest, an adopted predeploy, or a future system
  address" then holds by construction, and reviewers stop re-running
  the probability argument every time the address map grows.
- **Precedent.** No major bridge mirrors L1 addresses for its
  representations — OP's factory tokens, Arbitrum's gateway tokens,
  Polygon's mappings all mint fresh addresses. The same-address club is
  issuer-side deterministic deployment: exactly the party not to
  collide with.

What derivation costs is discovery — a user cannot paste an L1 address
into a wallet — and the mitigations are cheap enough to be
requirements: the router registry serves `l2TokenOf(address l1Token)`
over `eth_call`, the derivation is one keccak any client computes
offline, and since transfers emit real `Transfer` logs from the façade
address (§8), log-scanning token detection finds it with nothing pasted
at all. One refinement, because registration is owner-gated anyway: an
owner registration MAY name an explicit façade address (an overload of
`registerToken`), with derivation the default and the only option for
first-seen registration. That is the operator's escape hatch — for an
issuer who someday wants a particular address honored — recorded
on-chain and provable like every other owner configuration, without
weakening the namespace invariant for permissionless registrations.

**Genesis-parameter addresses.** The Cartesi-portal receiver resolves at
the envelope's application contract address — where portal deposits are
addressed — and, for deposits, by a **registered portal sender** whatever
the `to`. The second route is what makes the bootstrap work: the portals
and the application contract are deployed after the chain's genesis is
fixed, so the chain cannot name them up front; the owner's registration
input is what makes a portal real, and the sender is the deposit's
authentication anyway (§9). The receiver's calldata is `InputEncoding`'s
packed format, not ABI — which is fine, because *the router routes and the
handler owns its calldata*. ABI is the convention of the built-in family,
not a router-enforced rule; a handler speaking a packed format, or
protobuf, or anything else, is a first-class citizen.

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
   into "input reverted, deterministically" (§5). The cost is IPC and
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

### 10a. The application API, as built: ABI-driven registration

The prototype realizes the manifest as an API rather than a file. The
runtime is a workspace library (`@op-cartesi/app`); an application
boots it and declares each contract as **an address, an ABI, and
callbacks**:

```ts
const app = await Application.boot({ chainId, owner });
await app.contract({ address, abi, transactions: {...}, views: {...} });
await app.run();
```

The library owns everything the standard specifies — admission, dispatch,
outcomes, encodings — and the application owns only its semantics:

- **Dispatch is ABI-driven.** Calldata decodes against the registered ABI;
  the function's `stateMutability` decides which side may run it
  (`transactions` for nonpayable/payable, `views` for view/pure). View
  results are plain values; the library ABI-encodes them.
- **Exceptions are reverts** — the EVM's own rule, with no escape upward.
  A thrown `Revert` carries chosen revert data; a thrown `Fail` carries
  the same data but keeps the ledger and the outputs (§5); any other
  exception reverts with its message as `Error(string)`. Nothing an
  application throws can reject the input, drive refusals included.
- **Application state is just RAM the application owns.** A counter is
  `let count = 0n`. It is machine state — covered by the state root, as
  deterministic as anything else in the machine — but it is not
  outside-readable, and it does **not** roll back on REVERT; what must be
  readable, or must be undoable, belongs in a drive or in the ledger.
  There is no state API, because the honest one would be a byte store the
  application had to hand-serialize into and could silently forget to
  use — a guarantee that only held for the state you remembered to opt
  in. Owning the rule outright is simpler than a tool that half-enforces
  it.
- **The reserved namespaces are enforced at registration**: the 0xC751
  pattern and the adopted predeploys refuse application addresses.

And the manifest becomes machine-readable state: every registered address
and its ABI — built-ins as kind 0, applications as kind 1 — is recorded in
the **ABI drive** (ABI-DRIVE-SPEC.md) at boot, before the first yield, so
the record is genesis state under the genesis root. This closes the
discovery loop the drives were always pointing at: the **accounts drive**
names the tokens a machine serves, the **ABI drive** names the contracts
and their interfaces — so the chain, or anyone holding a snapshot, knows
what the machine speaks by reading drive bytes, with zero knowledge of the
application's implementation. It is the natural bridge for outside
communication: the shim serves contract discovery from the same
read-memory path `AccountAt` already uses. *(done — `eth_getCode`
(`chain/code.go`) answers `0xEF 0xC7 0x51 <kind>` for every routed
address: kind 0 system and 1 application from the ABI drive, 2 for token
façades derived from the accounts drive; the `0xEF` prefix is
EIP-3541-reserved, so no real EVM bytecode can collide with a marker. And
`cartesi_getContracts` serves the full surface — recorded contracts with
their ABIs embedded, façades with their L1 token — as of any retained
block.)*

## 11. What changes where

| Layer | Change |
|---|---|
| **Consensus / wire** | **Nothing.** EvmAdvance unchanged, block format unchanged, outputs tree and voucher encodings unchanged, op-node/op-batcher/op-proposer untouched, L1 contracts untouched. |
| **Guest** | The router (native, reference implementation of the standard) replaces `bank-app.sh`: CMIO loop, outputs accumulator, typed-tx sighash, enforcement, journal, manifest dispatch, built-in family. The accounts drive and its libraries: unchanged. *(done, as workspace libraries — `@op-cartesi/app` the runtime, `@op-cartesi/evm` the wire vocabulary, `@op-cartesi/abis` the ABI drive; `demo` is an application of them, §10a)* |
| **Shim** | `eth_call` builds `EvmCall` (CallArgs grows `From`/`Value`) and maps rejection to revert-with-data *(done — `engineapi/eth.go`)*; `eth_getCode` serves the routed-address markers and `cartesi_getContracts` the full surface with ABIs, both from the drives *(done — `chain/code.go`, §10a)*; receipts try the `EvmLog` decode; mempool already passes typed txs; `eth_estimateGas` unchanged until `EvmSimulate` is wired. |
| **Devnet** | `build-snapshot.sh` ships the router; the dialect scripts collapse into standard tooling — `cast send $TOKEN "transfer(address,uint256)" …`, `cast call $TOKEN "balanceOf(address)" …`, `cast send $BRIDGE "withdrawEther(address)" --value …` — and `send-l2-tx.sh` drops `--legacy`. Scripts getting shorter is the acceptance test. |
| **Tests** | The realmachine suite keeps its role with the new guest *(done — its inputs now speak the standard)*; enforcement (sighash, recovery, nonce) is covered by the router's vitest suite, retiring `test-guest.lua`; golden accounts-drive vectors already cover the ledger. |

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
- **Failed transactions stop being free** (§5): REVERT and FAIL both
  consume nonce and fee, closing the re-inclusion loop that flat-fee
  enforcement would otherwise leave open. Because an application cannot
  reject — not by throwing, not by crashing, not by exhausting the drive —
  there is no application-reachable path back to a free retry. What
  remains free is an input the sequencer never charges for at all
  (README, "Inputs are free"), which is the fee-market question, not
  this one.
- **Address collisions**: a specific system address is unreachable by
  keyed accounts short of a 2^160 preimage, the `prefix ‖ zero-run`
  namespace pattern costs ~2^144 to grind into, and façade derivation
  confines registration-minted routes to a namespace that cannot collide
  with the manifest at all (§6). The brand prefix alone is 2^16 and
  carries no authority — the rule §6 states normatively. The registry
  view makes the routed set auditable.
- **Handler blast radius**: out-of-process by default; crash → REVERT,
  so the sender still pays and the input still advances (§5); in-process
  reserved for the platform's own code and explicit operator grants
  (§10). `MaxCyclesPerInput` bounds every input regardless of what its
  handler does.
- **The journal is consensus code.** REVERT's partial rollback is part of
  the state transition function and must be bit-identical across
  implementations; it goes in the normative spec with golden vectors,
  like the drive.

## 15. Roadmap

1. **Freeze the standard.** The normative companion: envelope field
   authority (§4), transaction admission and sighashes (§5), the
   outcome model and journal semantics (§5), `EvmCall`/`EvmSimulate`
   /`EvmLog` encodings (§7–8), address derivations (§6), the handler ABI
   and manifest (§10) — with golden vectors in the accounts-drive style.
2. **The reference router**, native (Rust or C — the accounts-drive
   libraries and a vendorable secp256k1 exist for both): CMIO loop,
   outputs accumulator, enforcement, journal, and the built-in family
   (ledger, façade, bridge, config, L1Block, portal receiver). Port the
   enforcement vectors of the Lua guest's `test-guest.lua` (since retired
   in favor of the vitest suite).
3. **The shim half**: `EvmCall` in `eth_call` + revert mapping, the ABI
   drive's Go reader with `eth_getCode` markers, and `cartesi_getContracts`
   are done; remaining: `EvmLog` receipts (§10a).
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
