# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the Engine API and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

## Status

Shim scaffold (roadmap step 1, in progress). The Engine API service builds, is unit-tested against a deterministic mock machine, and implements the full sequencing loop — `engine_forkchoiceUpdatedV3` (with OP payload attributes) → `engine_getPayloadV3` → `engine_newPayloadV3` — plus verifier-side re-execution, reorg handling via machine snapshots, JWT auth, and the `eth_*` subset op-node and op-batcher read. Integration against a real `cartesi-jsonrpc-machine` server and a live op-node devnet is the next milestone.

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

Transactions are ordinary signed Ethereum transactions (plus OP deposit transactions injected by op-node); the raw transaction bytes are what the machine receives as CMIO inputs. Keeping them RLP-parseable is what lets stock op-node and op-batcher handle our blocks unmodified.

## Development

```sh
go build ./...
go test ./...

# Run with the in-memory mock machine (no emulator needed):
go run ./cmd/op-cartesi

# Run against a real emulator:
cartesi-jsonrpc-machine --server-address=127.0.0.1:6000 ... &
go run ./cmd/op-cartesi \
  -machine.remote http://127.0.0.1:6000 \
  -engine.jwt-secret ./jwt.hex \
  -chain-id 901 -genesis.timestamp 1720000000
```

The engine (authenticated, for op-node) and public `eth_*` endpoints listen on `127.0.0.1:8551` and `127.0.0.1:8545` by default. On startup the node logs the genesis block hash and state root — these go into the `rollup.json` given to op-node.

## Roadmap (from the design doc)

1. **Shim MVP** *(in progress — scaffold done, devnet integration next)* — local Cartesi Machine + op-node in sequencer mode on an L1 devnet, permissioned game type, no proofs. Milestone: deposits credited in-guest; a second verifier node derives identical blocks from L1 data alone.
2. **Batcher/proposer integration** — snapshot-based reorg handling, guest-side deposit decoder.
3. **Settlement track A** — Dave/PRT wrapped as an OP `IDisputeGame`, calldata batches, voucher-based withdrawals.
4. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest; go/no-go on ZK settlement.
