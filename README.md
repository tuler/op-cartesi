# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the [Engine API](https://specs.optimism.io/protocol/overview.html#engine-api) and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

Start here, then read **[docs/DESIGN.md](docs/DESIGN.md)** for the architecture in full: what the Cartesi Machine provides as an execution layer, why the OP Stack is the right chassis, how Cartesi outputs and reports map onto withdrawals and logs, and what a fault proof for this chain would require.

## Status

The chain runs. On a devnet it sequences L2 blocks, batches them to L1, is re-derived block-for-block by an independent verifier reading only L1, proposes to a `DisputeGameFactory`, and survives a restart. Both bridges — ether through the stock `OptimismPortal`, ERC-20 through `L1StandardBridge` — are covered end to end by the Go, TypeScript and Foundry suites, against Optimism's and geth's own verifiers; their live devnet re-run is the next thing on the list. Roadmap steps 1–3 are done; step 4 — [a provable definition of the computation](#roadmap) — is next.

What is **not** done is disputing a proposal. Proposals go into OP's permissioned game type and nobody can challenge them, because no fault-proof VM can execute a Cartesi Machine today. That is the chain's one remaining trust assumption, and it is a constructor argument on the validator rather than a hidden one — [DESIGN §8](docs/DESIGN.md) sets out exactly what closing it takes. The gaps that have nothing to do with proofs are listed under [Known gaps](#known-gaps-that-are-not-about-proofs).

## How it works

**Blocks are Ethereum-shaped.** Transactions are ordinary signed Ethereum transactions, plus the [OP deposit transactions](https://specs.optimism.io/protocol/deposits.html#the-deposited-transaction-type) op-node injects, which is what lets stock op-node and op-batcher handle these blocks unmodified. Each one reaches the machine wrapped in Cartesi's **`EvmAdvance` envelope**, with the raw transaction as the payload:

```
EvmAdvance(chainId, appContract, msgSender, blockNumber, blockTimestamp, prevRandao, index, payload)
```

That is the encoding Cartesi's guest tools already decode, so a stock guest-tools rootfs and existing Cartesi applications run unmodified. It also carries the L2 block context the guest could not otherwise learn, since the machine has no clock and no view of the chain. `msgSender` is the transaction's own sender (for a deposit, the L1 originator it carries); `index` is chain-wide, so the guest sees one gapless input sequence as it would from an InputBox. Every field is derivable from the block header, so a verifier re-executing a block reconstructs the exact context the builder used.

**The header carries two commitments.** `stateRoot` is the machine's Merkle root. `withdrawalsRoot` is a genuine Ethereum storage trie over the `L2ToL1MessagePasser.sentMessages` slots of every OP `Withdrawal` the guest has emitted — [the commitment the OP Stack expects there](https://specs.optimism.io/protocol/isthmus/exec-engine.html#l2tol1messagepasser-storage-root-in-header) — with the **Cartesi outputs Merkle root** at the reserved slot `keccak256("op-cartesi.outputsMerkleRoot")`. One commitment therefore serves both the portal's storage proofs and Cartesi's output proofs, and op-node turns it into the [L2 output root](https://specs.optimism.io/protocol/proposals.html#l2-output-commitment-construction) with no changes. Verifiers re-derive both from re-execution, so a payload claiming outputs or withdrawals the machine did not produce is rejected. See [DESIGN §5](docs/DESIGN.md).

**Both bridges are stock OP.** [`OptimismPortal.depositTransaction`](https://specs.optimism.io/protocol/deposits.html#deposit-contract) funds accounts; ether leaves through [`proveWithdrawalTransaction` / `finalizeWithdrawalTransaction`](https://specs.optimism.io/protocol/withdrawals.html#withdrawal-verification-and-finalization), paid from the lockbox the deposits filled. ERC-20 rides `L1StandardBridge` in both directions, against `L2CrossDomainMessenger` (0x4200…0007) and `L2StandardBridge` (0x4200…0010) adopted as real [predeploys](https://specs.optimism.io/protocol/predeploys.html) in the guest — no custom contract on the path. Cartesi vouchers remain for what an application emits for its own reasons, proven against the same proposal through one small contract, [`OPOutputsMerkleRootValidator`](contracts/src/OPOutputsMerkleRootValidator.sol). Nothing forks `OptimismPortal`. See [DESIGN §6](docs/DESIGN.md).

**The chain survives a restart.** `-datadir` gives the node a store: blocks and the machine's per-transaction emissions go into a pebble database through go-ethereum's own `rawdb`, and the machine itself is checkpointed whole at intervals with Cartesi's `cm_store`. Restarting loads the newest checkpoint and re-executes the blocks after it — on a 39-block devnet run, back and serving in about a second. Replay is not a shortcut around verification: each replayed block is checked against the state root and outputs commitment it was stored with, so a drifted checkpoint fails the restart rather than serving a wrong chain. See [DESIGN §7](docs/DESIGN.md).

### Outputs and receipts

The machine's emissions are recorded per transaction (`chain.TxOutputs`), split along the Cartesi provability boundary: **outputs** (vouchers and notices) are provable and enter the block's outputs commitment; **reports** are diagnostic and must never enter a commitment. Outputs of a rejected input are dropped, since a rejection rolls the machine back; its reports are kept, because they usually explain the failure.

Provable outputs accumulate into a Merkle tree that matches Cartesi's on-chain tree exactly — height 63, leaves `keccak256(output)`, parents `keccak256(left‖right)`, zero-padded — so existing voucher proofs verify against it unchanged. The tree is cumulative over the chain, and its root is committed through the withdrawal trie described above.

Receipts are synthesized from those records: outputs become logs, acceptance becomes `status`, mcycles become `gasUsed`. Each log carries the output's chain-wide index as a topic next to the raw bytes, which is exactly what a Cartesi output validity proof needs — so the receipt is enough to build the L1 proof later. Nothing on the OP Stack's critical path reads L2 receipts, so `receiptsRoot` and the header bloom stay empty and the encoding is not frozen into consensus while it is still moving.

Reports are not logs, because a log implies provability. They are served through the `cartesi_` namespace instead, alongside the output indices and the outputs commitment — see [JSON-RPC](#json-rpc) for every method the node serves.

## Running it

`./devnet/start-devnet.ts` brings up the whole stack: anvil as L1, the OP Stack L1 suite deployed with `op-deployer`, a Cartesi Machine, op-cartesi, op-node in sequencer mode, op-batcher and op-proposer — plus a **second node** with its own machine, engine and op-node that sequences nothing and rebuilds the chain purely from what the batcher posted to L1. It reaches byte-identical blocks: same hash, same machine root, same outputs commitment. That is the property that makes this a rollup rather than a database with an RPC.

Each piece runs in its own [mprocs](https://github.com/pvolok/mprocs) pane — including the machine's console and the guest's own per-transaction reports — so the whole stack is one screen you can watch, stop and restart a piece at a time. Client scripts drive it: `bun scripts/deposit.ts <address> <wei>`, `bun scripts/withdraw.ts <address> <wei>`, and the ERC-20 pair. See **[devnet/README.md](devnet/README.md)**.

The stack has been run against the **official released images** — op-node v1.19.3 and op-batcher v1.16.11 — as well as against locally built binaries. The OP monorepo ships no binaries of its own, so `./devnet/start-devnet.ts` falls back to docker when they are not on your `PATH`; nothing needs compiling but op-cartesi itself.

## Verification

Compatibility is checked against **op-node's own types** rather than hand-written JSON: the [`integration`](integration/) suite drives the shim over authenticated HTTP using `op-service/eth`, and checks each block with op-node's `ExecutionPayloadEnvelope.CheckBlockHash`, which independently reconstructs the header. A deliberate one-field mutation to header construction is caught there, so the check has teeth.

The chain also builds blocks on a **real Cartesi Machine**: the JSON-RPC client is pinned to machine-emulator 0.21.0 by probing a running server, and `chain` and `machine` carry tests that load a real machine, build blocks on it, re-execute them as a verifier, and check that the outputs commitment the host computes is byte-identical to the one the guest maintains. They are skipped unless a snapshot is supplied:

```sh
./scripts/build-snapshot.ts
OP_CARTESI_TEST_SNAPSHOT=./demo/.cartesi/image \
OP_CARTESI_TEST_LEDGER_SNAPSHOT=./demo/.cartesi/image go test ./...
```

The second variable turns on the deposit and token tests, which need a guest that means something by its inputs rather than merely consuming them — the routed guest of [`demo`](demo/README.md), which is what the snapshot script builds.

The withdrawal trie and the cross-domain encodings are pinned in three directions: Go against the guest's TypeScript encoders, Go against geth's own trie verifier, and Go against the vendored Solidity — Optimism's real `SecureMerkleTrie`, `Encoding` and `Hashing`, so the bytes are judged by the exact code that will judge them on L1.

## JSON-RPC

The node serves two listeners, assembled in `engineapi.NewHandler` (addresses under [Development](#development)). Only `engine_*` is exclusive to the engine port, where it is JWT-authenticated when a secret is configured; `eth_`, `cartesi_` and `miner_` are served on **both** — op-node and op-batcher read `eth_*` over the authenticated connection, and `miner_setMaxDASize` is required on the sequencer's L2 endpoint.

This is a deliberately small surface: the methods op-node, op-batcher and op-proposer actually call, the `eth_*` subset ordinary wallets and `cast` need, and a `cartesi_*` namespace for what `eth_*` cannot say faithfully. There are no filter, subscription, log-query, `debug_` or `txpool_` methods. `eth_getProof` answers for exactly one address — the `L2ToL1MessagePasser`, whose storage trie is the withdrawal commitment the header carries — because that is the call viem's withdrawal flow makes; there is still no Ethereum account trie, so for everything else `cartesi_getAccountProof` takes its place.

### `engine_` — the Engine API (engine port only)

Only these versions are served, for the reason in [Fork support](#fork-support).

| Method | Purpose |
|---|---|
| [`engine_forkchoiceUpdatedV3`](https://specs.optimism.io/protocol/exec-engine.html#engine_forkchoiceupdatedv3) | Sets the unsafe/safe/finalized heads and, when op-node passes OP payload attributes, starts building the next block — returning the payload id. Reorgs are honoured by rewinding to a machine snapshot. |
| [`engine_getPayloadV4`](https://specs.optimism.io/protocol/exec-engine.html#engine_getpayloadv4) | Returns the execution payload built for a payload id. The header's `stateRoot` is the machine's Merkle root and its `withdrawalsRoot` is the withdrawal trie root — the message passer storage trie holding the Cartesi outputs commitment at its reserved slot. |
| [`engine_newPayloadV4`](https://specs.optimism.io/protocol/exec-engine.html#engine_newpayloadv4) | Imports a payload from a peer or from L1 derivation and re-executes it on the machine, rejecting it unless the resulting root and withdrawal commitment match what the payload claims. This is the verifier path. Blob hashes and execution requests must be empty; a missing `parentBeaconBlockRoot` is rejected rather than defaulted. |

### `eth_` — the subset op-node, op-batcher and wallets read

| Method | Purpose |
|---|---|
| `eth_chainId` | The configured L2 chain id. |
| `eth_blockNumber` | Height of the unsafe head. |
| `eth_syncing` | Always `false`: the node has no sync protocol — it follows op-node, or replays from a checkpoint at startup. |
| `eth_getBlockByHash` | A block by hash, with transaction hashes or full transactions. Includes `requestsHash` and `withdrawalsRoot`, which clients that recompute the block hash need. |
| `eth_getBlockByNumber` | The same by number or by the `latest` / `safe` / `finalized` / `earliest` / `pending` tags. |
| `eth_sendRawTransaction` | Ingress for signed transactions. There is no public L2 mempool, so the sequencer's RPC is the only way in; the transaction lands in a bounded FIFO the next block drains. |
| `eth_getTransactionByHash` | A transaction from the canonical chain with its block coordinates, from the pool with null ones, or null. Standard client flows depend on it before they ask for a receipt — viem's `waitForTransactionReceipt` fetches the transaction first for its replacement detection, and treats a missing method as fatal. |
| `eth_getTransactionReceipt` | The receipt synthesized from what the machine emitted for that transaction — see [Outputs and receipts](#outputs-and-receipts). |
| `eth_getBlockReceipts` | Every receipt in a block. |
| `eth_call` | A read-only query: the call travels to the guest as the `EvmCall` envelope and runs as a machine inspect against a fork that is then discarded. A rejected inspect surfaces as the standard revert error (code 3) with the revert bytes, so `require`-style messages reach viem, ethers and `cast` verbatim. |
| `eth_getBalance` | The account's native balance, read straight out of the guest's accounts drive in machine memory — no fork, no execution. Zero on a machine without an accounts drive (the in-memory mock). |
| `eth_getTransactionCount` | The account's nonce from the same record. Since the guest enforces and bumps it, this is the next nonce a wallet must sign with. |
| `eth_getProof` | Storage proofs against the withdrawal trie, for the `L2ToL1MessagePasser` address only — `storageHash` is the header's `withdrawalsRoot`, the storage proof is what `OptimismPortal.proveWithdrawalTransaction` verifies, and the account proof is empty because there is no account trie. Any other address is refused with a pointer to `cartesi_getAccountProof`. |
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
| `cartesi_getOutputProof` | A Cartesi output validity proof — the raw output, its index and its sibling hashes — against a chosen block's outputs root, plus the storage proof anchoring that root to the block's `withdrawalsRoot` (the withdrawal trie), which together are what `OPOutputsMerkleRootValidator.accept` and `Application.executeOutput` need on L1. Since the tree is cumulative an output is provable against any block from the one that emitted it onward; the block tag defaults to the safe head, since proposals follow the safe chain. |
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

The fork schedule is **fixed**, not configurable: every fork through **[Isthmus](https://specs.optimism.io/protocol/isthmus/exec-engine.html)** is active from genesis. A new chain has no pre-fork history to preserve, and Isthmus is not optional — pre-Isthmus, op-node computes the L2 output root by proving the L2ToL1MessagePasser account against the block's state root, which cannot work for a Cartesi execution layer with no Ethereum MPT. A pre-Isthmus chain could never be proposed, so the shim does not offer one. See [DESIGN §4](docs/DESIGN.md).

That fixes the wire protocol too: `engine_forkchoiceUpdatedV3` plus the **V4** payload methods, which is exactly what op-node calls for an Isthmus chain. [Holocene's EIP-1559 parameters](https://specs.optimism.io/protocol/holocene/exec-engine.html#eip-1559-parameters-in-block-header) are encoded into header `extraData` with op-geth's own encoder, so the bytes match what an op-geth engine would commit to.

Jovian and later are not supported: Jovian adds a [minimum-base-fee field](https://specs.optimism.io/protocol/jovian/exec-engine.html#minimum-base-fee-in-block-header) the shim does not implement.

## Roadmap

1. **Shim MVP** *(done)* — a Cartesi Machine and op-node in sequencer mode on an L1 devnet. Milestone: deposits credited in-guest, and a second verifier node deriving identical blocks from L1 data alone.
2. **Batcher, proposer, persistence** *(done)* — the L1 contract suite through `op-deployer`, `op-batcher` posting calldata batches, `op-proposer` creating games, and a store that survives a restart by replaying from a machine checkpoint.
3. **Withdrawals** *(done)* — the withdrawal trie in `withdrawalsRoot`, ether through stock `OptimismPortal`, ERC-20 both directions through `L1StandardBridge` and the adopted messenger/bridge predeploys, Cartesi output validity proofs, and an L1 contract that opens a proposal's root claim so Cartesi's own verifier can execute app-specific vouchers. See [DESIGN §5–§6](docs/DESIGN.md).
4. **A provable definition of the computation** *(next)* — the prerequisite for *either* settlement track, and a design decision before it is a coding task. Three questions, all answered in [DESIGN §8](docs/DESIGN.md):
   - **Which state transition function?** Dave specifies a fixed `2^68` meta-cycle span per input, indexed as (input, big-arch cycle, uarch cycle), with the state a fixpoint once the machine yields. This chain's rule is `MaxCyclesPerInput` with a rejection branch when the budget is exceeded. Those are different functions, and ours is the one that would have to move.
   - **How does a referee check an input?** Cartesi hashes every input on L1 in an `InputBox`; OP runs derivation inside the fault-proof VM. This chain has neither — its inputs are op-node's derivation output from compressed channel frames plus L1 logs, which no contract can re-derive. Mirroring inputs to L1 or proving derivation are the two honest options; both are real work.
   - **Where does the outputs root live?** Inside the machine's memory, because a referee cannot dispute a value that is not in the proven state. Today the shim computes it in Go.
5. **Settlement track A** — Dave/PRT. `DaveConsensus` already implements `IOutputsMerkleRootValidator`, the interface `OutputExecutor` calls today, so pointing the executor at Dave is a smaller change than wrapping Dave as an OP `IDisputeGame`. Neither escapes step 4.
6. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest and get a cost per block; go/no-go on ZK settlement. Worth doing early: the number may redirect the choice, and it commits to nothing.

### Known gaps that are not about proofs

These do not change the trust model, which is why they sit outside the numbered steps — but a chain that ran for real would need them.

- **Inputs are free.** There is no fee market and no metering charged to anyone, so nothing rate-limits the sequencer's ingress. `MaxCyclesPerInput` bounds one input's execution; it does not bound a sender. This is the substantive question behind the next two entries — once an input costs something, the payer needs an identity, and the rest follows.
- **Gas is a constant, not a measurement.** `eth_gasPrice`, `eth_maxPriorityFeePerGas` and `eth_feeHistory` are synthesized from headers — a constant base fee and zero tips, which *is* the truth until there is a fee market to describe — and `eth_estimateGas` returns the per-input cycle budget (`MaxCyclesPerInput` at `CyclesPerGas`) expressed as gas: an upper bound the chain will accept. A true estimate would run the payload on a discarded fork and report its cycles, but an estimate arrives unsigned and the guest enforces sender and nonce, so an unsigned replay would measure the rejection. Measurement needs a signature-less simulation entry point in the guest, which belongs with the fee-market work.
- **Replay protection is enforced, but free.** The guest recovers the sender from the signature, requires the transaction's nonce to equal the sender's accounts-drive record, bumps it on acceptance, and debits a flat per-transaction fee — deposits are exempt and keep their L1-origin authentication. The mempool applies the same nonce check at ingress as a courtesy filter; the guest is the enforcer, inside the state the root commits to. The fee parameter is owner-settable and **defaults to 0 on the devnet** — fresh senders hold no ether to charge until someone deposits — so nonce records are still free to mint until a deployment sets it nonzero ([ACCOUNTS.md §5.7](docs/ACCOUNTS.md) is why one should).
- **P2P is disabled**, so unsafe-head gossip and the reorg paths that come with it are untested.
- **Blob DA is unexercised** — batches are calldata only.
- **No snapshot sync.** A new node replays from genesis, or from a checkpoint it already has.
- **Proof construction walks the chain.** `leavesThrough` is linear in chain length, which is fine while outputs are rare and will not be at any other scale; it wants an index from output index to block.

## Further reading

| Document | What it covers |
|---|---|
| [docs/DESIGN.md](docs/DESIGN.md) | The architecture: the shim, the commitments, bridging, persistence, and what settlement requires. |
| [docs/RAAS.md](docs/RAAS.md) | What it would take to launch chains from a customer's machine snapshot on Sepolia or mainnet: the components, and the gaps a hosted service forces open. |
| [docs/EVM-COMPAT.md](docs/EVM-COMPAT.md) | How the guest speaks EVM at the ABI boundary — `to`-address routing to native handlers, ERC-20 façades, events, `eth_call`. |
| [docs/ACCOUNTS.md](docs/ACCOUNTS.md) · [ACCOUNTS-DRIVE-SPEC.md](docs/ACCOUNTS-DRIVE-SPEC.md) | The guest's account model, and the byte-level drive format the host reads balances and nonces out of. |
| [docs/ABI-DRIVE-SPEC.md](docs/ABI-DRIVE-SPEC.md) | The drive recording which addresses the guest routes and what ABI each speaks. |
| [devnet/README.md](devnet/README.md) | Running the devnet, pane by pane. |
| [OP Stack specs](https://specs.optimism.io/) | The protocol this chain plugs into: the [Engine API](https://specs.optimism.io/protocol/exec-engine.html#engine-api), [derivation](https://specs.optimism.io/protocol/derivation.html), [deposits](https://specs.optimism.io/protocol/deposits.html), [withdrawals](https://specs.optimism.io/protocol/withdrawals.html), [output proposals](https://specs.optimism.io/protocol/proposals.html#l2-output-commitment-construction) and [fault proofs](https://specs.optimism.io/fault-proof/index.html). |
