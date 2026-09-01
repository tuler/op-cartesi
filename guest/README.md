# guest

The **application** inside the machine: the program that receives inputs,
maintains the state the machine's Merkle root commits to, and emits the
outputs the chain proves on L1.

| | | |
|---|---|---|
| [`ts/`](ts) | TypeScript on `@cartesi/rollup` | the reference runtime, `@op-cartesi/app` |

## Writing another

A Cartesi Machine runs any RISC-V Linux program, so nothing about the guest is
tied to a language. What binds it is:

- [EVM-COMPAT](../docs/EVM-COMPAT.md) — the routing standard: how an address
  becomes a handler, the admission rules, the outcome model, and how
  `eth_call` arrives and is answered.
- [BLOCKS-SPEC §7](../docs/BLOCKS-SPEC.md) — the `EvmAdvance` envelope every
  input arrives in, and [§10](../docs/BLOCKS-SPEC.md) — what an output is
  versus a report, which decides what can ever be proven.
- The two drives, which the host reads out of machine memory without executing
  anything: [ACCOUNTS-DRIVE-SPEC](../docs/ACCOUNTS-DRIVE-SPEC.md) and
  [ABI-DRIVE-SPEC](../docs/ABI-DRIVE-SPEC.md). Both already have libraries in
  six and two languages under [`lib/`](../lib) — start there rather than
  reimplementing them.

The encodings a guest produces are pinned by
[`conformance/encodings/`](../conformance/README.md), which the TypeScript
runtime's own tests replay: withdrawals, events, and the call envelope.
