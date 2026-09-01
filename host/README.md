# host

The **engine**: the process that sits where op-geth sits, speaking the Engine
API to `op-node` on one side and driving a Cartesi Machine on the other.

One implementation lives here today.

| | | |
|---|---|---|
| [`go/`](go) | Go | the reference implementation |

## Writing another

The engine is bound by two documents and nothing else:

- [ENGINE-RPC-SPEC](../docs/ENGINE-RPC-SPEC.md) — what it serves. A wire
  contract: `engine_*` for op-node, an `eth_*` subset for op-batcher and
  wallets, and a `cartesi_*` namespace for what `eth_*` cannot say faithfully.
- [BLOCKS-SPEC](../docs/BLOCKS-SPEC.md) — what it computes. The chain
  parameters, the input envelope, the state transition, the header and its two
  commitments, and the rules for importing a payload.

Neither is prose alone. [`conformance/`](../conformance/README.md) holds the
fixtures both are pinned by, and [`integration/`](../integration/README.md)
drives any engine that listens:

```sh
your-engine &
OP_CARTESI_TEST_ENGINE_URL=http://127.0.0.1:8551 go test ./integration/...
```

A reasonable order to build one in:

1. **Genesis and the header.** `conformance/blocks/genesis.json` and
   `extradata.json` need no machine at all — they are pure functions of the
   chain parameters and a root hash.
2. **The commitments.** `conformance/commitments/` — the outputs tree and the
   withdrawal trie, again with no machine.
3. **Blocks.** `conformance/blocks/block.json` supplies recorded machine
   answers, so the header rules can be finished against a stub.
4. **The wire.** `conformance/engine/sequencing.json` replays a whole
   sequencing run against a running engine.
5. **A real machine**, and then the devnet: take the `verifier-engine` slot
   (`VERIFIER_ENGINE_IMAGE`, [devnet/README.md](../devnet/README.md)) while the
   Go engine sequences. That node derives only from L1 and must reach
   byte-identical blocks, so a divergence is a refused payload rather than a
   broken chain.
