# op-cartesi

An OP Stack L2 whose execution layer is a [Cartesi Machine](https://github.com/cartesi/machine-emulator) (deterministic RISC-V, Linux) instead of an EVM.

The core of this project is an **Engine API shim**: a service that sits where op-geth normally sits — speaking the Engine API and a minimal `eth_*` subset to `op-node` on one side, and driving a Cartesi Machine through its CMIO input/output protocol on the other. The machine's Merkle root hash serves as the L2 state root. Everything else — sequencing, data availability, derivation, L1 bridging, and dispute tooling — is reused from the OP Stack.

## Status

Design phase. See **[docs/DESIGN.md](docs/DESIGN.md)** for the full architecture analysis, covering:

- What the Cartesi Machine provides as an execution layer (Merkleized state, CMIO, provable stepping, ZK pipeline)
- Why the OP Stack is the right chassis (and why Arbitrum Nitro is not)
- The Engine API shim — the one genuinely new component
- Two settlement plans: Cartesi-native (Dave/PRT + `machine-solidity-step`) vs. OP-native proving (Asterisc or Kailua-style ZK)
- The app-chain dimension: hosting multiple applications on one machine, with cycle-based metering

## Roadmap (from the design doc)

1. **Shim MVP** — local Cartesi Machine + op-node in sequencer mode on an L1 devnet, permissioned game type, no proofs. Milestone: deposits credited in-guest; a second verifier node derives identical blocks from L1 data alone.
2. **Batcher/proposer integration** — snapshot-based reorg handling, guest-side deposit decoder.
3. **Settlement track A** — Dave/PRT wrapped as an OP `IDisputeGame`, calldata batches, voucher-based withdrawals.
4. **Settlement track B** — benchmark the freestanding emulator inside a RISC Zero guest; go/no-go on ZK settlement.
