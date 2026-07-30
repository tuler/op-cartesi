# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the Engine API and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

## Status

Roadmap step 1, in progress. The shim implements the full sequencing loop — `engine_forkchoiceUpdatedV3` (with OP payload attributes) → `engine_getPayloadV3` → `engine_newPayloadV3` — plus verifier-side re-execution, reorg handling via machine snapshots, JWT auth, and the `eth_*` subset op-node and op-batcher read. It generates the `rollup.json` op-node consumes, and supports every fork from Ecotone through Holocene (see [Fork support](#fork-support)).

Compatibility is verified against **op-node's own types** rather than hand-written JSON: the [`integration`](integration/) suite drives the shim over authenticated HTTP using `op-service/eth`, and checks each block with op-node's `ExecutionPayloadEnvelope.CheckBlockHash`, which independently reconstructs the header. A deliberate one-field mutation to header construction is caught there, so the check has teeth.

Still outstanding for the milestone: running against a real `cartesi-jsonrpc-machine` server (the emulator's JSON-RPC encodings are decoded tolerantly and need pinning to a release), and a live op-node on an L1 devnet with deployed contracts. See [devnet/](devnet/).

See **[docs/DESIGN.md](docs/DESIGN.md)** for the full architecture analysis, covering:

- What the Cartesi Machine provides as an execution layer (Merkleized state, CMIO, provable stepping, ZK pipeline)
- Why the OP Stack is the right chassis (and why Arbitrum Nitro is not)
- The Engine API shim — the one genuinely new component
- Two settlement plans: Cartesi-native (Dave/PRT + `machine-solidity-step`) vs. OP-native proving (Asterisc or Kailua-style ZK)
- The app-chain dimension: hosting multiple applications on one machine, with cycle-based metering

## Layout

| Package | Purpose |
|---|---|
| `cmd/op-cartesi` | CLI: wires the machine, chain, and the two RPC listeners together. |
| `machine` | `Machine` interface (advance-input / root-hash / fork / close), a deterministic in-memory mock for development and tests, and a client for the emulator's `cartesi-jsonrpc-machine` remote protocol. |
| `chain` | Block store, genesis construction, payload building (sequencer) and payload import/re-execution (verifier), reorgs and snapshot pruning. Blocks are Ethereum-shaped headers whose `stateRoot` is the machine's Merkle root; gas is metered in machine mcycles. |
| `engineapi` | `engine_*` + `eth_*` JSON-RPC services, Engine API JWT authentication, HTTP handler assembly. |
| `mempool` | Bounded FIFO transaction ingress (the OP Stack has no public L2 mempool; the sequencer's RPC is the only entry point). |
| `rollup` | Generates the `rollup.json` document op-node reads. |
| `integration` | Separate Go module: compatibility tests driving the shim with op-node's wire types. Kept out of the main module so the shim itself depends only on op-geth. |
| `devnet` | Scripts and instructions for running the shim alongside op-node. |

Transactions are ordinary signed Ethereum transactions (plus OP deposit transactions injected by op-node); the raw transaction bytes are what the machine receives as CMIO inputs. Keeping them RLP-parseable is what lets stock op-node and op-batcher handle our blocks unmodified.

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

The shim implements the **V3** Engine API methods, covering Ecotone through Holocene; the generated rollup config activates those forks at genesis. Holocene's EIP-1559 parameters are encoded into header `extraData` using op-geth's own encoder, so the bytes match what an op-geth engine would commit to.

Isthmus and later are not supported yet: they switch op-node to `engine_newPayloadV4`/`engine_getPayloadV4` and add the `withdrawalsRoot` header field.

## Roadmap (from the design doc)

1. **Shim MVP** *(in progress — sequencing loop, rollup config and op-node compatibility done; real emulator and live devnet next)* — local Cartesi Machine + op-node in sequencer mode on an L1 devnet, permissioned game type, no proofs. Milestone: deposits credited in-guest; a second verifier node derives identical blocks from L1 data alone.
2. **Batcher/proposer integration** — snapshot-based reorg handling, guest-side deposit decoder.
3. **Settlement track A** — Dave/PRT wrapped as an OP `IDisputeGame`, calldata batches, voucher-based withdrawals.
4. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest; go/no-go on ZK settlement.
