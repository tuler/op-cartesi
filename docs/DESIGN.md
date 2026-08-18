# Cartesi Machine as an L2 Execution Layer: Reusing OP Stack / Arbitrum Components

**Goal:** run a chain whose state transition function is a Cartesi Machine (RISC-V, Linux) instead of an EVM, while writing as little new code as possible — reusing mature components for L1 interaction, sequencing, data availability, derivation, and dispute resolution.

**TL;DR:** The OP Stack is the right donor. It has two clean, already-exploited seams: the **Engine API** boundary between `op-node` and the execution engine (precedent: Monomer put a Cosmos SDK app behind it), and the **DisputeGameFactory** game-type registry on L1 (precedent: Cannon, Asterisc, OP Succinct, and Kailua all coexist as registered game types on the same contracts). Arbitrum Nitro has no equivalent seams — its STF *is* Geth+ArbOS compiled into the WAVM replay binary, so swapping the execution layer means forking the heart of Nitro. Two plans below, both OP Stack-based; they share ~90% of the work and differ only in the settlement/proving layer.

---

## 1. What the Cartesi Machine actually provides (from the `machine-emulator` code)

Reading the repo (`main` as of July 2026), the relevant capability surface for an L2 execution layer is:

**Deterministic RV64GC machine with a Merkleized state.** The emulator implements the full RISC-V RV64GC ISA (privileged + unprivileged), boots Linux, and is deterministic down to floating point. The entire 64-bit physical address space is committed to a hash tree; `src/cm.h` exposes `cm_get_root_hash()`, `cm_get_node_hash()`, and `cm_get_proof()` for Merkle proofs of any word/range against the root. This root hash is a natural **L2 output root / state root**.

**A rollup-shaped I/O boundary (CMIO).** `cm_receive_cmio_request()` / `cm_send_cmio_response()` implement a yield-based input/output protocol: the guest yields, the host feeds it an input, the guest emits outputs — exactly the advance-state loop Cartesi Rollups applications already use. Critically, `cm_send_cmio_response` has a logged counterpart, `cm_log_send_cmio_response()` + `cm_verify_send_cmio_response()`, so *feeding an input is itself a provable state transition*.

**Provable stepping at two granularities.**
- `cm_log_step(m, mcycle_count, ...)` + `cm_verify_step()` — logs/verifies a big-step transition over N machine cycles (the "computation hash" / step-log machinery).
- `cm_log_step_uarch()` / `cm_verify_step_uarch()` + `cm_log_reset_uarch()` — the microarchitecture: the emulator's own interpreter compiled to a tiny RISC-V core whose single step is small enough to replay **on-chain in Solidity** (the `cartesi/machine-solidity-step` repo). This is the leaf of Cartesi's interactive fraud proofs (Dave/PRT).

**A ZK path, in-repo.** The `risc0/` directory contains a working RISC Zero prover pipeline: `cartesi-risc0-cli prove <hash_before> step.log <mcycle> <hash_after>` produces a STARK receipt, compressible to a **Groth16 seal (~260 bytes, ~300k gas)** verified on-chain via the RISC Zero Verifier Router; Solidity integration lives in `risc0/solidity/`. So a machine state transition can be settled either interactively (uarch replay) or with a single ZK proof.

**Operational machinery a node needs.** Forking and rollback (`cm_clone_*`, revert-root-hash APIs, snapshots via `cm_store`/`cm_load`), a JSON-RPC remote machine server (`cm-jsonrpc.h`) so the machine can run as a separate process, freestanding/WASM compilation targets, and a C API designed for FFI.

The one thing it is **not**: an Ethereum execution client. No blocks, no transactions, no receipts, no `eth_*` RPC, no mempool. Everything in the plans below is about bridging that gap with as thin a layer as possible, and reusing everything else.

---

## 2. Where the donor stacks are (and aren't) modular

### OP Stack — two designed-in seams

The OP Stack splits an L2 node into a **consensus/rollup node** (`op-node`, or the Rust `kona-node`) and an **execution engine**, connected by the (slightly extended) Ethereum **Engine API**: `op-node` derives the L2 chain from L1 (batches + deposits), then drives the engine with `engine_forkchoiceUpdatedV3` / `engine_getPayloadV3` / `engine_newPayloadV3`. The engine is explicitly pluggable — op-geth, op-reth, op-erigon all sit behind the same interface, and **Monomer (Polymer/Nethermind) proved the seam works for non-EVM engines** by translating Engine API ↔ ABCI so Cosmos SDK apps run as the OP Stack execution layer. (Monomer is paused, but it's a working, readable Go reference for exactly the adapter you need.)

On L1, the settlement side is equally pluggable: `OptimismPortal` validates withdrawals against outputs proposed through the **DisputeGameFactory**, which dispatches by `GameType`. The deployed registry already includes `CANNON` (MIPS FPVM), `ASTERISC`/`ASTERISC_KONA` (RISC-V FPVM), `OP_SUCCINCT` (SP1 validity proofs), and `KAILUA` (RISC Zero hybrid ZK fraud/validity proofs). Adding a game type is a supported, production-exercised extension point — you deploy a new game implementation and register it; the portal, factory, batcher, proposer, and challenger tooling stay stock.

Around those seams, the reusable inventory is large: `op-batcher` (calldata/blob batch submission, compression, channel framing), `op-proposer` (posts output roots / creates games), `op-challenger` (dispute participation framework), `op-conductor` (HA sequencer failover), `op-deployer`, the bridge contract suite, and op-node's built-in sequencer mode with unsafe-head P2P gossip. That is your "Ethereum interaction + sequencer" subsystem, off the shelf.

### Arbitrum Nitro — one binary, no seam

Nitro's architecture is the "Geth sandwich": Geth core at the bottom, ArbOS in the middle (batch parsing, L1 fee accounting, bridging, block production), node software on top — and the **STF is defined as this Go code compiled to a WAVM replay binary**, whose Merkle root (`wasmModuleRoot`) is pinned in the L1 rollup contract for fraud proofs. Execution, derivation, and proving are one artifact. Arbitrum's own docs on customizing the STF describe it as "build a modified Nitro node Docker image" and warn that any change produces a new module root requiring coordination with Offchain Labs.

To put a Cartesi Machine here you would have to replace the Geth+ArbOS core inside Nitro *and* make the result compile to WAVM for BoLD-style disputes (the emulator is C++; the replay binary toolchain is Go→WASM), or replace the prover too — at which point you're rewriting the subsystems you wanted to reuse. Stylus doesn't help: it's a WASM contract runtime *inside* the EVM chain, not an alternative execution layer. **Conclusion: Nitro fails the "write as little code as possible" test.** Nothing from Nitro is worth extracting that the OP Stack doesn't offer with a cleaner boundary, so both plans below are OP Stack plans.

---

## 3. The shared core: `op-cartesi`, an Engine API shim (both plans need this)

This is the one genuinely new component, and it's Monomer-shaped: a Go (or TS/Rust) service that speaks the Engine API + a minimal `eth_*` subset to `op-node` on one side, and the Cartesi Machine JSON-RPC / C API on the other. Responsibilities:

1. **Block production** (`engine_forkchoiceUpdatedV3` with payload attributes → `engine_getPayloadV3`): take the attributes from op-node — timestamp, L1-origin info, and the mandatory deposit transactions — plus pending user transactions from its own tiny mempool (`eth_sendRawTransaction` endpoint; there is no public L2 mempool in the OP Stack, so the sequencer's shim is the only ingress). Feed each item into the machine as a CMIO input (`cm_send_cmio_response`), run to the next yield, then synthesize an L2 block: an EVM-style header whose `stateRoot` field carries `cm_get_root_hash()`, whose body is the input list, and whose hash chains to the parent. Receipts can be minimal/synthetic.
2. **Block import** (`engine_newPayloadV3`): verifiers replay the same inputs into their machine and check the resulting root hash against the header — this is what makes derivation-from-L1 work identically on every node.
3. **Fork choice and reorgs**: map `forkchoiceUpdated`'s unsafe/safe/finalized heads onto machine snapshots. This is where the emulator's fork/rollback/revert-root-hash and `cm_store`/`cm_load` APIs earn their keep: keep periodic snapshots keyed by block hash, roll back on L1 reorg. (Monomer does the analogous thing with Cosmos state; their `builder`/`engine` packages are the reference.)
4. **Deposit semantics**: op-node *will* inject the L1-attributes deposit and user deposits (ETH/token bridge mints) as the first transactions of every block. The guest program inside the machine must parse and honor them (credit balances, record L1 block info) for the standard bridge to be sound. This mirrors Cartesi Rollups' InputBox/`EtherPortal` pattern, just arriving in OP's deposit-transaction encoding, which the guest can ABI-decode like any other input.
5. **Minimal `eth_*` surface** for op-node, batcher, and proposer: `eth_chainId`, `eth_getBlockByNumber/Hash`, `eth_syncing`, and whatever `op-batcher` needs to read blocks for batching. Everything app-facing keeps using inspect/GraphQL-style reads against the machine directly — you don't need to fake a full EVM RPC.

Estimated scope: a few thousand lines plus tests. It's the price of admission for both plans, it's the piece with the best prior art to crib from, and note the leverage: **op-node, op-batcher, op-proposer, op-conductor, sequencer mode, P2P, blob DA, and the entire derivation pipeline come for free once this shim exists.**

![The devnet at a glance: OP Stack above the line, Cartesi execution layer below it](diagrams/devnet-overview.svg)

*The shim's seat, as the devnet runs it. Sequencing is a split job: op-node
triggers block production over the Engine API and pins the deposits; the shim
fills the rest of the block from its own mempool and computes it in the
machine, returning the machine Merkle root as `stateRoot` and the outputs
tree root as `withdrawalsRoot`. The full runtime, process by process:*

![Devnet runtime components: anvil L1 with the OP and outputs contract suites, the OP services, sequencer and verifier stacks of shim plus machine, and the viem scripts](diagrams/devnet-components.svg)

*Accent-stroked boxes are this repo's code (shim, routed guest, scripts,
outputs contracts); plain boxes are stock tooling (OP Stack, foundry, the
emulator); dashed boxes are startup-time artifacts. The verifier column is
the rollup property made visible: it rebuilds the same chain from nothing
but what the batcher posted to L1.*

One design decision to make early: **input granularity**. Either (a) one CMIO input per transaction (simple, matches Cartesi Rollups today, coarse dispute granularity at the input level), or (b) one CMIO input per block containing the ordered tx list (fewer yields, block-atomic). (a) composes better with existing Cartesi tooling and with Plan A's dispute story.

**Settled (a), with Cartesi's envelope.** Each transaction is one CMIO input, wrapped in `EvmAdvance(chainId, appContract, msgSender, blockNumber, blockTimestamp, prevRandao, index, bytes payload)` — the encoding Cartesi's guest tools already decode — with the raw transaction as the payload. Feeding raw transaction bytes was tried first and fails against a stock guest: the guest cannot parse them, exits, and halts the machine. The envelope is also what conveys the L2 block context, which the machine has no other way to learn. Indices are chain-wide, since an app-chain is one application. Every field is derivable from the block header, so verifiers reconstruct the builder's context exactly.

---

## 4. Plan A — OP Stack chassis + Cartesi-native settlement (Dave/PRT + `machine-solidity-step`)

**Thesis:** reuse OP for everything off-chain and for L1 plumbing (portal, factory, batcher, proposer, bridges), but settle disputes with the fraud-proof machinery Cartesi already built for exactly this VM — the Dave/PRT tournament contracts bisecting to a uarch step replayed by `machine-solidity-step`, or (cheaper leaf) a single Groth16 seal from the in-repo RISC Zero prover.

**How it fits together:**

- `op-proposer` posts output roots by creating games via `DisputeGameFactory.create(CARTESI_GAME_TYPE, claim, extraData)`. The claim is (a commitment to) the machine root hash at the epoch boundary.
- You write a `CartesiDisputeGame` contract implementing OP's `IDisputeGame` interface, which internally *is* a Dave/PRT tournament (or delegates to one). **Corrected by §7e:** this sentence is true and badly understates the work — the adapter is the small part, and `DaveConsensus` already implements the validator interface this repo calls, which makes wrapping a game the *more* expensive of two shapes. The one-step leaf is either `machine-solidity-step` uarch replay (pure Solidity, exists today) or a `cm_log_step` → RISC Zero receipt → Groth16 verification against the on-chain image ID (also exists today, in `risc0/solidity/`). The second option makes the on-chain leaf a single ~300k-gas verification instead of a uarch replay, and shrinks the tournament depth because one proof can cover a large `mcycle_count`.
- `OptimismPortal` is configured with your game type as the respected game; withdrawals then flow through the standard `proveWithdrawalTransaction` path — except withdrawal proofs are Merkle proofs **into the machine's hash tree** (via `cm_get_proof`) rather than MPT storage proofs. This needs a small portal/`CartesiOutputVerifier` adaptation: either fork `OptimismPortal`'s proof-verification library to verify Cartesi hash-tree proofs against a designated "outputs" memory range, or — lower-effort and closer to what you run today — keep OP's portal for ETH/token deposits only and route withdrawals through Cartesi Rollups' existing voucher/`executeVoucher` flow anchored to the accepted game outcome. For applications already built on the voucher path, the second option is nearly free.

  **Settled by implementation:** option 2 is built and works — see §7c. The bridge turned out to be one small contract, because an OP proposal already commits to the Cartesi outputs root and Cartesi's `Application` asks exactly one question before executing an output.

  **Refined by §7:** both options stay on the table, but they now verify against a commitment the stock OP plumbing already publishes. From Isthmus onward, op-node reads the withdrawal commitment straight out of the header's `withdrawalsRoot` field instead of proving it against the state trie, so the Cartesi outputs Merkle root goes there directly and `op-proposer` needs no changes. What Isthmus does *not* do is teach `OptimismPortal` to verify Cartesi proofs — that still requires option 1 or option 2. See §7 for both, and for why the pre-Isthmus path cannot be implemented at all.

**The subtle hard part: disputes must cover derivation, not just execution.** *(§7e sharpens this into the decision it actually is, having read Dave's `IDataProvider`.)* In stock OP, the fault-proof program (op-program/Kona) re-derives the disputed block *from L1 data* inside the FPVM, so a malicious sequencer can't win by lying about inputs. Your equivalent: the dispute must pin the machine's input sequence to L1. Two ways, in increasing order of reuse-of-what-you-know:

1. **Calldata-anchored inputs (v1):** run the batcher in calldata mode (or mirror inputs through an InputBox-style contract). The input Merkle root per epoch is then computable on-chain/on-challenge, and Dave's existing input handling applies essentially unchanged. This is exactly the Cartesi Rollups trust model today — lowest new code, modestly higher DA cost.
2. **Blob-anchored inputs (v2):** batches in EIP-4844 blobs; the dispute game needs a preimage/`kzg` step tying blob commitments to the input hashes the machine consumed (the same problem Cannon's `PreimageOracle` solves — its 4844 preimage support is reusable as a contract dependency). Do this only after v1 works.

**New code in Plan A:** the shim (§3), the `IDisputeGame` wrapper around Dave (Solidity, moderate), the guest-side deposit-tx decoder (small), and the withdrawal anchoring choice. **Reused:** all OP off-chain services and contracts, all Cartesi proving machinery, the existing app stack.

**Risks/limits:** Dave/PRT is the ecosystem's own frontier software (audit maturity ≠ Cannon's); the `IDisputeGame` bridging between OP's resolution semantics (clock, bonds, `resolve()`, airgap delay) and PRT's tournament semantics needs careful design; calldata-first DA costs more until v2.

---

## 5. Plan B — OP Stack chassis + OP's own proving stack (maximum reuse, two flavors)

**Thesis:** keep even the settlement layer stock by making the Cartesi state transition *provable by machinery OP already ships*. Same shim, zero-to-minimal new contracts. The trick in both flavors is the same: write a **`cartesi-program`** — the analogue of op-program/Kona — a self-contained, deterministic client that (a) runs OP derivation to reconstruct the input sequence from L1 data via the preimage oracle, and (b) executes those inputs by *embedding the Cartesi machine emulator itself* (the repo explicitly supports freestanding compilation for embedding, e.g. in a zkVM), asserting the final root hash. Derivation logic comes from Kona (Rust, designed as reusable `no_std` crates) — you compose crates and swap the execution backend, rather than writing derivation.

**Flavor B1 — Asterisc (interactive):** compile `cartesi-program` to RISC-V and prove it in **Asterisc**, OP's RISC-V FPVM, using the **stock `FaultDisputeGame`** and stock `op-challenger` (Asterisc deliberately mirrors Cannon's binary interface for challenger compatibility). New contracts: none — you deploy the standard game with your absolute prestate (the `cartesi-program` ELF commitment). The cost is emulator-inside-FPVM: Asterisc interprets the emulator interpreting your app. Off-chain that's a constant-factor slowdown on trace generation (bisection keeps on-chain work at one instruction regardless), but worst-case trace length grows by the emulation factor, so epochs must stay small enough that a challenger can generate the trace within the game clock. Also mind ISA scope: the emulator must build against Asterisc's supported RISC-V subset (rv64 IMAC-ish, no FPU/vector in the guest program — the *emulated* machine can still be full RV64GC, since its FP is software inside the emulator's own code... but verify which host instructions the freestanding build emits; you may need `-mno-*` flags or softfloat).

**Flavor B2 — ZK, Kailua-style (recommended flavor):** prove `cartesi-program` in the **RISC Zero zkVM** instead, and settle through a **Kailua-style hybrid game** (ZK fraud proof on dispute, optional heartbeat validity proofs, single-transaction resolution, no bisection). This is unusually well-aligned because (a) Kailua is exactly "Kona inside RISC Zero" with the execution backend being the thing you'd swap, (b) the machine-emulator repo *already contains* the RISC Zero guest for machine state transitions, its image-ID pipeline, and the Groth16 on-chain verification contracts, and (c) it upgrades cleanly to full validity proofs (fast finality — attractive for latency-sensitive bridging) by turning up proof frequency, with proving cost borne per-epoch rather than per-dispute. Worst-case proving cost is real money (Kailua's own estimate for full Kona fault proofs is on the order of ~100B cycles ≈ ~$100/hour-scale per worst-case proof; an embedded second emulator multiplies cycles), so benchmark cycles-per-input for the target application early — a typical advance handler is cheap per input, but Linux boot and syscall overhead inside a zkVM-embedded emulator is the number to measure. Mitigation: prove *machine step logs* directly with the in-repo prover (skipping the emulator-in-zkVM layer) and prove derivation separately, composing the receipts — more design work, far fewer cycles.

**New code in Plan B:** the shim (§3), `cartesi-program` (Rust: Kona derivation crates + emulator FFI + oracle plumbing — this is the substantial piece, but it's composition, not a subsystem), build/prestate tooling. **Reused:** everything in Plan A's OP list *plus* stock dispute contracts (B1) or Kailua's game + the repo's own ZK pipeline (B2).

---

## 6. Comparison and recommendation

| | Plan A (Dave settlement) | Plan B1 (Asterisc) | Plan B2 (ZK / Kailua-style) |
|---|---|---|---|
| New off-chain code | Engine shim | Engine shim + `cartesi-program` | Engine shim + `cartesi-program` (or receipt composition) |
| New contracts | `IDisputeGame`→Dave wrapper (+ withdrawal anchoring) | ~none (stock FDG, new prestate) | ~none (Kailua game + existing risc0 verifier) |
| Proving maturity | Cartesi-native (Dave, solidity-step) | OP-native (Asterisc is a registered game type) | RISC Zero-native (Kailua audited, deployed) |
| Perf risk | Low (native machine, native proofs) | Trace blowup from emulator-in-FPVM | Proving cost per worst-case epoch |
| Finality path | 7d-style optimistic | 7d-style optimistic | Hours; upgradable to validity |
| Leverages existing Cartesi stack | Most (vouchers, InputBox model) | Least | Middle |

**Recommendation:** Build the **Engine API shim first** — it's plan-independent, it's the enabler for everything, and with just the shim + stock op-node/batcher/proposer + a *permissioned* game type you already have a running chain with mature sequencing, DA, and bridging (this is how every OP chain, including Base, launched: proofs came later). Then decide settlement with real data: prototype **Plan A v1 (calldata + Dave)** because it reuses what the Cartesi ecosystem has already built and de-risks the known withdrawal path, while benchmarking **Plan B2's** cycle counts in parallel — if proving costs come in sane, B2 is the better end-state (fewer bespoke contracts, fast finality for bridge UX). Skip B1 unless you specifically want to avoid ZK dependencies; skip Arbitrum entirely.

**Suggested sequence:**
1. ~~Shim MVP against a local machine + op-node in sequencer mode on an L1 devnet (no proofs; permissioned/`FAST` game type). Milestone: deposits credited in-guest, blocks derived identically by a second verifier node from L1 data alone.~~ **Done**, on a devnet with anvil as L1: op-batcher posts calldata batches, a second node rebuilds identical blocks from them, and an L1 `TransactionDeposited` event becomes a balance the guest keeps and `eth_call` reads back. Isthmus and the `withdrawalsRoot` commitment came along early, since step 2 could not have been reached without them.
2. Batcher/proposer integration; snapshot-based reorg handling. **Batcher, contracts and proposer done.** `op-deployer` deploys the L1 suite onto the devnet L1 and `op-proposer` creates a game per proposal; the root claim recorded on L1 is `keccak(0³² ‖ stateRoot ‖ withdrawalsRoot ‖ blockHash)`, so it commits to the machine's Merkle root and the Cartesi outputs tree at once. Deposits arrive through the real `OptimismPortal`. Two things a devnet L1 needs that the standard path does not give: a `custom` intent, because the standard one resolves OPCM from a per-chain table keyed on L1 chain id (this also makes `op-deployer bootstrap` unnecessary, since apply deploys the implementations itself); and using only `inspect l1`, since `inspect genesis` and `inspect rollup` describe an op-geth L2 that does not exist here. The fee scalars are read back off the deployed SystemConfig rather than assumed — the blob scalar really was different from the value we had hardcoded.

   Proposals go into the **permissioned** game and are never disputed: there is no fault proof VM that can execute a Cartesi Machine, which is exactly what step 3 is for. Deploying `OptimismPortal` likewise does not make withdrawals work, since `proveWithdrawalTransaction` verifies an MPT storage proof against a state root that is a Cartesi hash tree — see §7.

   Persistence is done too — see §7b. A node restarts from a store rather than losing the chain, and the whole of step 2 is now complete.

3. Settlement track A: wrap Dave in `IDisputeGame`, calldata batches, voucher-based withdrawals anchored to resolved games.
4. Settlement track B (parallel, measurement-only): compile the freestanding emulator into a RISC Zero guest, measure cycles/input and cycles/epoch for the target application workload; go/no-go on B2.

## 7. Outputs, reports, inspect — and what becomes a receipt

The Cartesi Machine's I/O model has three concepts that look receipt-shaped but are not interchangeable. They map to **three different places** in the OP Stack, and getting the split right is what determines the withdrawal path.

| Cartesi concept | Nature | OP Stack / Ethereum counterpart |
|---|---|---|
| **Output — voucher** (`tx-output`; an executable call on L1) | provable, committed | **Withdrawal** (an L2ToL1MessagePasser message). Belongs in the *output root*, not in a receipt. |
| **Output — notice** (a provable statement) | provable, committed | Ethereum **log / event**. Belongs in the receipt *and* in the outputs commitment. |
| **Report** (`tx-report`) | explicitly **not** provable | Receipt **status, revert reason, debug payload**. Must never enter a commitment. |
| **Inspect** (`CmioRxRequestInspectState`) | read-only, no state change | **`eth_call`**. Not a receipt concern at all. |

In Cartesi Rollups, outputs accumulate into an **outputs Merkle root** — that root is what gets claimed on L1 and what `executeVoucher`/output validation proves against. In the OP Stack the structural analogue is the `messagePasserStorageRoot` inside

```
OutputRootV0 = keccak(version, stateRoot, messagePasserStorageRoot, blockHash)
```

which is exactly what `op-proposer` posts and what `OptimismPortal` checks withdrawals against.

### Why this forces Isthmus

op-node builds that output root in `L2Client.outputV0`, and it has two paths:

- **Pre-Isthmus:** `eth_getProof(L2ToL1MessagePasser, [], blockHash)`, then `proof.Verify(block.Root())` — an MPT proof of a specific account against the block's state root.
- **Isthmus:** reads `block.WithdrawalsRoot()` directly from the header.

**The pre-Isthmus path is unimplementable for a Cartesi execution layer.** It requires a genuine Ethereum MPT state trie containing an account at a fixed address, provable against the state root. Our state root is a Cartesi hash-tree root over the machine's address space; there is no MPT and no such account. Producing a proof that verifies is impossible, and faking one would be worse than not having it.

The Isthmus path, by contrast, is a plain 32-byte header field — which is precisely the right home for the Cartesi outputs Merkle root.

So **Isthmus is not merely "the next fork we haven't implemented"; it is the fork that makes `op-proposer` work for a non-EVM execution layer.** That reorders the roadmap: Isthmus support moves from "deferred" into the batcher/proposer milestone. Its cost is bounded and known:

- `engine_newPayloadV4` / `engine_getPayloadV4` (Isthmus is the fork that switches op-node to V4). Since the chain is Isthmus from genesis, these are the *only* payload methods worth serving; the V3 forms are dead code and are not implemented.
- Setting `RequestsHash = EmptyRequestsHash` in the header, because op-node's `CheckBlockHash` sets it whenever `WithdrawalsRoot != nil`; omitting it makes every block hash diverge.
- Keeping op-node's `fetchWithdrawalRootFromState` off, so it uses the header field rather than falling back to the proof path.

### What Isthmus does and does not buy (correction)

An earlier draft of this section claimed Isthmus lets us keep the stock portal with no proof-library fork. **That was wrong, and the distinction matters.** Isthmus settles the *producer* side of the output root, not the *consumer* side:

- **What it does buy, unambiguously:** op-node can compute an output root at all. Without it, `optimism_outputAtBlock` fails on the `eth_getProof` call and `op-proposer` cannot function — there is no chain to propose. It also makes the Cartesi outputs root a committed part of `OutputRootV0`, so anything that verifies against that root is verifying against the machine's real outputs. `op-node`, `op-batcher` and `op-proposer` all stay stock.

  **Confirmed against a released op-node** (v1.19.3), rather than argued from the source: `optimism_outputAtBlock` returns an output root for our chain, and the `withdrawalStorageRoot` it reports is byte-for-byte the value `cartesi_getOutputsRoot` gives for the same block. So the output root op-proposer would submit already commits to the Cartesi outputs tree, and nothing on that path calls `eth_getProof`.
- **What it does not buy:** L1-side withdrawal verification. `OptimismPortal.proveWithdrawalTransaction` verifies a withdrawal with an **MPT storage proof against `messagePasserStorageRoot`**. Feeding it a Cartesi outputs root does not make its `SecureMerkleTrie` verification succeed — the proof formats are unrelated. *(This half has since been closed from the other side: `withdrawalsRoot` is now a genuine storage trie the portal's verification succeeds against — see §7f.)*

So the L1 side still needs Cartesi-aware proof checking, and §4's two options remain live for that half of the problem:

1. Replace the portal's withdrawal-proof verification with Cartesi's own verification against the outputs root (a contract change, but a contained one — the surrounding portal logic, the game-type wiring and the proposer are untouched).
2. Route withdrawals through Cartesi's existing `Application.validateOutput` against the same outputs root, anchored to the resolved game, and use OP's portal only for deposits. Nearly free for applications already on the voucher path.

Both now verify against the *same* commitment that op-proposer already publishes, which is the real simplification Isthmus delivers: one outputs root, produced by stock OP plumbing, consumed by whichever verifier is chosen.

### Receipts are for users, not for the protocol

Nothing on the OP Stack's critical path reads L2 receipts: derivation fetches *L1* receipts (for deposits and system-config events), and `op-batcher` reads blocks and transactions. Receipts exist for wallets, explorers, indexers, and SDKs.

That grants freedom in how they are synthesized, subject to two hard constraints:

1. `receiptsRoot` and `logsBloom` are header fields. Committing them makes the receipt encoding **consensus-critical** — re-derived by every verifier and adjudicated in disputes. An encoding change then becomes a hard fork.
2. Nothing derived from **reports** may ever be committed. Reports are non-provable by construction and may reflect host-side state.

The consequence is a deliberate ordering: serve receipts *before* committing them. Keep `receiptsRoot` and the bloom empty while the receipt format is still moving, and only commit once it is stable — at a fork, on purpose.

### Staged plan

1. **Thread outputs through the chain.** *(done)* Per-transaction emissions are split into provable outputs and diagnostic reports, attributed by transaction index and hash, and recorded on the block. Outputs of a rejected input are dropped, because a rejection rolls the machine back; its reports are kept, since they are usually the only explanation of the failure. Builder and verifier are tested to record identical outputs — the agreement everything downstream depends on.
2. **Commit the outputs Merkle root** in the header's `withdrawalsRoot`, making `optimism_outputAtBlock` meaningful. *(done)* The chain is Isthmus from genesis and the fork schedule is not configurable — a pre-Isthmus chain could never be proposed, so supporting one would only add untested paths. The tree matches Cartesi's on-chain tree exactly — height 63 (`CanonicalMachine.LOG2_MAX_OUTPUTS`), leaves `keccak256(output)`, parents `keccak256(left‖right)`, unfilled positions padded with the zero-subtree chain — so existing voucher proofs and tooling verify against it unchanged. The accumulator is cumulative over the chain, not per block, which both models require: Cartesi indexes outputs globally, and a withdrawal must stay provable against the output root of any later block. Verifiers re-derive the root from re-execution and reject a payload that claims outputs the machine did not produce. *(Since §7f the header field carries the withdrawal trie root, with this outputs root at the trie's reserved slot — same commitment chain, one storage-proof hop longer.)*
3. **Synthesize receipts** from the recorded outputs. *(done)* Provable outputs become logs, acceptance becomes `status`, and consumed mcycles become `gasUsed`; `eth_getTransactionReceipt` and `eth_getBlockReceipts` serve the standard shape. Each log carries the output's **chain-wide index** as a topic alongside the raw bytes, which is precisely what a Cartesi output validity proof takes — so a receipt is enough to construct the L1 proof later. `receiptsRoot` and the header bloom stay empty, so none of this encoding is frozen into consensus while it is still moving.

   Reports deliberately do *not* become logs. Dressing a non-provable emission up as a log would imply it can be proven on L1. They are served instead through a `cartesi_` namespace — `cartesi_getTransactionEmissions` returns outputs with their indices plus the reports, and `cartesi_getOutputsRoot` returns the commitment and output count at a block.
4. **Map `inspect` to `eth_call`**. *(done)* A read-only CMIO inspect runs against a fork of the machine at the requested block, and the fork is discarded, so whatever the guest does while answering cannot touch the chain. `eth_call` concatenates the reports into its single return value; `cartesi_inspect` returns them individually, with the acceptance flag and cycle count.

Storage is the loose end: outputs are currently in memory and retained for as long as their block, which — like the block store itself — needs persistence and a retention policy before this runs for any length of time.

## 7b. Persistence: what to keep, and what already exists to keep it with

**Done.** A node restarts from its store instead of losing the chain: on a
devnet run of 39 blocks it came back from the checkpoint at block 30, replayed
nine, and was serving in about a second — against a real Cartesi Machine.

The state splits into three kinds, and they want three different answers.

**Blocks, the canonical chain, and head pointers — reuse.** An op-cartesi block
*is* a `types.Header` plus a transaction list, which is exactly what
go-ethereum's `core/rawdb` stores, so this is glue rather than design:
`ethdb/pebble` underneath, `rawdb` on top, both already in the dependency tree.
Canonical-hash rewriting also gives the unsafe-head reorgs op-node performs,
for free. The one thing rawdb has no key for is the safe head, which is an OP
notion rather than an Ethereum one.

**Outputs, the tree frontier, and per-transaction emissions — ours, but small.**
No OP component models these. The frontier is 63 hashes per block; outputs are
raw bytes keyed by their chain-wide index; receipts need not be stored at all,
since they are synthesized from the emissions. A handful of tables in the same
key-value store.

**Machine state — Cartesi's own `cm_store`.** The emulator exposes it as
`machine.store` over JSON-RPC, and the measurements shape the design:

| | |
|---|---|
| A stored machine | **532 MiB apparent, ~380 MiB on disk** |
| Time to store | **~1.8 s** |
| Storing from a fork | **works**, and the parent is unaffected |
| Reflink/dedup between checkpoints | not available; every checkpoint is a full copy |

Storing from a fork is what makes this cheap. The chain already forks a machine
server per block for snapshots, so a checkpoint is `machine.store` on a fork
that exists anyway — the live machine never stalls for the write.

One trap, and it is a silent one. `machine.store` takes a `sharing` parameter
its schema does not advertise, with three modes, and **only `all` stores the
machine as it is now**. Under `none` and `config` the call succeeds, writes a
plausible directory, and stores the state the machine was *loaded* at — so a
checkpoint taken after a thousand inputs reloads to the root from before the
first one. Nothing reports an error; the files simply describe the wrong
machine. `machine.Remote.Store` pins `all`, and `TestRemoteStoreCapturesLiveState`
fails if that ever changes. The mode's name suggests the stored copy would keep
being written to as execution continues, which would make it useless as a
checkpoint; measured, it does not — the directory is independent once written.

Half a gigabyte per checkpoint, with no deduplication available, rules out one
per block, so the shape is **checkpoint plus replay**: store every N blocks, and
on restart load the newest checkpoint and re-execute the persisted blocks after
it. Replay costs one block execution each — about 1.9 s in the devnet,
dominated by the guest rather than the emulator — so N bounds the worst-case
restart, and N = 100 puts it near three minutes.

The genuinely new code is the checkpoint-and-replay controller and the outputs
tables. Note there is no OP analogue for the controller and there could not be:
op-geth's state *is* its database, so the OP Stack has never needed a notion of
"snapshot the execution engine and replay forward". Cartesi's `cm_store` does
the hard half.

Replay is not a fast path around verification — it re-executes, and checks each
block reaches the state root and outputs commitment it was stored with. A
checkpoint that has drifted from the blocks beside it therefore fails the
restart rather than quietly serving a wrong chain.

Two consequences of restoring at a checkpoint rather than at genesis, both of
which showed up as bugs before they showed up as design. A restored node holds
no block below its checkpoint, so a forkchoice naming an older safe or
finalized block — which op-node does while its own view lags — has to be
answered from the store rather than rejected. And the store is held by one
process: pointing a second node at the same directory fails, so the devnet's
verifier gets its own.

Checkpoints are triggered by block count and by finalization both: finalized
blocks can never be reorged away, so a checkpoint at one never needs discarding,
but nothing finalizes on a devnet L1 and the count is what makes progress there.

## 7c. Withdrawals: executing Cartesi outputs against an OP proposal

**Done, end to end.** The guest emits a voucher, `op-proposer` proposes, and the
voucher executes on L1 — moving real ETH — with no change to `OptimismPortal`,
`op-node`, `op-batcher` or `op-proposer`.

![Deposit and withdrawal value paths across the bridge](diagrams/devnet-value-paths.svg)

*Deposits ride OP's own derivation pipeline into the guest's portal receiver;
withdrawals become vouchers whose tree root the header already carries, so a
stock proposal makes them provable and executable on L1.*

The bridge is one contract, and it is small for a structural reason. A Cartesi
`Application` asks exactly one question before executing an output:
`isOutputsMerkleRootValid(appContract, outputsMerkleRoot)`. An OP proposal
already commits to the answer, because op-node builds its root claim as

    keccak256(version ‖ stateRoot ‖ messagePasserStorageRoot ‖ blockHash)

and on this chain `messagePasserStorageRoot` is the header's `withdrawalsRoot`
— since §7f, the withdrawal trie, which holds the Cartesi outputs Merkle root
at a reserved slot. So `OPOutputsMerkleRootValidator` opens a game's root
claim — four words, one keccak, and one storage proof verified by the same
`SecureMerkleTrie` the portal runs — and records the outputs root it commits
to. That is the entire adaptation between the two settlement models.

Nothing forks `OptimismPortal` — and since §7f nothing needs to sidestep it
either: its withdrawal path verifies a storage slot against
`messagePasserStorageRoot`, and that root is now a real storage trie this
chain maintains, so ether withdrawals go through the portal itself. The
voucher path this section describes remains for what the portal cannot
execute — ERC-20 withdrawals and any other Cartesi output.

Two pieces on this side of the boundary:

- **Proofs.** The accumulator in `outputtree.go` carries only a frontier, so it
  cannot prove an old leaf. `ProveOutput` builds the co-path from the stored
  leaves and `cartesi_getOutputProof` serves it. Building the tree is the
  off-chain half: Cartesi 2.x shipped a builder on chain in `LibMerkle32`, 3.0
  dropped it and kept only the verifier, so a test on each side pins the two
  implementations to the same root. The proof is against the commitment of a
  *chosen* block, not the block that emitted the output: the tree is cumulative,
  so a withdrawal stays provable against every later proposal, and the caller
  wants whichever block was actually proposed.
- **Execution.** `OutputExecutor` verifies with Cartesi's own
  `LibOutputValidityProof` over `LibBinaryMerkleTree`, taken as a dependency
  rather than reimplemented, so a proof
  this node produces is checked by Cartesi's real verifier rather than by a
  reimplementation of it. It is a reduced stand-in for `Application` — no
  ownership, upgrades, token receivers or delegate-call vouchers — and a
  production chain should deploy the real one against the same validator.

**What is still assumed.** Proposals go into the permissioned game and nothing
can dispute them, so the validator's `requireDefenderWins` is false on the
devnet and its `maturityDelay` is zero. Those are constructor arguments rather
than hidden assumptions: a chain with a real proof system sets both, and the
same contract then waits for a resolved game past the dispute window. That
proof system is step 3, and it is now the only thing between this and a
trust-minimised withdrawal.

**And what is unbridged.** The executor pays vouchers from its own balance,
the way a Cartesi `Application` holds the assets it can be told to move. For
ether this asymmetry is resolved by §7f: `withdrawEther` now emits an OP
`Withdrawal` message and the portal pays it from the same lockbox the
deposits funded, so the two ends of the ether bridge share custody. For
ERC-20 the vouchers and the application-contract escrow remain, and remain
consistent with each other.

## 7d. ERC-20, and where the standard bridge stops working

The two directions come apart, and it is worth being precise about where.

**Deposits arrive intact.** `L1StandardBridge.depositERC20` escrows the tokens
on L1 and sends a cross-domain message, which reaches `OptimismPortal` as an
ordinary `TransactionDeposited` — so op-node derives it and hands it to the
machine like any other deposit. Observed on the devnet, deposit of 5 TST:

    to     0x4200…0007          (L2CrossDomainMessenger)
    from   0x199ed609…56393     (aliased L1CrossDomainMessenger)
    data   relayMessage(nonce, sender=L1StandardBridge, target=0x4200…0010,
                        value, minGasLimit,
             message = finalizeBridgeERC20(l2Token, l1Token, from, to, 5e18, ""))

Everything needed is there. What is missing is the two EVM predeploys that
would normally unwrap it — `L2CrossDomainMessenger` and `L2StandardBridge` —
so the guest decodes the two ABI layers itself instead of a predeploy doing it.
That is not a protocol change; it is guest code, and the same code an
application would write to accept any deposit.

The risk this creates is worth naming: the L1 escrow happens whether or not the
guest understands the message. A guest that ignores `finalizeBridgeERC20`
leaves real tokens locked in `L1StandardBridge` with nothing on L2 to show for
it. Deposits are unconditional; crediting them is not.

**Withdrawals cannot use the standard bridge at all.** The escrowed tokens sit
in `L1StandardBridge`, and only `L1CrossDomainMessenger` can make it call
`finalizeBridgeERC20` — after `OptimismPortal.proveWithdrawalTransaction`, which
is exactly the MPT-proof path this chain cannot satisfy (§7c). A Cartesi voucher
executes from the application's own context, so it cannot release that escrow.
At the time this section was written there was no arrangement of guest code
that fixed this: the custody was on the wrong side of a proof the chain could
not produce. §7f changes the premise — the proof is now producible — so the
messenger-shaped path (option 2 below) has become real future work rather
than a dead end; what stands unchanged is that the escrow stays stuck until
a guest speaks `relayMessage` byte-exactly.

So the honest options are two, and they are the same choice §4 posed, now
sharpened by ERC-20:

1. **Cartesi-style portals, and the application holds the assets.** An
   `ERC20Portal` pulls tokens into the application contract and tells the guest;
   withdrawal is a voucher calling `token.transfer(user, amount)` from that
   contract, which is what already works for ETH in §7c. This composes with
   everything built and forks nothing — but it means not using
   `L1StandardBridge`, and tokens bridged through the standard bridge stay
   stuck. Recommended.

2. **A messenger-shaped shim on L1** that relays `finalizeBridgeERC20` on proof
   of an accepted outputs root, so the standard bridge's escrow becomes
   releasable. This keeps the OP bridge's L1 surface and its token
   registry — worth something for tooling — at the cost of reimplementing the
   messenger's proof path, which is the thing §7c deliberately avoided doing to
   the portal.

Either way the L2 side is guest code, because there are no predeploys. The
choice is only about which L1 contract holds the assets.

### What is built: option 1

`contracts/src/portals/` is Cartesi's portal, one line different. Cartesi's own
`ERC20Portal` ends with `getInputBox().addInput(appContract, payload)`; ours
ends with `OptimismPortal.depositTransaction(appContract, 0, gasLimit, false,
payload)`. Everything else is unchanged — the escrow goes to the application
contract, and the payload is `InputEncoding`'s, so a guest written for Cartesi
Rollups parses it byte for byte.

That substitution works because the two transports carry the same two things: a
payload and an authenticated sender. Cartesi's `InputBox` stamps `msg.sender`
into the `EvmAdvance` envelope; `OptimismPortal` aliases a contract caller into
the deposit's `from`. So `alias(portal)` on this chain plays the part `portal`
plays there, and a guest that trusts a portal address on one trusts an aliased
one on the other.

Which leaves one question the ETH path never had to answer: **how does the guest
learn the portal addresses?** They do not exist when the snapshot is built —
the machine's root hash is the L2 genesis state, which the rollup config
commits to, which L1 is deployed against. Naming a portal in genesis would
require deploying it first, and deploying it requires the chain. The circle has
to be cut somewhere:

- **Bake in the addresses.** Requires deploying L1 before the snapshot, which
  inverts the dependency rather than removing it, and makes genesis specific to
  one L1 deployment.
- **Trust any sender.** Fatal. Any contract could call `depositTransaction` with
  bytes shaped like an ERC-20 deposit and mint claims against tokens the
  application actually holds; a voucher would then pay them out. The portal
  address *is* the authentication.
- **Bake in an owner, and let it register the portals as an input.** An address
  is not a deployment artifact — it can be chosen before anything exists. The
  guest carries one, and takes configuration from nothing else.

The third is what the devnet guest does (today the routed `demo`, and
its Lua predecessor `bank-app.sh` before it). The owner address is baked into
the snapshot — a Dockerfile build argument, covered by the genesis state root
like every other consensus parameter — and registration arrives as an
ordinary deposit whose `from` is the owner — unaliased, because
`OptimismPortal` only aliases contract callers. The registration is answered
with a notice rather than a report: which contracts the guest will credit is
consensus state, so it belongs in the outputs tree where it can be proven.

The ledger is keyed by `token ‖ account`, with the zero address for ether, so
one table serves both assets, and a withdrawal is the same voucher either way —
`transfer(to, amount)` on the token, or a plain value call. Nothing downstream
of the guest distinguishes them, because to the outputs tree a voucher is a
voucher.

One asymmetry survives, and it is worth stating plainly rather than hiding in
the ledger: ether deposited through `OptimismPortal` and ether deposited through
`OPEtherPortal` are credited the same, but only the second is held anywhere a
voucher can reach. The first sits in OP's lockbox. The devnet papers over this
by funding the application contract directly; a real chain would either use the
Cartesi portal for both directions or accept that OP-path ether is one-way.

## 7e. Settlement: what reading Dave actually costs

§4 sketched Plan A from the outside — "write a `CartesiDisputeGame` implementing
`IDisputeGame`, which internally is a Dave/PRT tournament". Having now read
`cartesi/dave` rather than reasoned about it, that sentence is true and badly
misleading about the size.

The one thing §4 got right is the warning it buried at the end: *disputes must
cover derivation, not just execution*. That turns out to be the largest item of
all, and the rest of this section is mostly about why. Below: what the claim
actually is, three requirements this chain does not meet, two integration shapes,
and the decision that gates all of them.

### The claim is a machine-state commitment, not an output root

`prt/contracts/src/arbitration-config/ArbitrationConstants.sol` defines a
three-level tournament: `log2step` `[44, 27, 0]`, `height` `[48, 17, 27]`. Each
level's commitment is a Merkle tree over `cm_get_root_hash()` sampled every
`2^log2step` cycles, and a level refines its parent's stride until the leaf is a
single micro-instruction replayed on chain by `machine-solidity-step`.

Producing those commitments is the off-chain "dance". `MachineCommitmentBuilder`
(`prt/client-rs/core/src/machine/commitment_builder.rs`) takes a base cycle, a
level and a stride, reconstructs the machine at that cycle
(`MachineInstance::new_rollups_advanced_until`), steps it, and hashes at every
stride boundary; the leaves are cached in SQLite because recomputing them per
move is not affordable. `strategy/player.rs` then has to respond to opponents
inside per-match clocks (`react_match`, `win_timeout_match`).

That is a validator daemon with its own state, not a library op-cartesi links.
Dave ships one for Cartesi Rollups —
`cartesi-rollups/node/{blockchain-reader,epoch-manager,machine-runner,state-manager}`
— and an OP-shaped chain would need the equivalent, wired to a different source
of inputs and a different notion of epoch.

### Three requirements this chain does not currently meet

**1. A referee has to be able to check an input, on chain.** `IDataProvider`
exposes exactly one method, `provideMerkleRootOfInput(index, input)`, and
`DaveConsensus` implements it by hashing the input and comparing against
`InputBox.getInputHash(app, index)`. Our inputs exist in no such contract: they
are op-node's derivation output — compressed channel frames in the BatchInbox,
plus deposits from L1 logs — wrapped in an `EvmAdvance` envelope built by our Go
code. No Solidity contract can re-derive that. This is §4's "subtle hard part",
and it is the largest single item.

**2. The outputs Merkle root has to live inside the machine.**
`DaveConsensus._validateOutputTree` proves that
`keccak256(abi.encode(outputsMerkleRoot))` sits at
`PMA_CMIO_TX_BUFFER_START` in the machine's memory tree, against the final
machine state hash the tournament resolved; the node reads it back with
`read_memory(TX_START, 32)`. Our outputs tree is maintained by the shim, in Go,
and published in the header's `withdrawalsRoot`. A referee cannot dispute a
value that is not in the state, so under Dave the *guest* has to maintain the
tree and leave its root in the tx buffer. The header field can keep carrying it;
what changes is who computes it and what commits to it.

**3. Dave already defines the computation, and our definition contradicts it.**
`prt/client-rs/core/src/machine/constants.rs`: `LOG2_UARCH_SPAN_TO_BARCH = 20`,
`LOG2_BARCH_SPAN_TO_INPUT = 48`, `LOG2_INPUT_SPAN_TO_EPOCH = 24`. Every input
occupies a fixed `2^68` meta-cycle span indexed as
`(input index, big-arch cycle, uarch cycle)`, and once the machine yields the
state is a fixpoint — which is why `provideMerkleRootOfInput` returns zero past
the end of the epoch rather than reverting. This chain's rule is
`MaxCyclesPerInput` (default `10^9`, about `2^30`), with *exceeding the budget
counted as a rejection*. Those are different state transition functions, and the
divergence is not cosmetic: a bounded budget with a rejection branch is a
different function from an unbounded span with a fixpoint.

So the roadmap item is not "write down the rule we already implement". It is
"decide whether to adopt Dave's meta-cycle model", and if so, change ours.

### Two integration shapes, and the cheaper one is not the obvious one

`DaveConsensus` already implements `IOutputsMerkleRootValidator` — the exact
interface `OutputExecutor` calls today (§7c). That opens a shape §4 did not
consider:

- **Dave as the validator.** Point `OutputExecutor` at a `DaveConsensus` instead
  of at `OPOutputsMerkleRootValidator`. OP's `DisputeGameFactory` stays
  permissioned and governs only OP's own (unused) withdrawal path, while Cartesi
  outputs settle under Dave. On the L1 side this is nearly a no-op — the
  interface is already the one we call.
- **Dave as an `IDisputeGame`.** Everything above, plus an adapter reconciling
  tournament semantics with OP's game semantics (clock, bonds, `resolve`, airgap)
  and Dave's L1-block-range epochs with OP's per-block `l2BlockNumber`, plus
  binding the resolved machine-state hash to the `stateRoot` inside op-node's
  output-root preimage.

Neither escapes requirements 1–3. The difference between them is a contract
adapter; the difference between *having* and *not having* a fault proof is
requirements 1–3.

### The real decision: input availability

§4 posed this as v1 calldata-anchored / v2 blob-anchored. Reading the code
sharpens it into three options with different things being given up:

1. **Mirror inputs into an InputBox-shaped contract.** Every input is hashed on
   L1, so `provideMerkleRootOfInput` works unchanged and Dave applies almost
   as-is. Costs a second copy of the data on L1 — the batcher's compressed
   channel stops being the disputable artifact and becomes an optimisation for
   fast sync only. Simplest, most expensive.
2. **Prove derivation too.** Put the derivation pipeline inside the machine, the
   way OP puts op-program inside Cannon, so the disputed computation starts from
   L1 data rather than from an input list. Keeps DA compressed and is the only
   option that makes the sequencer unable to lie about inputs at all. Much the
   largest, and it makes brotli and the OP batch format part of the guest.
3. **Commit to the input sequence and trust the committer.** Cheapest, and it
   gives back exactly the property the fault proof was for. Worth naming only so
   it is rejected explicitly rather than by accident.

There is no fourth option where compressed batches stay the sole DA and a
Solidity referee still checks inputs: checking would mean decompressing on
chain, which reduces to (1) with extra steps.

### What this means for sequencing

Requirements 1–3 are shared by **both** settlement tracks. A RISC Zero proof of
the same computation still needs a computation whose inputs are pinned to L1 and
whose outputs root is inside the proven state; it removes the tournament, the
clocks, the bonds and the commitment builder, not the definition problem.

So the next step is not PRT. It is to settle the definition — meta-cycle model
in or out, input availability option 1 or 2 — and to get the RISC Zero
cost-per-block number, which is information needed either way and commits to
nothing.

*Checked against `cartesi/dave` at HEAD: `prt/contracts/src/`,
`prt/client-rs/core/src/`, `cartesi-rollups/contracts/src/DaveConsensus.sol`,
`cartesi-rollups/node/`. Note that Dave pins `cartesi-rollups-contracts` 2.2.0
while this repo is on 3.0.0-alpha.6; an integration has to reconcile them.*

## 7f. The withdrawal trie: OP-native withdrawals without forking the portal

§7 established that Isthmus settles the *producer* side of the output root and
left the *consumer* side — `OptimismPortal.proveWithdrawalTransaction`'s MPT
storage proof — as the thing this chain could not satisfy, and §7c/§7d built
the Cartesi-voucher bridge around that hole. This section closes the hole, and
the observation that closes it is worth stating precisely, because an earlier
draft of §7 over-claimed the impossibility:

**The portal never proves the account. It proves one storage slot of one
contract, against a root the header already carries.** `eth_getProof`'s
account proof — the MPT path from the state root to the `L2ToL1MessagePasser`
account — is consumed only by op-node's *pre-Isthmus* output-root path, which
this chain does not run. From Isthmus, `messagePasserStorageRoot` is the
header's `withdrawalsRoot`, taken on faith by op-node and committed by the
proposal; the portal then verifies withdrawals *against that storage root
alone*, with `SecureMerkleTrie.verifyInclusionProof(abi.encode(slot), 0x01,
proof, messagePasserStorageRoot)` where `slot =
keccak256(abi.encode(withdrawalHash, 0))`. viem's `buildProveWithdrawal` reads
only `proof.storageHash` and `proof.storageProof[0].proof` — the account proof
is never touched. So what "there is no Ethereum MPT" actually rules out is the
*account trie*; a single insert-only *storage trie*, maintained outside any
EVM, is not ruled out by anything. It is, in fact, the easiest possible MPT
workload: keys uniformly distributed by keccak, one constant value (`0x01`),
no deletions, no updates.

So the chain now maintains exactly that trie, and `withdrawalsRoot` is its
root:

- **The guest emits withdrawals.** `withdrawEther` no longer emits a voucher;
  it emits a `Withdrawal(uint256 nonce, address sender, address target,
  uint256 value, uint256 gasLimit, bytes data)` message — the OP
  `WithdrawalTransaction` fields, riding as a Notice because the rollup
  device has no raw output. A notice deliberately: a voucher is executable by
  the Cartesi output executor, and a message the portal can finalize must not
  have a second executor to be paid by. The nonce is
  `encodeVersionedNonce`-shaped (version 1 in the top 16 bits) and derived
  from the chain-wide input index plus a per-input ordinal, which makes it
  unique with no stored counter — nothing on L1 requires the counter to be
  dense, only the withdrawal hash to be unique.
- **The host maintains the trie** (`chain/passertrie.go`): a genuine
  Ethereum storage trie over geth's own `trie` package, secure-keyed the way
  geth keys account storage, holding the `sentMessages` slot of every
  withdrawal hash. It is cumulative like the outputs tree, carried per block
  across forks with copy-on-write, rebuilt from persisted block outputs on
  restart, and re-derived by verifiers — a payload claiming a withdrawal the
  machine did not emit fails `engine_newPayloadV4` with a withdrawals-root
  mismatch.
- **The Cartesi outputs root moves into the trie** rather than out of the
  header: the reserved slot `keccak256("op-cartesi.outputsMerkleRoot")` holds
  the outputs tree root as of each block. One storage proof against
  `withdrawalsRoot` opens it, so vouchers and notices lose nothing — their
  commitment is still under the root claim, one keccak-verified hop further
  down. The slot cannot collide with a `sentMessages` slot short of a keccak
  collision: those hash a 64-byte mapping preimage, this hashes a short
  string.
- **`eth_getProof` exists now**, for exactly one address:
  `0x4200…0016`. It serves the storage proof viem asks for, with
  `storageHash = withdrawalsRoot` and an empty account proof — there is
  still no account trie, and nothing on the Isthmus path wants one. Every
  other address is refused with a pointer to `cartesi_getAccountProof`.
- **`OPOutputsMerkleRootValidator` gains one step**: after opening the root
  claim's preimage it verifies the outputs-root slot with the *vendored*
  `SecureMerkleTrie` — the same code the portal runs, against the same root,
  so the two consumers of the trie cannot drift apart.

What this buys, concretely:

1. **Ether withdrawals through the stock portal.** `proveWithdrawalTransaction`
   and `finalizeWithdrawalTransaction` work unmodified, and viem's standard
   withdrawal actions drive them. The custody split §7c ended on — deposits
   in OP's lockbox, voucher payments from the executor's own balance —
   dissolves for ether: the lockbox that takes the deposit pays the
   withdrawal.
2. **A path to the standard bridge.** §7d's "there is no arrangement of guest
   code that fixes this" was true only while the proof was unproducible. With
   the trie in place, a guest that emits withdrawals with `sender =
   L2CrossDomainMessenger` and `data = relayMessage(...)` (nonce and
   `baseGas` computed as the messenger does) would make
   `L1CrossDomainMessenger` — and therefore `L1StandardBridge`, both
   directions — work against this chain. That is future guest work, not a
   protocol change, and it is byte-exactness work: the messenger's encoding
   becomes consensus the guest must reproduce.
3. **A cleaner dispute posture.** The bespoke validator stops being the only
   bridge; the portal's ordinary respected-game-type machinery governs
   withdrawals, so when a Cartesi dispute game lands as an `IDisputeGame`,
   withdrawals inherit it with no bridge changes.

What it costs, honestly:

- **OP predeploy semantics enter consensus.** `hashWithdrawal` and the
  storage layout of `sentMessages` are now things the host and guest must
  reproduce byte-exactly, forever. Today that surface is small — one struct
  hash, one mapping slot — and it is pinned by cross-tests in three
  directions: Go against the guest's TypeScript encoding
  (`chain/withdrawal_test.go`), Go against geth's verifier
  (`chain/passertrie_test.go`), and Go against the vendored Solidity
  verifier (`contracts/test/PasserTrieVectors.t.sol`, mirrored by
  `TestPasserTrieMatchesSolidityVectors`). Rung 2 above would grow it to the
  messenger's encoding; that is the price of admission and should be paid
  deliberately.
- **ERC-20 withdrawals stay on the voucher path.** The tokens are escrowed
  in the application contract, which only a voucher executing from that
  contract's context can move; a portal-finalized call comes *from the
  portal*. Moving tokens to the standard-bridge escrow is rung 2's decision,
  not this one's.
- **The account proof stays impossible.** Anything that verifies the
  `0x4200…0016` *account* against `stateRoot` — a third-party prover
  service, a pre-Isthmus consumer — still breaks. viem and the portal do
  not.
- **The ether invariant becomes real.** The portal pays from the lockbox, so
  the guest must never let more ether out than went in through
  `depositTransaction`. On the devnet both paths exist (`OPEtherPortal`
  escrows at the executor; the portal escrows in the lockbox); a real chain
  should pick the portal path for ether and retire `OPEtherPortal` to
  ERC-20-style uses or nothing.

**Verification status.** The trie, the guest emission, the chain wiring,
`eth_getProof`, and the validator's storage-proof step are covered by the Go,
TypeScript and Foundry suites, including proofs produced by the Go trie and
verified by the portal's own `SecureMerkleTrie` with the portal's exact value
check. The end-to-end devnet run — `scripts/withdraw.ts`, now written against
the portal (prove → mature → resolve the permissioned game → finalize) — has
not yet been exercised against a live devnet, and the finalization leg
depends on the deployed portal's game-validity checks; that is the first
thing to run.

## 8. The app-chain dimension: one machine, many applications

A Cartesi-machine L2 is natively an **app-chain**: the chain's state transition function is whatever the guest program does, which puts it closer to a Cosmos appchain (Monomer's world) than to a general smart-contract L2. That's the point — full Linux, any language, no gas-VM straitjacket. But "one machine = one application" is a configuration choice, not an architectural constraint. Because the guest is a Linux system and the block boundary is just a sequence of CMIO inputs, there is a clean spectrum of ways to host multiple applications on one chain and to add new ones over time — with the crucial property that **none of them change the proving story**. Dave, Asterisc, or RISC Zero prove *the machine*, not any particular application; whatever the guest does, including loading new code, is automatically covered by the same root-hash commitments. Extending the chain is a guest-software problem, not a protocol problem.

**Level 0 — static multi-app with input routing.** Design the input envelope from day one as `(app_id, payload)`, mirroring how Cartesi Rollups addresses inputs to an application address via the InputBox. Inside the guest, a small supervisor process dispatches each input to the application registered under that id (separate processes or dynamically linked handlers), with per-app state namespaced in the filesystem and outputs/vouchers tagged with the originating app. Cross-app calls are ordinary IPC or function calls — meaning applications on the same machine get **synchronous composability**, like contracts on one chain, which is something the "one rollup per app" Cartesi model doesn't give you. Adding an application at this level means shipping a new machine template: a chain upgrade that changes the genesis/absolute-prestate commitment, governed exactly like OP prestate upgrades or Nitro's `wasmModuleRoot` upgrades. Simple and safe, but not dynamic.

**Level 1 — in-band dynamic deployment (code as a transaction).** Since the guest is Linux, "deploy an application" can literally be a transaction type. An input addressed to the supervisor carries (or commits to) an executable artifact — a static ELF binary, or a squashfs bundle — plus metadata (app id, resource limits, deposit/fee). The supervisor validates it, writes it to the merklized filesystem, registers it, and from the next input onward the app is live. No chain upgrade, no L1 action, no new contracts: the artifact traveled through the same batcher/DA path as any input, derivation pins it to L1, and disputes replay it like everything else. Practical constraints are DA-shaped, not proof-shaped: artifact size costs calldata/blob space, so v1 should cap artifact size and require the full bytes in-band (a hash-only deploy with out-of-band data fetch is possible later, but it drags in preimage-oracle semantics for the dispute game — defer it). A useful middle ground is preloading heavy shared runtimes (libc, interpreters, frameworks) in the base template so deployed artifacts stay small.

**Level 2 — embedded runtimes (a platform inside the app-chain).** Raw native binaries from third parties imply trusting them with syscall access, and determinism discipline (no wall-clock, no host randomness, entropy only derived from inputs) becomes each developer's problem. If the goal is *permissionless* extension, run deployed code under a constrained runtime inside the guest: a Wasm runtime, a deterministic interpreter (Lua/JS/Python), or even an EVM interpreter compiled for riscv64 — at which point the app-chain contains a general smart-contract platform as one of its applications, and "deployment" is just data, Stylus-style but under your rules. The supervisor enforces sandboxing (user separation, seccomp, no network) and determinism at the boundary. This is a product decision more than an engineering one; the architecture supports it whenever wanted.

**Level 3 — many machines, one chain (the horizontal alternative).** Instead of multiplexing inside one machine, the engine shim could manage N machines and commit the block state root to a Merkle tree over per-machine root hashes — effectively "Cartesi Rollups' one-machine-per-app model, but sharing one chain's sequencing, DA, bridge, and settlement." It buys hard isolation and parallel execution, but it violates the minimal-code principle: the two-level state commitment leaks into the dispute contracts (a challenge must first bisect to *which machine* diverged), the shim grows real orchestration logic, and cross-app calls degrade to async messaging. Not recommended for v1; worth revisiting only if single-machine throughput or isolation becomes the binding constraint.

**Multi-tenancy needs metering.** The moment apps are multiple (and especially if deployment is permissionless), one app must not be able to starve the chain. The machine gives you the natural gas unit for free: **mcycles**. The shim runs each input with a bounded `cm_run(mcycle_end)` budget; the supervisor charges fees in-guest per cycles consumed (mcycle delta is part of machine state, so fee accounting is provable like everything else) and per bytes of merklized storage occupied (storage rent). An input that exhausts its budget is deterministically treated as reverted. This cycle-metering design also directly serves Plan B2, where cycles are literally the proving cost driver.

**Recommendation:** build Level 0's input envelope and supervisor dispatch from the start even if the chain launches with a single application — it costs almost nothing and keeps every later level additive. Add Level 1 when a second team wants in without a coordinated upgrade. Treat Levels 2–3 as roadmap options, not prerequisites.

## Key references
- Cartesi machine emulator (code analyzed): https://github.com/cartesi/machine-emulator — `src/cm.h` (C API), `uarch/`, `risc0/` (ZK pipeline)
- OP Stack specs — rollup node & Engine API: https://specs.optimism.io/protocol/rollup-node.html · https://specs.optimism.io/protocol/exec-engine.html
- op-node README (CL/EL split): https://github.com/ethereum-optimism/optimism/blob/develop/op-node/README.md
- Monomer (non-EVM engine behind op-node; reference for the shim): https://github.com/polymerdao/monomer
- Asterisc (RISC-V FPVM, registered OP game type): https://github.com/ethereum-optimism/asterisc
- Kailua (ZK hybrid dispute game on OP Stack): https://github.com/risc0/kailua
- Arbitrum Nitro STF / ArbOS / WASM module root: https://docs.arbitrum.io/how-arbitrum-works/inside-arbitrum-nitro · https://docs.arbitrum.io/launch-arbitrum-chain/protocol-hacks/stf
- Cartesi fraud proofs: https://github.com/cartesi/dave · https://github.com/cartesi/machine-solidity-step
