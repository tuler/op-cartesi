# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the Engine API and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

## Status

**Roadmap steps 1 to 3 are done.** On a live devnet the chain sequences, batches to L1, is re-derived block-for-block by an independent verifier, proposes to a `DisputeGameFactory`, survives a restart, and settles withdrawals of both ether and ERC-20 through Cartesi vouchers proven against those proposals. What is *not* done is disputing a proposal — that, and the gaps that have nothing to do with proofs, are in the [roadmap](#roadmap). The shim implements the full sequencing loop — `engine_forkchoiceUpdatedV3` (with OP payload attributes) → `engine_getPayloadV4` → `engine_newPayloadV4` — plus verifier-side re-execution, reorg handling via machine snapshots, JWT auth, and the `eth_*` subset op-node and op-batcher read. It generates the `rollup.json` op-node consumes, and runs every fork through Isthmus from genesis (see [Fork support](#fork-support)).

Compatibility is verified against **op-node's own types** rather than hand-written JSON: the [`integration`](integration/) suite drives the shim over authenticated HTTP using `op-service/eth`, and checks each block with op-node's `ExecutionPayloadEnvelope.CheckBlockHash`, which independently reconstructs the header. A deliberate one-field mutation to header construction is caught there, so the check has teeth.

The chain also builds blocks on a **real Cartesi Machine**: the JSON-RPC client is pinned to machine-emulator 0.21.0 by probing a running server, and `chain` and `machine` carry tests that load a real machine, build blocks on it, re-execute them as a verifier, and check that the outputs commitment the host computes is byte-identical to the one the guest maintains. They are skipped unless a snapshot is supplied:

```sh
./scripts/build-snapshot.ts
OP_CARTESI_TEST_SNAPSHOT=./demo/.cartesi/image \
OP_CARTESI_TEST_LEDGER_SNAPSHOT=./demo/.cartesi/image go test ./...
```

The second variable turns on the deposit and token tests, which need a guest that means something by its inputs rather than merely consuming them — the routed guest of [`demo`](demo/README.md) (docs/EVM-COMPAT.md), which is what the snapshot script builds.

**The chain round-trips through L1.** `./devnet/start-devnet.ts` brings up anvil as L1, a Cartesi Machine, op-cartesi, and op-node in sequencer mode; op-node sequences L2 blocks continuously, each carrying the L1-attributes deposit it injects, wrapped in the `EvmAdvance` envelope and executed by the machine. The block's state root is the machine's Merkle root and its `withdrawalsRoot` is the outputs commitment. Each piece runs in its own [mprocs](https://github.com/pvolok/mprocs) pane — including the machine's console and the guest's own per-transaction reports — so the whole stack is one screen you can watch, stop and restart a piece at a time. See [devnet/README.md](devnet/README.md).

`op-batcher` posts those blocks to L1 as calldata batches, which advances the safe head. A **second node** then runs alongside — its own machine, engine and op-node, sequencing nothing — and rebuilds the chain purely from what the batcher put on L1. It reaches byte-identical blocks: same hash, same machine root, same outputs commitment. That is the property that makes this a rollup rather than a database with an RPC.

The stack has been run against the **official released images** — op-node v1.19.3 and op-batcher v1.16.11 — as well as against locally built binaries. The OP monorepo ships no binaries of its own, so `./devnet/start-devnet.ts` falls back to docker when they are not on your `PATH`; nothing needs compiling but op-cartesi itself.

**Deposits reach the guest.** `bun scripts/deposit.ts <address> <wei>` calls `OptimismPortal.depositTransaction` on L1; op-node derives it into an L2 deposit transaction, and the guest — the routed [`demo`](demo/README.md) — credits the recipient on its accounts drive. The balance is machine state, so the state root commits to it, and `eth_getBalance` reads it straight out of machine memory. The verifier, deriving from L1 alone, arrives at the same balance.

**Proposals land on L1.** The devnet deploys the full OP Stack L1 suite with `op-deployer` and runs `op-proposer` against the `DisputeGameFactory`. The claim it submits is `keccak(0³² ‖ stateRoot ‖ withdrawalsRoot ‖ blockHash)` — recomputed independently and matched against the on-chain game — so the root claim on L1 commits to the machine's Merkle root *and* the Cartesi outputs tree. Deposits go through the real `OptimismPortal.depositTransaction`.

**Withdrawals work, through Cartesi vouchers.** `bun scripts/withdraw.ts <address> <wei>` asks the guest to withdraw; it emits a Cartesi `Voucher`, the voucher enters the outputs tree, `op-proposer` proposes the block, and an L1 contract proves the voucher against that proposal and executes it — moving real ETH. Verified end to end on the devnet, including that a voucher is single-use.

The bridge is one small contract. A Cartesi `Application` asks one question before executing an output — `isOutputsMerkleRootValid` — and an OP proposal already commits to the answer, since op-node's root claim is `keccak(version ‖ stateRoot ‖ withdrawalsRoot ‖ blockHash)` and `withdrawalsRoot` *is* the Cartesi outputs root. [`OPOutputsMerkleRootValidator`](contracts/src/OPOutputsMerkleRootValidator.sol) opens that preimage on chain; verification uses Cartesi's own `LibOutputValidityProof`, pulled in as a dependency rather than reimplemented. Nothing forks `OptimismPortal` — its withdrawal path wants an MPT proof this chain cannot produce, so this sidesteps it rather than replacing it.

**Tokens bridge through Cartesi portals, not the standard bridge.** [`OPERC20Portal`](contracts/src/portals/OPERC20Portal.sol) is Cartesi's `ERC20Portal` with one line changed: where Cartesi calls `inputBox.addInput`, it calls `OptimismPortal.depositTransaction`. The escrow still goes to the application contract, and the payload is still `InputEncoding`'s, so a guest written for Cartesi Rollups parses it byte for byte — and a withdrawal is a voucher calling `token.transfer(user, amount)` from the contract holding the tokens. `L1StandardBridge` is deliberately not used: it escrows in itself and releases only against the MPT proof this chain cannot produce, so tokens sent through it would be stuck. See [DESIGN.md §7d](docs/DESIGN.md).

Run end to end on the devnet: 1 TST deposited through the portal is credited in-guest, and the verifier — deriving from L1 alone — agrees on the balance; 0.5 TST is then withdrawn by proving the voucher against an `op-proposer` proposal, and the tokens move on L1. The same round trip works for ether through [`OPEtherPortal`](contracts/src/portals/OPEtherPortal.sol), which escrows in the application rather than OP's lockbox, so the deposit and the withdrawal agree about who holds the money. A forged deposit payload from a sender that is not a registered portal credits nothing.

The guest authenticates a portal by its address, which raises the one question the ETH path never had: the portals do not exist when the snapshot that *is* L2 genesis is built. So the guest carries a single owner address — chosen before anything is deployed, substituted into the app at snapshot time, and therefore covered by the genesis state root — and takes the portal addresses from that owner as an ordinary L1 deposit. Trusting any sender instead would let any contract mint claims against tokens the application really holds.

Proposals still use the permissioned game type and are never disputed: no fault proof VM can execute a Cartesi Machine. That is the remaining trust assumption, and it is a constructor argument on the validator rather than a hidden one. See [DESIGN.md](docs/DESIGN.md) §4 and §7c.

**The chain survives a restart.** `-datadir` gives the node a store: blocks and the machine's per-transaction emissions go into a pebble database through go-ethereum's own `rawdb`, and the machine itself is checkpointed whole at intervals with Cartesi's `cm_store`. Restarting loads the newest checkpoint and re-executes the blocks after it — on a 39-block devnet run, back and serving in about a second. Replay is not a shortcut around verification: each replayed block is checked against the state root and outputs commitment it was stored with, so a checkpoint that has drifted fails the restart rather than serving a wrong chain.

See **[docs/DESIGN.md](docs/DESIGN.md)** for the full architecture analysis, covering:

- What the Cartesi Machine provides as an execution layer (Merkleized state, CMIO, provable stepping, ZK pipeline)
- Why the OP Stack is the right chassis (and why Arbitrum Nitro is not)
- The Engine API shim — the one genuinely new component
- Two settlement plans: Cartesi-native (Dave/PRT + `machine-solidity-step`) vs. OP-native proving (Asterisc or Kailua-style ZK)
- How Cartesi outputs, reports and inspect map onto withdrawals, logs and `eth_call` — and why the withdrawal path forces Isthmus
- The app-chain dimension: hosting multiple applications on one machine, with cycle-based metering

## Outputs and receipts

The machine's emissions are recorded per transaction (`chain.TxOutputs`), split along the Cartesi provability boundary: **outputs** (vouchers and notices) are provable and destined for the block's outputs commitment; **reports** are diagnostic and must never enter a commitment. Outputs of a rejected input are dropped, since a rejection rolls the machine back; its reports are kept, because they usually explain the failure.

Provable outputs accumulate into a Merkle tree that matches Cartesi's on-chain tree exactly — height 63, leaves `keccak256(output)`, parents `keccak256(left‖right)`, zero-padded — so existing voucher proofs verify against it unchanged. The tree is cumulative over the chain, and its root is published in every header's `withdrawalsRoot`, which is what op-node turns into the L2 output root. Verifiers re-derive it from re-execution, so a payload claiming outputs the machine did not produce is rejected.

Receipts are synthesized from those records: outputs become logs, acceptance becomes `status`, mcycles become `gasUsed`. Each log carries the output's chain-wide index as a topic next to the raw bytes, which is exactly what a Cartesi output validity proof needs — so the receipt is enough to build the L1 proof later. Nothing on the OP Stack's critical path reads L2 receipts, so `receiptsRoot` and the header bloom stay empty and the encoding is not frozen into consensus while it is still moving.

Reports are not logs, because a log implies provability. They are served through the `cartesi_` namespace instead, alongside the output indices and the outputs commitment — see [JSON-RPC](#json-rpc) for every method the node serves.

## JSON-RPC

The node serves two listeners, assembled in `engineapi.NewHandler` (addresses under [Development](#development)). Only `engine_*` is exclusive to the engine port, where it is JWT-authenticated when a secret is configured; `eth_`, `cartesi_` and `miner_` are served on **both** — op-node and op-batcher read `eth_*` over the authenticated connection, and `miner_setMaxDASize` is required on the sequencer's L2 endpoint.

This is a deliberately small surface: the methods op-node, op-batcher and op-proposer actually call, the `eth_*` subset ordinary wallets and `cast` need, and a `cartesi_*` namespace for what `eth_*` cannot say faithfully. There are no filter, subscription, log-query, `debug_` or `txpool_` methods, and no `eth_getProof` — this chain has no Ethereum MPT, so `cartesi_getAccountProof` takes its place.

### `engine_` — the Engine API (engine port only)

Only these versions are served, for the reason in [Fork support](#fork-support).

| Method | Purpose |
|---|---|
| `engine_forkchoiceUpdatedV3` | Sets the unsafe/safe/finalized heads and, when op-node passes OP payload attributes, starts building the next block — returning the payload id. Reorgs are honoured by rewinding to a machine snapshot. |
| `engine_getPayloadV4` | Returns the execution payload built for a payload id. The header's `stateRoot` is the machine's Merkle root and its `withdrawalsRoot` is the Cartesi outputs commitment. |
| `engine_newPayloadV4` | Imports a payload from a peer or from L1 derivation and re-executes it on the machine, rejecting it unless the resulting root and outputs commitment match what the payload claims. This is the verifier path. Blob hashes and execution requests must be empty; a missing `parentBeaconBlockRoot` is rejected rather than defaulted. |

### `eth_` — the subset op-node, op-batcher and wallets read

| Method | Purpose |
|---|---|
| `eth_chainId` | The configured L2 chain id. |
| `eth_blockNumber` | Height of the unsafe head. |
| `eth_syncing` | Always `false`: the node has no sync protocol — it follows op-node, or replays from a checkpoint at startup. |
| `eth_getBlockByHash` | A block by hash, with transaction hashes or full transactions. Includes `requestsHash` and `withdrawalsRoot`, which clients that recompute the block hash need. |
| `eth_getBlockByNumber` | The same by number or by the `latest` / `safe` / `finalized` / `earliest` / `pending` tags. |
| `eth_sendRawTransaction` | Ingress for signed transactions. There is no public L2 mempool, so the sequencer's RPC is the only way in; the transaction lands in a bounded FIFO the next block drains. |
| `eth_getTransactionReceipt` | The receipt synthesized from what the machine emitted for that transaction — see [Outputs and receipts](#outputs-and-receipts). |
| `eth_getBlockReceipts` | Every receipt in a block. |
| `eth_call` | A read-only query: the call travels to the guest as the `EvmCall` envelope and runs as a machine inspect against a fork that is then discarded. A rejected inspect surfaces as the standard revert error (code 3) with the revert bytes, so `require`-style messages reach viem, ethers and `cast` verbatim. |
| `eth_getBalance` | The account's native balance, read straight out of the guest's accounts drive in machine memory — no fork, no execution. Zero on a machine without an accounts drive (the in-memory mock). |
| `eth_getTransactionCount` | The account's nonce from the same record. Since the guest enforces and bumps it, this is the next nonce a wallet must sign with. |
| `eth_getCode` | A four-byte marker for every address the guest routes (system built-ins, application contracts from the ABI drive, token façades from the accounts drive registry) and `0x` for everything else. There is no EVM bytecode to serve; the marker is what lets tooling treat a routed address as a contract. |
| `eth_gasPrice` | The head header's base fee plus a zero tip. There is no fee market — every header carries the same constant — so this is the honest suggestion, and it is what lets `cast send` run without gas flags. |
| `eth_maxPriorityFeePerGas` | Zero: nothing charges or pays priority fees, so a tip would buy no ordering. |
| `eth_feeHistory` | Fee history synthesized from headers, in geth's exact wire shape: the constant base fee, `gasUsedRatio` from metered mcycles over the block limit, and an all-zero reward matrix when percentiles are requested. Clamped to the blocks this node still holds. |
| `eth_estimateGas` | The per-input cycle budget (`MaxCyclesPerInput` at `CyclesPerGas`) expressed as gas — an upper bound the chain will accept, not a measurement of this call. A true estimate needs a signature-less simulation entry point in the guest; see [Known gaps](#known-gaps-that-are-not-about-proofs). |

### `cartesi_` — the machine's own vocabulary

| Method | Purpose |
|---|---|
| `cartesi_getTransactionEmissions` | Everything the machine produced for one transaction: provable outputs with their chain-wide indices, the reports, the cycle count, and whether the input was accepted. |
| `cartesi_getOutputsRoot` | The outputs commitment as of a block, plus the number of outputs the tree holds — which bounds the valid output indices there. |
| `cartesi_getOutputProof` | A Cartesi output validity proof — the raw output, its index and its sibling hashes — against a chosen block's commitment, which is what `Application.executeOutput` needs to execute a voucher on L1. Since the tree is cumulative an output is provable against any block from the one that emitted it onward; the block tag defaults to the safe head, since proposals follow the safe chain. |
| `cartesi_inspect` | A read-only query against the machine state at a block, with the reports returned individually and the payload passed through untouched both ways. `eth_call` is the enveloped, EVM-shaped view of the same mechanism. |
| `cartesi_getContracts` | Every address the guest routes as of a block — system built-ins and application contracts with their recorded ABIs, plus the token façades the accounts drive's registry implies — read off the parked machine's drives with no execution. Lets a client answer "what does this chain speak?" from drive bytes alone. |
| `cartesi_getAccountProof` | The account record (or its provable absence) with machine Merkle proofs against the block's `stateRoot`, plus the drive geometry and probe walk a verifier needs to replay the lookup itself. Requires a machine that can prove — against the in-memory mock it errors rather than serving proofless "proofs". |

### `miner_`

| Method | Purpose |
|---|---|
| `miner_setMaxDASize` | op-batcher's backpressure: when batches back up on L1 it asks the sequencer to build smaller blocks. The limits apply to mempool transactions; deposits are forced by op-node and cannot be shed. op-batcher treats an engine that does not serve this method as fatal, so it is served on both ports. |

## Layout

| Package | Purpose |
|---|---|
| `cmd/op-cartesi` | CLI: wires the machine, chain, and the two RPC listeners together. |
| `machine` | `Machine` interface (advance-input / inspect / root-hash / fork / close), a deterministic in-memory mock for development and tests, and a client for the emulator's `cartesi-jsonrpc-machine` remote protocol. The node never boots a machine: a snapshot arrives already parked at its first input yield, so genesis is the snapshot's own root hash. |
| `chain` | Block store and its durable half (`rawdb` over pebble, plus machine checkpoints), genesis construction, payload building (sequencer) and payload import/re-execution (verifier), reorgs and snapshot pruning, and the per-transaction record of machine emissions. Blocks are Ethereum-shaped headers whose `stateRoot` is the machine's Merkle root; gas is metered in machine mcycles. |
| `engineapi` | `engine_*`, `eth_*` and `cartesi_*` JSON-RPC services, Engine API JWT authentication, HTTP handler assembly. |
| `mempool` | Bounded FIFO transaction ingress (the OP Stack has no public L2 mempool; the sequencer's RPC is the only entry point). |
| `rollup` | Generates the `rollup.json` document op-node reads. |
| `integration` | Separate Go module: compatibility tests driving the shim with op-node's wire types. Kept out of the main module so the shim itself depends only on op-geth. |
| `devnet` | Brings the devnet up: anvil, the OP Stack L1 suite, the machine, the shim and op-node, one mprocs pane each. |
| `scripts` | Client scripts for a running devnet — deposits, withdrawals, balances — and the machine snapshot they run against. |

Transactions are ordinary signed Ethereum transactions (plus OP deposit transactions injected by op-node), which is what lets stock op-node and op-batcher handle our blocks unmodified.

Each one reaches the machine wrapped in Cartesi's **`EvmAdvance` envelope**, with the raw transaction as the payload:

```
EvmAdvance(chainId, appContract, msgSender, blockNumber, blockTimestamp, prevRandao, index, payload)
```

That is the encoding Cartesi's guest tools already decode, so a stock guest-tools rootfs and existing Cartesi applications run unmodified — the same reuse argument that picked the OP Stack. It also carries the L2 block context the guest could not otherwise learn, since the machine has no clock and no view of the chain. `msgSender` is the transaction's own sender (for a deposit, the L1 originator it carries); `index` is chain-wide, so the guest sees one gapless input sequence as it would from an InputBox. `appContract` is configurable and becomes load-bearing only when vouchers are executed through a Cartesi `Application` contract.

Every field of the envelope is derivable from the block header, so a verifier re-executing a block reconstructs the exact context the builder used.

## Development

```sh
go build ./...
go test ./...
(cd integration && go test ./...)   # op-node compatibility suite

# The TypeScript side is a bun workspace: the drive libraries
# (accounts-drive/js, abi-drive/js), the EVM-compat wire vocabulary
# (evm-compat/js), the guest runtime (app), the devnet guest
# application (demo), the devnet itself (devnet) and the client
# scripts that drive it (scripts).
bun install
bun run test
bun run typecheck

# Biome formats and lints every TypeScript file in the workspace, from the
# single biome.jsonc at the repository root.
bun run check         # format + lint + import order, report only
bun run check:fix     # ... and apply the safe fixes

# Run with the in-memory mock machine (no emulator needed):
go run ./cmd/op-cartesi run

# Run against a real emulator:
cartesi-jsonrpc-machine --server-address=127.0.0.1:6000 ... &
go run ./cmd/op-cartesi run \
  -machine.remote http://127.0.0.1:6000 \
  -engine.jwt-secret ./jwt.hex \
  -chain-id 901 -genesis.timestamp 1720000000

# Generate the rollup.json op-node needs (same chain flags as `run`):
go run ./cmd/op-cartesi genesis -h
```

The engine (authenticated, for op-node) and public `eth_*` endpoints listen on `127.0.0.1:8551` and `127.0.0.1:8545` by default. On startup the node logs the genesis block hash and state root.

The chain flags passed to `genesis` and `run` must match: they determine the L2 genesis block hash, and op-node refuses to start if the engine's genesis disagrees with its rollup config. `chainFlags()` in `devnet/lib/opcartesi.ts` is the single copy of them both are built from.

## Fork support

The fork schedule is **fixed**, not configurable: every fork through **Isthmus** is active from genesis. A new chain has no pre-fork history to preserve, and Isthmus is not optional — pre-Isthmus, op-node computes the L2 output root by proving the L2ToL1MessagePasser account against the block's state root, which cannot work for a Cartesi execution layer with no Ethereum MPT. A pre-Isthmus chain could never be proposed, so the shim does not offer one. See [DESIGN.md §7](docs/DESIGN.md).

That fixes the wire protocol too: `engine_forkchoiceUpdatedV3` plus the **V4** payload methods, which is exactly what op-node calls for an Isthmus chain. Holocene's EIP-1559 parameters are encoded into header `extraData` with op-geth's own encoder, so the bytes match what an op-geth engine would commit to.

Jovian and later are not supported: Jovian adds a minimum-base-fee field the shim does not implement.

## Roadmap

1. **Shim MVP** *(done)* — a Cartesi Machine and op-node in sequencer mode on an L1 devnet. Milestone: deposits credited in-guest, and a second verifier node deriving identical blocks from L1 data alone.
2. **Batcher, proposer, persistence** *(done)* — the L1 contract suite through `op-deployer`, `op-batcher` posting calldata batches, `op-proposer` creating games, and a store that survives a restart by replaying from a machine checkpoint.
3. **Withdrawals** *(done)* — the outputs tree in `withdrawalsRoot`, output validity proofs, and an L1 contract that opens a proposal's root claim so Cartesi's own verifier can execute a voucher against it. Cartesi-style portals bring ether and ERC-20 the other way. This was the withdrawal half of what the design doc filed under settlement track A; the disputing half is below.
4. **A provable definition of the computation** *(next)* — the prerequisite for *either* settlement track, and a design decision before it is a coding task. Two questions, both answered in [DESIGN.md §7e](docs/DESIGN.md):
   - **Which state transition function?** Dave already specifies one: a fixed `2^68` meta-cycle span per input, indexed as (input, big-arch cycle, uarch cycle), with the state a fixpoint once the machine yields. This chain's rule is `MaxCyclesPerInput` with a rejection branch when the budget is exceeded. Those are different functions, and ours is the one that would have to move.
   - **How does a referee check an input?** Cartesi's answer is that every input is hashed on L1 in an `InputBox`; OP's is that derivation runs inside the fault-proof VM. This chain has neither — its inputs are op-node's derivation output from compressed channel frames plus L1 logs, which no contract can re-derive. Mirroring inputs to L1 or proving derivation are the two honest options; both are real work.

   A third requirement falls out of the same reading: the outputs Merkle root has to live *inside* the machine's memory, because a referee cannot dispute a value that is not in the proven state. Today the shim computes it in Go.
5. **Settlement track A** — Dave/PRT. Note that `DaveConsensus` already implements `IOutputsMerkleRootValidator`, the interface `OutputExecutor` calls today, so pointing the executor at Dave is a smaller change than wrapping Dave as an OP `IDisputeGame`. Neither escapes step 4.
6. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest and get a cost per block; go/no-go on ZK settlement. Worth doing early: the number may redirect the choice, and it commits to nothing.

### Known gaps that are not about proofs

These do not change the trust model, which is why they sit outside the numbered steps — but a chain that ran for real would need them, and they are cheap to state plainly.

- **Inputs are free.** There is no fee market and no metering charged to anyone, so nothing rate-limits the sequencer's ingress. `MaxCyclesPerInput` bounds one input's execution; it does not bound a sender. This is the substantive question hiding behind the next two entries — once an input costs something, the payer needs an identity, and the rest follows from that rather than being bolted on ahead of it.
- **The account model reaches the shim — and, since v1, the rules (next bullet).** The devnet guest now keeps its ledger on a standard *accounts drive* inside the machine — [docs/ACCOUNTS.md](docs/ACCOUNTS.md), byte-level format in [docs/ACCOUNTS-DRIVE-SPEC.md](docs/ACCOUNTS-DRIVE-SPEC.md) — and the shim serves `eth_getBalance` and `eth_getTransactionCount` by reading the record straight out of machine memory, no fork, no execution (ACCOUNTS.md roadmap v0). The gas methods are now served too, so a plain `cast send` needs no gas flags: `eth_gasPrice`, `eth_maxPriorityFeePerGas` and `eth_feeHistory` are synthesized from headers — a constant base fee and zero tips, which *is* the truth until there is a fee market to describe — and `eth_estimateGas` returns the per-input cycle budget (`MaxCyclesPerInput` at `CyclesPerGas`) expressed as gas: an upper bound the chain will accept, not a measurement. A true estimate would run the payload on a discarded fork and report its cycles, but an estimate arrives unsigned and the guest now enforces sender and nonce (next bullet), so an unsigned replay would measure the rejection; measurement needs a signature-less simulation entry point in the guest, which belongs to the fee-market work.
- **Replay protection: now enforced in the guest.** For ordinary L2 transactions the guest recovers the sender from the signature (viem's secp256k1 in the routed guest; its Lua predecessor carried a pure-Lua implementation pinned against go-ethereum vectors), requires the transaction's nonce to equal the sender's accounts-drive record, bumps it on acceptance, and debits a flat per-transaction fee — deposits are exempt and keep their L1-origin authentication. The mempool applies the same nonce check at ingress, but only as a courtesy filter: the guest is the enforcer, inside the state the root commits to. The fee parameter is owner-settable and **defaults to 0 on the devnet** — fresh devnet senders hold no ether to charge until someone deposits — so nonce records are still free to mint until a deployment sets it nonzero (ACCOUNTS.md §5.7 is why one should). This was the v1 step of [docs/ACCOUNTS.md](docs/ACCOUNTS.md).
- **P2P is disabled**, so unsafe-head gossip and the reorg paths that come with it are untested.
- **Blob DA is unexercised** — batches are calldata only.
- **No snapshot sync.** A new node replays from genesis, or from a checkpoint it already has.
- **Proof construction walks the chain.** `leavesThrough` is linear in chain length, which is fine while outputs are rare and will not be at any other scale; it wants an index from output index to block.
