# Conformance vectors

Fixtures that pin the contracts in [`docs/BLOCKS-SPEC.md`](../docs/BLOCKS-SPEC.md)
and [`docs/ENGINE-RPC-SPEC.md`](../docs/ENGINE-RPC-SPEC.md) as **files both
sides read**, rather than as constants copied from one implementation into
another's tests.

That is the same discipline
[`accounts-drive/testdata/golden.json`](../accounts-drive/testdata/golden.json)
already applies to the drives, where six languages replay one file. Here it
applies to the chain: the node computes these values in Go, the guest computes
several of them in TypeScript, and the L1 contracts verify two of them in
Solidity. Before these files existed, each pair was pinned by a hex string
pasted from one side into the other — which works until there is a third
implementation, and hides which side is the reference.

## What is here

| File | Pins | Spec |
|---|---|---|
| `blocks/genesis.json` | the genesis header, its RLP and its hash, from the chain parameters and a snapshot's root hash | BLOCKS §6 |
| `blocks/extradata.json` | the 9 header `extraData` bytes from the Holocene parameters, valid and invalid | BLOCKS §12.2 |
| `blocks/block.json` | whole blocks: admission, metering, the commitments and the header, driven the way op-node drives them | BLOCKS §8–§12 |
| `blocks/import.json` | `engine_newPayloadV4`'s verdict, one payload per validation rule | BLOCKS §13 |
| `encodings/evmadvance.json` | the input envelope, including `msgSender` recovery and chain-wide indices | BLOCKS §7 |
| `encodings/withdrawal.json` | the withdrawal notice, its hash and its `sentMessages` slot | BLOCKS §11.2 |
| `encodings/evmlog.json` | guest events as notices, and what is *not* one | ENGINE-RPC §5.3 |
| `encodings/evmcall.json` | the `eth_call` envelope and the report tags | ENGINE-RPC §5.4 |
| `commitments/outputs-tree.json` | the height-63 outputs tree: leaves, roots and proofs | BLOCKS §10.2 |
| `commitments/passer-trie.json` | the withdrawal trie: roots block by block, and storage proofs | BLOCKS §11 |

## Who reads them

- **The node (Go)** — [`chain/conformance_test.go`](../chain/conformance_test.go)
  generates every file and replays every one of them.
- **The guest (TypeScript)** — [`evm-compat/js/test/conformance.test.ts`](../evm-compat/js/test/conformance.test.ts)
  replays the four sets `@op-cartesi/evm` implements: withdrawals, events,
  the call envelope, and output leaf hashing.
- **The contracts (Solidity)** — [`contracts/test/Vectors.sol`](../contracts/test/Vectors.sol)
  loads them with `vm.parseJson`, and the three suites that used to carry pasted
  constants read it: `OutputTree.t.sol`, `PasserTrieVectors.t.sol` and
  `CrossDomainVectors.t.sol`. That side is the interesting one, because it runs
  the proofs in these files through the code that will actually judge them on
  L1 — Cartesi's `LibOutputValidityProof` and OP's `SecureMerkleTrie`.

  `vm.parseJson` cannot select an array element by field value, so cases are
  addressed by index and every accessor asserts the `id` at that index.
  Reordering a file fails loudly instead of silently testing a different case.
- **A second implementation** — this is what the files are for. Nothing in
  them is Go-shaped: integers are JSON numbers or decimal strings, byte
  strings are `0x` hex, and the machine is a script rather than an emulator.

## Regenerating

```sh
go test ./chain -run TestConformance -update
```

Then **read the diff**. A vector file is only as good as the review of the
commit that changes it.

Generation is not a rubber stamp, though. Before anything is written:

- every outputs-tree root is checked against the slow, level-by-level builder,
  and every output proof must fold back to the root it claims;
- every storage proof is verified with **geth's own trie verifier** — the
  algorithm `SecureMerkleTrie` implements in Solidity — including the
  exclusion proofs;
- every header is hashed from the fields the file records, not from the
  object that produced them;
- and the constants that predate these files are asserted: the withdrawal
  payload captured from the TypeScript guest, and the five roots and hashes the
  Solidity suites used to hardcode. A change that moves any of them fails
  generation instead of quietly rewriting the file.

## The scripted machine

`blocks/block.json` and `blocks/import.json` carry `machineResponses`: a list
of `{accepted, cycles, postRoot, outputs, fail}` consumed **one per attempted
input, in call order** — not per fork, which is what lets a vector describe a
build that forks per input and drops the forks it rejects. An implementation
replays a case by standing up a stub machine over that list:

- `advance` pops the next response; `fail` of `cycleLimit` or `halted` is an
  error and the instance is discarded;
- an **accepted** response moves the instance's root hash to `postRoot`; a
  rejected one leaves it, because a rejection rolls the machine back;
- `fork` copies the current root and shares the response cursor.

That is enough to pin every header rule without an emulator. Vectors that need
a real machine do not belong here — they are the snapshot-gated tests in
`chain/` and `machine/`.

## What is deliberately not pinned

- **Error messages.** `blocks/import.json` records the status and names the
  rule; the message text is unspecified (ENGINE-RPC §9.2) and a replay must
  not match on it.
- **Receipts.** The header commits an empty receipts root precisely so the
  encoding stays changeable (BLOCKS §15); `encodings/evmlog.json` pins the
  guest's event *encoding*, not the receipt built from it.
- **Payload identifiers, the store, checkpoints, mempool policy** — BLOCKS §15
  in full.
- **The `engine_` argument rules of BLOCKS §13.6–§13.7** (an absent
  `parentBeaconBlockRoot`, non-empty blob hashes or execution requests). They
  are enforced a layer above the state transition, in the RPC service, and
  belong with the engine transcripts below.

## Still missing

The two sets [ENGINE-RPC §10](../docs/ENGINE-RPC-SPEC.md) asks for that are not
here yet:

- **`engine` transcripts** — recorded request/response pairs for a whole
  sequencing run. Driven over authenticated HTTP they are implementation-agnostic
  by construction, which is what would turn [`integration/`](../integration) —
  today an in-process Go harness using op-node's own wire types — into a
  cross-implementation test. Making that harness take an endpoint is the
  prerequisite for a second engine.
- **`crossdomain`** — the messenger encodings, the last pasted constant in the
  Solidity suite (`CrossDomainVectors.t.sol`'s v1 message hash). Unlike everything above, the
  reference for those is the *guest*, so the generator would have to be the
  TypeScript side. `encodings/withdrawal.json` already carries the relayMessage
  payload as opaque withdrawal data, which is the half that reaches consensus.
