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

One design decision to make early: **input granularity**. Either (a) one CMIO input per transaction (simple, matches Cartesi Rollups today, coarse dispute granularity at the input level), or (b) one CMIO input per block containing the ordered tx list (fewer yields, block-atomic). (a) composes better with existing Cartesi tooling and with Plan A's dispute story.

---

## 4. Plan A — OP Stack chassis + Cartesi-native settlement (Dave/PRT + `machine-solidity-step`)

**Thesis:** reuse OP for everything off-chain and for L1 plumbing (portal, factory, batcher, proposer, bridges), but settle disputes with the fraud-proof machinery Cartesi already built for exactly this VM — the Dave/PRT tournament contracts bisecting to a uarch step replayed by `machine-solidity-step`, or (cheaper leaf) a single Groth16 seal from the in-repo RISC Zero prover.

**How it fits together:**

- `op-proposer` posts output roots by creating games via `DisputeGameFactory.create(CARTESI_GAME_TYPE, claim, extraData)`. The claim is (a commitment to) the machine root hash at the epoch boundary.
- You write a `CartesiDisputeGame` contract implementing OP's `IDisputeGame` interface, which internally *is* a Dave/PRT tournament (or delegates to one). The one-step leaf is either `machine-solidity-step` uarch replay (pure Solidity, exists today) or a `cm_log_step` → RISC Zero receipt → Groth16 verification against the on-chain image ID (also exists today, in `risc0/solidity/`). The second option makes the on-chain leaf a single ~300k-gas verification instead of a uarch replay, and shrinks the tournament depth because one proof can cover a large `mcycle_count`.
- `OptimismPortal` is configured with your game type as the respected game; withdrawals then flow through the standard `proveWithdrawalTransaction` path — except withdrawal proofs are Merkle proofs **into the machine's hash tree** (via `cm_get_proof`) rather than MPT storage proofs. This needs a small portal/`CartesiOutputVerifier` adaptation: either fork `OptimismPortal`'s proof-verification library to verify Cartesi hash-tree proofs against a designated "outputs" memory range, or — lower-effort and closer to what you run today — keep OP's portal for ETH/token deposits only and route withdrawals through Cartesi Rollups' existing voucher/`executeVoucher` flow anchored to the accepted game outcome. For applications already built on the voucher path, the second option is nearly free.

  **Refined by §7:** both options stay on the table, but they now verify against a commitment the stock OP plumbing already publishes. From Isthmus onward, op-node reads the withdrawal commitment straight out of the header's `withdrawalsRoot` field instead of proving it against the state trie, so the Cartesi outputs Merkle root goes there directly and `op-proposer` needs no changes. What Isthmus does *not* do is teach `OptimismPortal` to verify Cartesi proofs — that still requires option 1 or option 2. See §7 for both, and for why the pre-Isthmus path cannot be implemented at all.

**The subtle hard part: disputes must cover derivation, not just execution.** In stock OP, the fault-proof program (op-program/Kona) re-derives the disputed block *from L1 data* inside the FPVM, so a malicious sequencer can't win by lying about inputs. Your equivalent: the dispute must pin the machine's input sequence to L1. Two ways, in increasing order of reuse-of-what-you-know:

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
1. Shim MVP against a local machine + op-node in sequencer mode on an L1 devnet (no proofs; permissioned/`FAST` game type). Milestone: deposits credited in-guest, blocks derived identically by a second verifier node from L1 data alone.
2. Batcher/proposer integration; snapshot-based reorg handling; guest-side deposit decoder. Includes Isthmus support and committing the outputs Merkle root in `withdrawalsRoot`, which is what makes `op-proposer` produce a meaningful output root at all — see §7.
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

- `engine_newPayloadV4` / `engine_getPayloadV4` (Isthmus is the fork that switches op-node to V4).
- Setting `RequestsHash = EmptyRequestsHash` in the header, because op-node's `CheckBlockHash` sets it whenever `WithdrawalsRoot != nil`; omitting it makes every block hash diverge.
- Keeping op-node's `fetchWithdrawalRootFromState` off, so it uses the header field rather than falling back to the proof path.

### What Isthmus does and does not buy (correction)

An earlier draft of this section claimed Isthmus lets us keep the stock portal with no proof-library fork. **That was wrong, and the distinction matters.** Isthmus settles the *producer* side of the output root, not the *consumer* side:

- **What it does buy, unambiguously:** op-node can compute an output root at all. Without it, `optimism_outputAtBlock` fails on the `eth_getProof` call and `op-proposer` cannot function — there is no chain to propose. It also makes the Cartesi outputs root a committed part of `OutputRootV0`, so anything that verifies against that root is verifying against the machine's real outputs. `op-node`, `op-batcher` and `op-proposer` all stay stock.
- **What it does not buy:** L1-side withdrawal verification. `OptimismPortal.proveWithdrawalTransaction` verifies a withdrawal with an **MPT storage proof against `messagePasserStorageRoot`**. Feeding it a Cartesi `LibMerkle32` root does not make its `SecureMerkleTrie` verification succeed — the proof formats are unrelated.

So the L1 side still needs Cartesi-aware proof checking, and §4's two options remain live for that half of the problem:

1. Replace the portal's withdrawal-proof verification with `LibMerkle32` verification against the outputs root (a contract change, but a contained one — the surrounding portal logic, the game-type wiring and the proposer are untouched).
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
2. **Commit the outputs Merkle root** in the header's `withdrawalsRoot` under Isthmus, making `optimism_outputAtBlock` meaningful. *(done)* The tree matches Cartesi's on-chain tree exactly — height 63 (`CanonicalMachine.LOG2_MAX_OUTPUTS`), leaves `keccak256(output)`, parents `keccak256(left‖right)`, unfilled positions padded with the zero-subtree chain, as `LibMerkle32` does — so existing voucher proofs and tooling verify against it unchanged. The accumulator is cumulative over the chain, not per block, which both models require: Cartesi indexes outputs globally, and a withdrawal must stay provable against the output root of any later block. Verifiers re-derive the root from re-execution and reject a payload that claims outputs the machine did not produce.
3. **Synthesize receipts** from the recorded outputs: notices → logs, accepted/rejected → status, mcycles → `gasUsed`, reports → failure detail. Served from `eth_getTransactionReceipt` with `receiptsRoot` still empty.
4. **Map `inspect` to `eth_call`**: a read-only CMIO inspect run against a fork of the head machine, never mutating state, never entering a block.

Storage is the loose end: outputs are currently in memory and retained for as long as their block, which — like the block store itself — needs persistence and a retention policy before this runs for any length of time.

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
