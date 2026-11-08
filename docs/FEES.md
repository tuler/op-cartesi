# Fees: pricing execution, storage and data on a Cartesi-machine L2

**Status: proposed, feasibility unproven.** This document consolidates a
design discussion into one place: what the chain charges for today
(nothing, in effect), what Ethereum and the OP Stack charge for and why,
which of their mechanisms transfer to a chain whose execution layer is a
Cartesi Machine, and which do not. It takes positions, but several of the
load-bearing ones rest on properties of the guest that have not been
measured — §11 lists them, each with the experiment that would settle it.
Nothing here should be built before §11 has come back with numbers. It
fills the README's first known gap, "Inputs are free", which
[RAAS.md §9](RAAS.md) calls the largest blocker that has nothing to do
with proofs.

The one-line summary: borrow the OP Stack's fee machinery wholesale where
the resource is the same (EIP-1559, the L1 data fee, the operator fee, the
fee vaults, deposit pricing on L1); measure execution in **machine cycles**
read from the guest's own deterministic clock, so the charge lands inside
the state root; price **storage as deposits against fixed capacity** rather
than as rent against unbounded growth; and leave the economics of
application RAM to the application, because in a single-process guest
nothing else can bound it — the platform supplies the gauge, the budget and
priced tables, not the policy.

## 1. The problem, precisely

Ethereum prices three resources. Execution is metered per opcode and paid
per unit of gas at a block-level price. State is priced at write time
(`SSTORE`, account creation) and never again, which is why Ethereum's state
only grows. And an OP Stack L2 prices a third resource Ethereum itself does
not have: the L1 data its batcher posts, charged per transaction as the L1
data fee, plus since Isthmus an operator fee for costs that fit neither
model. Deposits are priced on L1, inside `OptimismPortal`'s resource
metering, and execute free on L2.

This chain measures the first resource and charges for none of them:

| Piece | Where it stands |
|---|---|
| Execution measurement | The host counts the mcycles each input consumes (`machine/jsonrpc.go`, `yieldLoop`) and divides by `CyclesPerGas = 1000` (`chain/config.go`) into header and receipt `gasUsed`. Nobody pays for it. |
| The charge | The guest debits a flat per-transaction fee (`app/src/ledger.ts`, `fee`), owner-settable through the Config contract, zero on the devnet. It is charged on accept, revert and fail (EVM-COMPAT §5). The transaction's `gas`, `maxFeePerGas` and `maxPriorityFeePerGas` are parsed for form and otherwise unread. |
| Base fee | A constant 1 gwei in every header (`chain.Config.BaseFee`). The Holocene EIP-1559 parameters op-node sends are recorded in `extraData`; no update rule runs. |
| Block limit | op-node delivers the SystemConfig gas limit in the payload attributes; block filling stops when metered gas reaches it (`chain/chain.go`, `applyBuild`). `MaxCyclesPerInput` (10⁹) caps one input, and exceeding it is a rollback the sender never pays for. |
| L1 data fee | The attributes deposit already delivers the L1 base fee, blob base fee, both fee scalars and the operator fee parameters, and the L1Block handler stores them (`app/src/handlers/l1block.ts`). The rollup config seeds the scalars with OP's own values (`rollup/config.go`, 1368 and 810949) because op-node refuses a zero scalar. Nothing reads them to charge. |
| Mempool | A FIFO gated on nonce (`mempool/mempool.go`). No fee admission, no ordering by tip, same-nonce replacement without a price bump — "which this chain's nominal fees could never clear", as the comment says. |
| Collection | The deployment names four fee vault recipients (RAAS.md §4). The guest has no vault accounts; a fee it debits goes nowhere. |
| Storage | The accounts drive (1 MiB) and ABI drive (256 KiB) are fixed at genesis. ACCOUNTS.md §5.7 names the exhaustion attack, makes nonce-bearing records permanent, and concludes that the account model *requires* a fee market rather than merely enabling it. |

The consequences compound, as RAAS.md §9 says: free ingress is a DoS
target, the chain cannot pay for its own L1 costs, and it has no way to
price the resource it actually consumes. The neighbouring README gaps — gas
as a constant rather than a measurement, `eth_estimateGas` returning the
cycle budget as an upper bound, a nonce that is enforced but free to mint —
are the same problem from three sides.

Underneath all of it is one structural fact: **the host measures and the
guest charges, and they do not talk.** The host's cycle count is not visible
inside the machine, and the `EvmAdvance` envelope has no field to carry it
(EVM-COMPAT §4 keeps the envelope's bytes). So the only thing the guest can
charge today is a constant. Every execution-pricing question below reduces
to closing that gap.

## 2. The resources, and how they differ from Ethereum's

**Execution is cycles.** The machine's native cost unit is the mcycle, and
DESIGN §9 already names it as the natural gas unit. Cycles are what a
verifier spends re-executing, what the sequencer spends building, and — for
settlement track B2 — literally the proving cost. The approximation that
every cycle costs the same is the one Stylus makes for WASM instructions and
the EVM gas schedule makes for opcodes; Dave's leaf is a microarchitecture
step whose count per instruction varies, so the approximation is imperfect
for track A too, and acceptable for the same reason it is everywhere else.

**Storage is a fixed pool, not a growing trie.** The machine's state is RAM
(`ram_length`, the CLI's 128 MiB default, since `cartesi.toml` sets none)
plus flash drives of fixed size, all fixed at genesis; changing the geometry
is a machine template upgrade, governed like a prestate change (ACCOUNTS.md
§8). Ethereum's storage problem is that state grows without bound and every
node pays for it forever. This chain's storage problem is different in
kind: **exhaustion of consensus capacity**. The accounts table has a fixed
number of slots; when it is full, no new sender can transact, and the
records that filled it can never be reclaimed. The secondary costs are real
but operational: a checkpoint is a copy of every non-pristine page (DESIGN
§7, half a gigabyte each), every dirty page is rehashed when the root is
taken, and a new node replays rather than syncs. None of them is the
proving cost, which is indifferent to state size.

What grows, then, is occupancy: of the accounts drive, of the ABI drive, of
application RAM, and of whatever the root filesystem writes. RAM is the one
that is not a table. A handler's scratch space and the application's
long-lived records are the same bytes in the same process, separated only by
whether the application keeps a reference — the EVM separates memory and
storage by construction, and this chain does not (§7).

**Data availability is the dominant real cost.** An OP chain's recurring
bill is the batcher's L1 gas (RAAS.md §6). This is the resource Ethereum
does not price and every L2 must, and it is the one for which the chain
already carries every parameter.

**Deposits are priced on L1.** `OptimismPortal` runs an EIP-1559-shaped
market over deposit gas (its `ResourceMetering`), so the L2 side executes
them free, as OP does. What the L2 side owes is to *honour* the gas limit
the depositor bought, which the guest does not do today.

## 3. Prior art, and what is borrowed from each

| Mechanism | Origin | Taken | Left |
|---|---|---|---|
| EIP-1559 base fee controller | Ethereum, OP | The update rule verbatim: op-node already supplies denominator and elasticity, and go-ethereum's `eip1559.CalcBaseFee` is a dependency. Base fee to the BaseFeeVault, not burned, as OP does. | The framing of 1559 as an auction. It is a feedback controller targeting half-full blocks; only the priority fee is an auction, and only for ordering under congestion. |
| Priority-fee ordering | geth txpool | The admission rules (`maxFee ≥ baseFee`, balance covers `gasLimit × maxFee + value`), the replacement bump. | Tip ordering is optional: Arbitrum's sequencer is first come first served and ignores tips, which is what the FIFO does now. Either is a mempool-only choice. |
| L1 data fee | OP, Fjord | The formula unchanged (§6): a FastLZ size estimate times the scalar-weighted L1 prices, to the L1FeeVault. | Nothing. The regression was fit on RLP-encoded signed transactions compressed by a stock batcher, which is exactly what this chain posts. |
| Operator fee | OP, Isthmus | `gasUsed × operatorFeeScalar / 10⁶ + operatorFeeConstant` to the OperatorFeeVault; deposits exempt. Added precisely for stack variants whose resource profile differs — proving cost, here. | The values, until a settlement track is chosen. |
| Deposit resource metering | OP, `OptimismPortal` | Already stock on L1. | Nothing; the L2 honours the bought limit. |
| Ink and the page limit | Arbitrum Stylus | A fixed, owner-adjustable ratio between the native unit and gas (10,000 ink per gas there; `CyclesPerGas` here), re-tuned when the VM gets faster. A declared memory ceiling per invocation (128 pages of 64 KiB). | Runtime-enforced memory metering, which needs a runtime the application code runs *inside* (§7). |
| Weights | Substrate | Benchmarked cost per operation, charged up front at the worst case and refunded against the measured cost after dispatch. The natural model for an operator-written handler set (§5.3). | Nothing; kept as a live alternative to runtime measurement. |
| Gas meter | Cosmos SDK | In-process cooperative metering: exhaustion raises inside the process and the framework recovers it (§5.5). | — |
| Per-transaction gas cap | Ethereum, EIP-7825 | One global cap on any single transaction, which `MaxCyclesPerInput` already is. | — |
| Rent-exempt deposit, storage staking | Solana, NEAR | A deposit per unit of persistent storage, locked while occupied, refunded on release, funded by a payer the program names (§7.2). | Rent as a flow, which cannot apply to records that may never be deleted. |
| Existential deposit, positive-balance rule | Polkadot, ewtools | Already adopted by ACCOUNTS.md §5.7. | — |
| Excess-gas exponential | Ethereum, EIP-4844 | The shape of a price that climbs as a *stock* nears a ceiling, for table occupancy (§7.2). | — |
| No gas at all | Cartesi Rollups | The application bounds its own execution and memory; the platform's budget is a safety net, not a meter. This remains the baseline for the application's own RAM (§7.3). | As the whole model: a chain with public ingress needs a price. |

## 4. The gas authority: the guest, with the host as backstop

Whatever is charged must be charged inside the state the root commits to —
the same argument DESIGN §8 makes for the outputs root and ACCOUNTS.md §1
for nonces. A referee cannot adjudicate a fee the shim debited in Go. That
rules out the obvious shortcut of letting the host's cycle count drive the
charge, and leaves two ways for the guest to know what an input cost: the
host tells it, or it measures itself.

The host cannot tell it without an envelope change, so **the guest measures
itself**, and the machine makes that possible. There is no wall clock in a
Cartesi Machine; `mtime` is a pure function of `mcycle` at a fixed divisor
(`RTC_FREQ_DIV`, 8192 cycles per tick in the emulator's `rtc-defines.h`;
the architecture page still says 100 — one of §11's checks), and Linux's
clocksource reads it. `CLOCK_MONOTONIC` inside the guest is therefore
deterministic and is consensus state like everything else in the machine.
The router reads it before and after dispatch, in the same process, and the
difference is the transaction's cycles to within one tick.

Two counts then exist for every input: the host's, from `mcycle` across the
yield, and the guest's, from its clock across dispatch. They differ by the
router's own overhead and by tick rounding, and they must not both be
authoritative. **The guest's count is canonical.** The header's `gasUsed`
is read out of guest state — a block-context slot at a well-known address,
exactly as the host reads balances out of the accounts drive — and a
verifier checks the header against it as it checks `stateRoot`. The host's
count survives in one role: `MaxCyclesPerInput` remains the halt
protection, the thing that guarantees a runaway input cannot stall block
production, and nothing about pricing depends on it.

The granularity is coarse. At `CyclesPerGas = 1000` a tick is eight gas,
and the router's fixed per-transaction overhead — envelope decoding,
signature recovery, ledger reads — is many ticks. That overhead becomes
the **intrinsic cost**, measured once and charged as a constant the way
Ethereum charges 21,000 for a plain transfer, so that the measured part is
the handler alone. The number is guest-specific: a JavaScript router
recovering secp256k1 in software pays orders of magnitude more than the
native router EVM-COMPAT §4 anticipates, and the intrinsic cost is where
that difference shows.

One consequence of measuring inside a managed runtime deserves its own
flag. The cycles a handler takes depend on the runtime's internal state —
garbage-collector timing, JIT warm-up, heap layout — all deterministic in
the machine, so every node agrees, but **path-dependent**: the same
transaction costs different gas at different points in the chain's history.
Ethereum's gas is state-independent for a given execution path, and tooling
assumes so when it estimates. A native router shrinks the effect; a
scheduled cost model (§5.3) removes it. This is one of the feasibility
doubts §11 is honest about.

## 5. Execution pricing

### 5.1 Units

Gas is cycles divided by `CyclesPerGas`. The ratio stays a single number
and becomes an **owner parameter in the guest** rather than a Go constant,
so it can be re-tuned when the emulator gets faster without a template
upgrade — Stylus's ink price, by another name. Receipts, `eth_estimateGas`
and headers all speak gas; cycles appear only in the `cartesi_` namespace.

### 5.2 Three limits, one of them global

Three things are called a gas limit in this chain and they belong to three
parties:

| Limit | Scope | Set by | Today |
|---|---|---|---|
| Block gas limit | One per block, chain-wide | The SystemConfig owner on L1, delivered by op-node in the payload attributes | Header `gasLimit`, 30M default |
| Transaction gas limit | Per transaction | The sender. For a deposit, the L1 depositor, who paid for it in the portal | The `gas` field, parsed and ignored |
| Per-input cap | A ceiling on any single input | Protocol parameter | `MaxCyclesPerInput`, 10⁹ cycles |

There is no gas limit per transaction *type*. The block limit is global by
construction; the per-input cap is one number because its job is to bound
the worst case a verifier or prover must execute for one input (EIP-7825
adds the same single cap to Ethereum); and the per-transaction limit is the
sender's declaration of how much they will buy. What a chain may want per
type is a *cost*, which is the next section.

### 5.3 Measured, scheduled, or both

How many gas an operation costs is a design fork with prior art on both
sides, and the choice between them is the largest open decision in this
document.

- **Measured.** Charge the cycles actually consumed, from the guest clock
  (§4). Handles arbitrary application code; exact up to a tick; subject to
  the path-dependence noted above; needs the intrinsic cost calibrated.
- **Scheduled.** Benchmark each operation offline and charge a fixed
  weight, possibly as a function of payload size. This is Substrate's
  weight system, built for app-specific chains with a known operation set,
  and Cosmos SDK's fixed gas per message type is the same idea. No runtime
  metering, state-independent by construction, trivially estimable — the
  schedule *is* the `eth_estimateGas` answer. Every operation must have a
  boundable worst case, so a handler that loops over user-supplied data
  needs an input size cap or it breaks the model.
- **Hybrid.** Substrate itself charges the scheduled weight up front and
  refunds against the measured weight after dispatch. Here that would give
  the built-in family — bridge, messenger, config, transfers, whose cost is
  predictable — scheduled costs, and application handlers measured cycles
  under a declared ceiling.

For an operator-written handler set the scheduled model is the cheapest to
build and the easiest to reason about, and it sidesteps §4's path
dependence entirely. The measured model is what makes the chain safe for
code the operator did not write, which is DESIGN §9's Level 1 and beyond.
The hybrid is recommended as the shape to design for, with the schedule
alone as the honest v1 if §11's clock experiments disappoint.

### 5.4 Prepay, charge, refund

Ethereum's sequence transfers directly, and the router's journal
(EVM-COMPAT §5) is the mechanism: debit `gasLimit × effectivePrice` and bump
the nonce before dispatch, run the handler, refund the unused part after.
The outcome table gains a column:

| Outcome | Ledger + value | Outputs | Nonce | Gas charged |
|---|---|---|---|---|
| ACCEPT | kept | emitted | bumped | measured (or scheduled), plus intrinsic |
| REVERT | rolled back | dropped | bumped | the same — a revert pays for the attempt |
| FAIL | kept | emitted | bumped | the same |
| Out of gas | rolled back | dropped | bumped | **all of the limit** |
| REJECT | — | — | — | nothing; nothing ran |

The flat fee the guest charges today is the degenerate case of this table
with `gasUsed = 0` and a nonzero intrinsic. It is also what ACCOUNTS.md
§5.7 wants priced: the first transaction from a fresh sender allocates a
permanent record, and the intrinsic cost on a fresh sender should be at
least that record's deposit (§7.2).

### 5.5 Out of gas in a single process

The design assumes the application is **one process at user level** —
asking application developers to run handlers out of process (EVM-COMPAT
§10, kind 3) is too hard an ask to make a prerequisite. In a single process
nothing can preempt a handler, so the per-transaction limit is enforced by
three mechanisms that stack:

1. **Cooperative checks at platform calls.** Every ledger operation,
   emission or API call the handler makes compares elapsed guest time
   against the transaction's budget and throws out of gas, which the router
   already turns into a revert that charges the sender. This is the Cosmos
   SDK gas meter — exhaustion raises inside the process, the framework
   recovers — and the EVM interpreter is itself in-process cooperative
   metering. The handler does not call a meter. A pure compute loop that
   never touches the platform escapes until the host cap.
2. **Scheduled worst cases.** Under §5.3's schedule, overrun is a bug in
   the operator's own handler, not an attack, and user-influenced loops
   carry input size caps.
3. **The host cap, with the charge made durable first.** Today an input
   that exceeds `MaxCyclesPerInput` is rolled back unpaid. To make it pay,
   the router would yield once *after* the prepay and *before* dispatch —
   an automatic yield whose response tells the guest whether to proceed —
   and the host would snapshot at that yield. Normally it answers proceed.
   If the continuation exceeds the cap, it rolls back to the snapshot and
   answers abort, and the guest records an out-of-gas revert with the
   prepay kept and finishes the input. The charge is in-state; a verifier
   applies the same rule. The cost is one extra fork per input and a small
   extension of the yield protocol, and it keeps exactly the host-side
   branch DESIGN §8 already has to reconcile with Dave's fixpoint model —
   neither better nor worse than today's rejection branch, but now with
   the sender paying for it.

Without (3), a cap overrun costs the sequencer the wasted cycles and the
sender nothing: a DoS surface bounded by the cap times the ingress rate
limit. That is an economic gap, not a liveness one, and a chain may choose
to live with it behind RAAS.md §7's gateway.

For a JavaScript guest there is a middle path between one process and two.
Node's worker threads accept `resourceLimits`, a worker that exceeds its
heap emits an error instead of aborting the process, and `terminate` is a
preemption primitive. That is a sandbox without a second process; but a
worker that holds state loses it when killed, so it suits transient work
only, and it is one more thing §11 has to measure on riscv64.

### 5.6 The base fee

The EIP-1559 update rule runs on the guest's own per-block gas total, which
the router accumulates from its measurements, so the base fee is in-state
and the next block's price is derivable by anyone with the previous block.
The base fee goes to the BaseFeeVault (§8), as OP does, not to a burn.

The gas *limit* is the awkward input. Its authority is the SystemConfig on
L1; op-node delivers it per block; and the guest cannot see it. Two
resolutions, in order of preference:

- **Mirror it.** The gas limit becomes a guest parameter the owner sets
  through the Config contract, the operator keeps it equal to the
  SystemConfig value, and the shim refuses to build a block whose
  attributes disagree with the guest. This is the same coordination
  discipline the chain already accepts for `CHAIN_ID`, baked into both the
  image and the chain flags (RAAS.md §2), and a disagreement halts rather
  than diverges.
- **Carry it.** At the next wire-touching fork, an envelope revision
  (EVM-COMPAT §4's "envelope slimming") carries the block gas limit and
  base fee explicitly, and the mirror goes away.

### 5.7 Ordering and admission

The mempool adopts geth's admission rules — `maxFeePerGas ≥ baseFee`,
balance at the head covering `gasLimit × maxFeePerGas + value`, a price bump
on same-nonce replacement — mirroring the guest's enforcement at ingress the
way the nonce gate already does. Ordering is a separate and smaller choice:
by effective tip, as geth does, or first come first served, as Arbitrum
does and the FIFO already does. Either way the tip is paid, to the
SequencerFeeVault, and `eth_maxPriorityFeePerGas` stops answering zero.

### 5.8 Deposits

Deposits stay free on L2 and the portal's resource metering stays their
price, as in OP. The L2 side owes one thing: the deposit's `gasLimit`
bounds its execution, through the same cooperative checks as §5.5, with
OP's semantics that the mint survives when the call runs out of gas.

That creates a **calibration coupling**. A deposit's gas limit is chosen by
L1 tooling with EVM assumptions — the devnet's `DEPOSIT_GAS_LIMIT` is
1,000,000; the standard bridge's `minGasLimit` is of the same order — and
once the guest honours it, `CyclesPerGas` must be set so that the guest's
deposit handling fits inside a stock bridge gas limit. Otherwise every
bridge deposit mints and then fails. The deposit path's cycle cost at the
chosen ratio is therefore one of §5.9's required measurements, and a hard
constraint on the ratio rather than a tuning knob.

### 5.9 Calibration

Nothing in this section has a number yet that was measured rather than
chosen. With the defaults, a block may hold 30 billion cycles and one input
a billion; the emulator executes neither within a two-second block, so the
real block limit today is wall clock and a single input can exceed the
block time alone. Before any of §5 is built:

- **Emulator throughput** on the target hardware, in cycles per second,
  which sets `gasLimit × CyclesPerGas` to fit the block time with margin
  and bounds `MaxCyclesPerInput` to a fraction of it.
- **Router overhead per transaction**, in ticks, for the intrinsic cost.
- **Deposit handling cost** at the candidate ratio, against stock bridge
  gas limits (§5.8).
- **Per-handler worst cases**, if §5.3 schedules any of them.

## 6. The data availability fee

OP's Fjord formula applies unchanged. For a signed transaction of
FastLZ-compressed size `s`:

```
estimatedSizeScaled = max(minTransactionSize × 10⁶, intercept + fastlzCoef × s)
l1FeeScaled         = baseFeeScalar × l1BaseFee × 16 + blobBaseFeeScalar × l1BlobBaseFee
l1Fee               = estimatedSizeScaled × l1FeeScaled / 10¹²
```

with `intercept = −42,585,600`, `fastlzCoef = 836,500` and
`minTransactionSize = 100`, the constants OP fit against two weeks of
mainnet batches compressed with Brotli. Every input is already in the
guest: the L1Block handler stores `basefee`, `blobBaseFee`,
`baseFeeScalar` and `blobBaseFeeScalar` from each attributes deposit, and
the rollup config seeds the scalars. The router computes the fee at
admission, debits it with the gas prepay, and credits the L1FeeVault. The
scalars are then what they are on every OP chain: the operator's lever for
recovering batcher spend, adjusted on L1 through the SystemConfig and
arriving in the guest with the next attributes deposit, with no guest
change. `eth_estimateGas` does not include it; the `GasPriceOracle`
predeploy's `getL1Fee` is the surface wallets use, and a façade at its
address serving the same formula from the same stored attributes is the
natural EVM-COMPAT §9 built-in.

## 7. Storage and memory

### 7.1 What the costs really are

Because the pool is fixed (§2), the storage fee's job is not to make nodes
whole for growth; it is to keep **capacity from being exhausted** by anyone
who can afford to, and to make the operational costs — checkpoint size,
rehash work — fall on whoever causes them. A flat price cannot do the first
job: a funded attacker fills a fixed table at any flat price, and Ethereum
survives the same attack only because its state is elastic. The mechanisms
below therefore price *occupancy*, not just allocation.

### 7.2 Platform-owned storage: the drives

For records the platform owns — the accounts table, the token registry and
balance tables of ACCOUNTS.md §5.5, the ABI drive — the model is a
**storage deposit**: allocating a record locks a deposit from a payer,
deleting it refunds the deposit. This is Solana's rent-exempt minimum and
NEAR's storage staking, and the platform enforces it generically while the
handler names the payer, exactly Solana's split between runtime and program.

- **Nonce-bearing records cannot be deleted** (ACCOUNTS.md §5.7), so their
  deposit is never refunded. It is simply a one-time price for permanence,
  and it is what the intrinsic cost on a fresh sender (§5.4) must cover.
- **Balance-only records** delete on reaching zero, under the existential
  minimum ACCOUNTS.md already adopts, and the deposit comes back.
- **The price climbs with occupancy.** The deposit for a new record is a
  function of `liveCount / capacity` that rises exponentially as the table
  nears full — the shape EIP-4844 uses for excess blob gas and Stylus for
  memory pages past the free allowance. This is the mechanism that actually
  protects capacity: as the table fills, the marginal record becomes
  unaffordable before the last slot is taken, and a chain whose table is
  genuinely in demand earns the signal to grow it.
- **Growing capacity is a template upgrade**, with the drive-geometry
  caveats of ACCOUNTS.md §8. The occupancy price is also the trigger for
  planning one.

### 7.3 Application RAM in a single process

Here the design departs from the EVM most sharply, and the departure is
forced. An application's persistent state is, by EVM-COMPAT §10a's rule,
"just RAM the application owns": a JavaScript variable in the same process
as the router. The platform cannot bound that in-process. V8 aborts the
process on heap exhaustion rather than throwing; Rust's default allocator
aborts too; a cgroup or rlimit on the process would convert "the chain
halts from RAM exhaustion" into "the chain halts from a kill" and save
nothing, because killing the process destroys the state it held. The
out-of-process handler that *would* make exhaustion a clean revert is the
ask this design declines to make.

What the per-input fork already gives is worth stating precisely, because
it is better than the doctrine suggests. Every input runs on a fork; a
handler that dies mid-input — an infinite loop hitting the host cap, a heap
abort — kills the fork, not the chain. The sequencer drops the transaction
(`applyBuild`), the verifier records no state change (`replayImport`). So:

- **Transient exhaustion in one input** is a dropped input. It costs the
  sequencer the wasted cycles and the sender nothing unless §5.5's abort
  protocol is built. An economic gap, bounded by the cap and the ingress
  rate limit.
- **Persistent exhaustion** is chain death. Once the long-lived heap sits
  at the limit, every input fails — and deposits are mandatory, so a
  failing deposit aborts block production and op-node retries the same
  attributes forever. No in-process mechanism bounds this. Only the
  application's economics can.

So the responsibility is split, and the split is the same one Cartesi
Rollups applications already live under. **The platform guarantees that
one input cannot take the chain down and that measured work is charged.
The application guarantees that its persistent footprint is bounded and
paid for.** The platform's contribution is to make the second guarantee
cheap to keep, with three things:

1. **A declared budget, in records.** The application states its persistent
   capacity at genesis — how many of each record it may hold — the way the
   accounts drive declares table capacity and tracks `liveCount`. Bytes
   from `process.memoryUsage()` are deterministic inside the machine but
   jump with garbage collection; a record count is exact, cheap, and what
   the application knows anyway.
2. **An admission check between inputs.** Before dispatch, the router
   compares occupancy against the budget and refuses *creation* of records
   beyond it with a revert. Approaching the ceiling then degrades to "no
   new accounts" rather than to chain death, and existing users keep
   transacting.
3. **Priced tables as a library.** Generalize the accounts drive:
   fixed-slot tables in a flash drive or NVRAM range, allocated against a
   deposit a named payer funds, refunded on deletion, priced by occupancy
   as in §7.2 — `app.table({ capacity, slotSize, deposit })` beside
   `app.contract`. ACCOUNTS.md §5.5 and §5.6 already sketch the sparse
   table and per-app namespacing. An application that keeps user-created
   records there inherits exhaustion economics without designing them; one
   that keeps records in the heap owns the pricing, with (1) and (2) as its
   instruments.

The RAM budget then stops being an unexamined default and becomes a genesis
calculation the operator writes down: kernel and runtime baseline, plus the
persistent cap, plus the worst-case transient allocation of one input,
equals `ram_length`. The devnet's demo is on the wrong side of this line
today — one process, a counter in the heap, nothing bounding it — and is
harmless only because a counter is one integer. An application with a
user-keyed map in that position is §7.1's exhaustion attack with no fee in
front of it.

### 7.4 Transient memory

Per-input scratch allocation is bounded implicitly by cycles (touching
memory costs them), by the mempool's 128 KiB transaction cap, and by the
handler's own discipline. There is no in-process recovery from a transient
out-of-memory, so the operator's worst-case allocation per input is a
number in the RAM budget above, not something the platform meters. That is
the Cartesi Rollups contract, unchanged.

## 8. Collection: the fee vaults

Fees go to the four OP fee vaults, as ledger accounts at the predeploy
addresses the OP tooling expects:

| Vault | Address | Receives |
|---|---|---|
| SequencerFeeVault | `0x4200000000000000000000000000000000000011` | priority fees |
| BaseFeeVault | `0x4200000000000000000000000000000000000019` | base fee × gas |
| L1FeeVault | `0x420000000000000000000000000000000000001A` | the L1 data fee (§6) |
| OperatorFeeVault | `0x420000000000000000000000000000000000001B` | the operator fee |

On OP each vault is a contract that withdraws to a recipient on L1 through
the standard bridge once a minimum accrues. Here the vaults are accounts
the ledger credits, and withdrawal is a built-in handler at each address
that the recipient (an owner parameter, RAAS.md §4's four recipients) calls
to initiate an OP withdrawal through the messenger the guest already speaks
(DESIGN §5). No new L1 contract.

The operator fee is the slot for **proving cost**. Under track B2 the
prover's bill is a function of cycles, and `operatorFeeScalar` prices that
separately from congestion; under track A it is mostly bonds and a validator
daemon, and the constant term fits better. The parameters already arrive in
the guest with every attributes deposit and are zero in the rollup config.
Setting them waits for a settlement track and a cost per block (DESIGN §8,
roadmap step 6).

## 9. The RPC surface

`eth_gasPrice` and `eth_feeHistory` stop synthesizing and start reporting:
the base fee the guest computed, the tips the sequencer actually received.
`eth_estimateGas` runs the payload through `EvmSimulate` on a discarded fork
— the hook EVM-COMPAT §7 reserved for exactly this — and reports the guest's
measured cost plus a margin, or the scheduled weight with no simulation at
all under §5.3's schedule. Receipts carry the real `effectiveGasPrice` and
a `l1Fee` field as OP receipts do. `eth_maxPriorityFeePerGas` reports a
tip once tips buy something.

## 10. What changes where

| Where | What |
|---|---|
| **Guest** (`app/`) | Clock reads around dispatch; intrinsic cost; prepay, charge, refund in the journal; cooperative out-of-gas checks in the platform API; EIP-1559 base fee from accumulated gas; `CyclesPerGas` and the gas limit as Config parameters; the L1 fee formula over stored L1Block attributes; vault accounts and their withdrawal handlers; a block-context slot (gasUsed, baseFee) at a well-known address; storage deposits and occupancy pricing in the drive libraries; the priced-table API. |
| **Shim** (`chain/`, `engineapi/`) | Read `gasUsed` and `baseFee` from guest state and verify headers against them; drop `CyclesPerGas` as a Go constant; EIP-1559 header fields from the guest's values; refuse to build when the attributes' gas limit disagrees with the guest's; `eth_estimateGas` over `EvmSimulate`; honest `eth_gasPrice`, `eth_feeHistory`, receipts. Optionally the proceed/abort snapshot of §5.5. |
| **Mempool** | geth's admission rules; a replacement bump; ordering by tip or FIFO by choice. |
| **Rollup config** | Operator fee parameters once chosen; scalars as the operator's DA lever, already present. |
| **Devnet** | A nonzero flat fee by default, funded by the deposit the scripts already make; `ram_length` set deliberately; the §5.9 measurements as a script. |
| **Tests** | Builder and verifier agree on `gasUsed` read from the guest; the same transaction replayed twice measures the same cycles (determinism); the deposit path fits a stock bridge gas limit; occupancy pricing refuses the last slot before it is taken. |

## 11. What would have to be true

Each of these is unverified, and each has an experiment. Together they are
the go/no-go for everything above.

1. **The guest clock is deterministic and reads `mcycle`.** Run the same
   input twice from the same snapshot and compare `clock_gettime` deltas;
   confirm the kernel's clocksource is the RISC-V timer, not a jiffies
   fallback; confirm the divisor on emulator 0.21 (source says 8192, docs
   say 100).
2. **Tick granularity is fine enough.** Measure the router's overhead and a
   small handler in ticks. If a trivial transaction is a handful of ticks,
   measurement is too coarse to price handlers and §5.3 must schedule.
3. **Path dependence is tolerable.** Replay one transaction at many points
   in a long chain and plot its measured cycles. If the spread is a large
   fraction of the mean, a JavaScript guest cannot be priced by measurement
   and the choice is a native router or a schedule.
4. **The emulator fits the block.** Cycles per second on target hardware,
   against a two-second block, sets whether a 30M-gas block is even
   possible and what `MaxCyclesPerInput` must drop to.
5. **Deposits fit stock gas limits** at the ratio (4) implies (§5.8). If
   they do not, the ratio is constrained from both sides and may have no
   solution without a native router.
6. **The host can read the guest's block context** the way it reads the
   accounts drive, and a verifier reproduces it bit for bit.
7. **The gas-limit mirror is operable** — the owner can keep a guest
   parameter equal to the SystemConfig without the chain stalling on every
   change — or the envelope revision is scheduled.
8. **The proceed/abort yield is expressible** through the CMIO yield the
   guest tools already use (an automatic yield whose response is read), and
   its host-side branch is acceptable to whichever settlement definition
   DESIGN §8 adopts. If not, out-of-gas-pays is dropped and the sequencer
   bears cap overruns.
9. **Worker threads behave on riscv64 Node 24** — `resourceLimits` yields
   an error rather than an abort — if the JavaScript middle path of §5.5 is
   wanted.
10. **A testnet can ship without any of it**, behind RAAS.md §7's gateway
    and a nonzero flat fee, which is the posture RAAS.md §10 already takes.
    Mainnet cannot, and this list is how long the pole is.

## 12. Alternatives considered

**Host-authoritative gas.** The host measures, the shim charges. Rejected
because the shim cannot write state: a fee debited in Go is outside the
state root and outside anything a referee can check, the move DESIGN §8
forbids for the outputs root.

**Carrying the cycle count in the envelope.** The cleanest way for the host
to tell the guest, and impossible without an envelope revision, which
EVM-COMPAT §4 defers to a wire-touching fork. Kept as §5.6's second option.

**Out-of-process handlers by default.** Makes memory exhaustion a revert
and preemption a kill, and is the design EVM-COMPAT §10 kind 3 describes.
Declined as a prerequisite: too hard an ask of application developers, who
will most likely ship a single process. §7.3 is what is left.

**Runtime-enforced metering, Stylus-style.** Exact, preemptible, memory
included — and only available to code that runs inside a runtime the
platform controls, which is DESIGN §9 Level 2, not this chain's native
handlers.

**An EVM in the guest for the sake of its gas.** Rejected for the reasons
EVM-COMPAT §12 gives; the gas schedule is the part of the EVM this chain
least wants.

**No fee market, a rate limiter only.** RAAS.md's testnet posture, and the
right one for a testnet. Not a mainnet answer.

## 13. Deliberately out of scope

- Metering code the operator did not write — DESIGN §9 Levels 1–2 — beyond
  noting which mechanisms here (measured cycles, the admission check) would
  extend to it and which (the schedule) would not.
- The operator fee's values, which wait for a settlement track.
- Blob DA, which changes the L1 fee's inputs and not its formula (RAAS.md
  §9).
- The envelope revision, beyond naming what it would carry.
- Where the platform's own per-block overhead — the attributes deposit, the
  outputs accumulator — is charged. Today it is free, as OP's system
  transactions are, and that stays.

## 14. Roadmap

0. **Measure.** The experiments of §11 and the numbers of §5.9, as a script
   in the devnet that anyone can rerun. One to two weeks. This step decides
   between measured and scheduled costs and may redirect everything after
   it; it commits to nothing.
1. **Host-only.** geth's admission rules in the mempool; EIP-1559 header
   fields computed by the shim from host-measured gas as an interim; a
   nonzero flat fee on the devnet; `eth_estimateGas` over `EvmSimulate`.
   No guest change. Headers become honest and wallets stop guessing.
2. **Guest.** The gas authority moves into the guest (§4): clock reads or
   the schedule, prepay and refund, cooperative out-of-gas, the base fee
   from accumulated gas, the L1 fee, the vaults, the block-context slot
   the shim verifies against. The interim of step 1 is removed.
3. **Storage.** Deposits and occupancy pricing in the drive libraries; the
   record budget and admission check; the priced-table API.
4. **Operator fee**, with a settlement track and a cost per block.

## References

- This repo: DESIGN §4 (receipts doctrine), §7 (checkpoints), §8 (what
  settlement requires, and the host-side branch), §9 (metering for
  multi-tenancy); ACCOUNTS.md §5.7 (exhaustion), §8 (sizing and upgrades);
  EVM-COMPAT §4 (envelope), §5 (outcomes and the journal), §7
  (`EvmSimulate`), §10 (handler kinds); RAAS.md §7 (the gateway as the only
  ingress defence), §9 ("Inputs are free"), §10 (build order).
- Cartesi machine time: architecture page,
  https://docs.cartesi.io/cartesi-machine/target/architecture/ ; the
  emulator's `src/rtc-defines.h` (`RTC_FREQ_DIV_DEF`).
- OP Stack fees: Fjord L1 data fee,
  https://specs.optimism.io/protocol/fjord/exec-engine.html ; Isthmus
  operator fee, https://specs.optimism.io/protocol/isthmus/exec-engine.html ;
  predeploys and fee vaults, https://specs.optimism.io/protocol/predeploys.html ;
  deposit resource metering, https://specs.optimism.io/protocol/deposits.html .
- EIP-1559 (base fee), EIP-4844 (excess gas pricing shape), EIP-7825
  (per-transaction gas cap).
- Arbitrum Stylus gas metering and ink,
  https://docs.arbitrum.io/stylus/concepts/gas-metering .
- Substrate weights and benchmarking; Cosmos SDK `GasMeter`.
- Solana rent-exempt accounts; NEAR storage staking.
