# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the Engine API and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

## Status

**Roadmap step 1 is done.** Its milestone was *deposits credited in-guest, and blocks derived identically by a second verifier node from L1 data alone* — both now hold on a live devnet. The shim implements the full sequencing loop — `engine_forkchoiceUpdatedV3` (with OP payload attributes) → `engine_getPayloadV4` → `engine_newPayloadV4` — plus verifier-side re-execution, reorg handling via machine snapshots, JWT auth, and the `eth_*` subset op-node and op-batcher read. It generates the `rollup.json` op-node consumes, and runs every fork through Isthmus from genesis (see [Fork support](#fork-support)).

Compatibility is verified against **op-node's own types** rather than hand-written JSON: the [`integration`](integration/) suite drives the shim over authenticated HTTP using `op-service/eth`, and checks each block with op-node's `ExecutionPayloadEnvelope.CheckBlockHash`, which independently reconstructs the header. A deliberate one-field mutation to header construction is caught there, so the check has teeth.

The chain also builds blocks on a **real Cartesi Machine**: the JSON-RPC client is pinned to machine-emulator 0.21 by probing a running server, and `chain` and `machine` carry tests that load a real machine, build blocks on it, re-execute them as a verifier, and check that the outputs commitment the host computes is byte-identical to the one the guest maintains. They are skipped unless a snapshot is supplied:

```sh
./devnet/build-snapshot.sh
OP_CARTESI_TEST_SNAPSHOT=./devnet/snapshot \
OP_CARTESI_TEST_BANK_SNAPSHOT=./devnet/snapshot go test ./...
```

The second variable turns on the deposit tests, which need a guest that means something by its inputs rather than merely consuming them — the ledger app the snapshot script builds by default.

**The chain round-trips through L1.** `./devnet/start-devnet.sh` brings up anvil as L1, a Cartesi Machine, op-cartesi, and op-node in sequencer mode; op-node sequences L2 blocks continuously, each carrying the L1-attributes deposit it injects, wrapped in the `EvmAdvance` envelope and executed by the machine. The block's state root is the machine's Merkle root and its `withdrawalsRoot` is the outputs commitment.

`op-batcher` posts those blocks to L1 as calldata batches, which advances the safe head. A **second node** then runs alongside — its own machine, engine and op-node, sequencing nothing — and rebuilds the chain purely from what the batcher put on L1. It reaches byte-identical blocks: same hash, same machine root, same outputs commitment. That is the property that makes this a rollup rather than a database with an RPC.

The stack has been run against the **official released images** — op-node v1.19.3 and op-batcher v1.16.11 — as well as against locally built binaries. The OP monorepo ships no binaries of its own, so `./devnet/start-devnet.sh` falls back to docker when they are not on your `PATH`; nothing needs compiling but op-cartesi itself.

**Deposits reach the guest.** `./devnet/deposit.sh <address> <wei>` emits the canonical `TransactionDeposited` event on L1; op-node derives it into an L2 deposit transaction, and the guest — a small ledger app in [`devnet/bank-app.sh`](devnet/bank-app.sh) — decodes it and credits the recipient. The balance is machine state, so the state root commits to it, and `eth_call` reads it back through the machine's inspect protocol. The verifier, deriving from L1 alone, arrives at the same balance.

**Proposals land on L1.** The devnet deploys the full OP Stack L1 suite with `op-deployer` and runs `op-proposer` against the `DisputeGameFactory`. The claim it submits is `keccak(0³² ‖ stateRoot ‖ withdrawalsRoot ‖ blockHash)` — recomputed independently and matched against the on-chain game — so the root claim on L1 commits to the machine's Merkle root *and* the Cartesi outputs tree. Deposits go through the real `OptimismPortal.depositTransaction`.

**Withdrawals work, through Cartesi vouchers.** `./devnet/withdraw.sh <address> <wei>` asks the guest to withdraw; it emits a Cartesi `Voucher`, the voucher enters the outputs tree, `op-proposer` proposes the block, and an L1 contract proves the voucher against that proposal and executes it — moving real ETH. Verified end to end on the devnet, including that a voucher is single-use.

The bridge is one small contract. A Cartesi `Application` asks one question before executing an output — `isOutputsMerkleRootValid` — and an OP proposal already commits to the answer, since op-node's root claim is `keccak(version ‖ stateRoot ‖ withdrawalsRoot ‖ blockHash)` and `withdrawalsRoot` *is* the Cartesi outputs root. [`OPOutputsMerkleRootValidator`](contracts/src/OPOutputsMerkleRootValidator.sol) opens that preimage on chain; verification uses Cartesi's own `LibMerkle32`, vendored unmodified. Nothing forks `OptimismPortal` — its withdrawal path wants an MPT proof this chain cannot produce, so this sidesteps it rather than replacing it.

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

Reports are not logs, because a log implies provability. They are served through the `cartesi_` namespace instead, alongside the output indices and the outputs commitment.

| Method | Purpose |
|---|---|
| `eth_getTransactionReceipt`, `eth_getBlockReceipts` | Standard receipts; outputs appear as logs. |
| `eth_call` | Runs the machine's read-only inspect against a discarded fork, returning the concatenated reports. |
| `cartesi_getTransactionEmissions` | Outputs with their chain-wide indices, plus the reports and cycle count. |
| `cartesi_getOutputsRoot` | The outputs commitment and output count as of a block. |
| `cartesi_inspect` | Inspect with the reports kept separate. |

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
| `devnet` | Scripts and instructions for running the shim alongside op-node. |

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

The chain flags passed to `genesis` and `run` must match: they determine the L2 genesis block hash, and op-node refuses to start if the engine's genesis disagrees with its rollup config. `devnet/env.sh` keeps a single copy of them for both scripts.

## Fork support

The fork schedule is **fixed**, not configurable: every fork through **Isthmus** is active from genesis. A new chain has no pre-fork history to preserve, and Isthmus is not optional — pre-Isthmus, op-node computes the L2 output root by proving the L2ToL1MessagePasser account against the block's state root, which cannot work for a Cartesi execution layer with no Ethereum MPT. A pre-Isthmus chain could never be proposed, so the shim does not offer one. See [DESIGN.md §7](docs/DESIGN.md).

That fixes the wire protocol too: `engine_forkchoiceUpdatedV3` plus the **V4** payload methods, which is exactly what op-node calls for an Isthmus chain. Holocene's EIP-1559 parameters are encoded into header `extraData` with op-geth's own encoder, so the bytes match what an op-geth engine would commit to.

Jovian and later are not supported: Jovian adds a minimum-base-fee field the shim does not implement.

## Roadmap (from the design doc)

1. **Shim MVP** *(done)* — local Cartesi Machine + op-node in sequencer mode on an L1 devnet, permissioned game type, no proofs. Milestone: deposits credited in-guest; a second verifier node derives identical blocks from L1 data alone.
2. **Batcher/proposer integration** *(next)* — the L1 contract suite, `op-proposer`, and persistence for blocks and outputs, which are still in memory.
3. **Settlement track A** — Dave/PRT wrapped as an OP `IDisputeGame`, calldata batches, voucher-based withdrawals.
4. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest; go/no-go on ZK settlement.
