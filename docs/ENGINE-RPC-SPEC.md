# The node's JSON-RPC surface — specification, v1

**Status: draft specification, descriptive of the running chain.**

This document specifies the JSON-RPC an op-cartesi node serves: the
`engine_` methods `op-node` drives it with, the `eth_` subset it answers,
the `cartesi_` namespace for what `eth_` cannot say faithfully, and the one
`miner_` method `op-batcher` requires.

It is the companion of [BLOCKS-SPEC.md](BLOCKS-SPEC.md), which specifies
what a node *computes*. This one specifies what it *says*. Between them they
are what a second implementation — in Rust, in TypeScript, in anything with
decent Ethereum primitives — needs in order to stand where the Go node
stands today.

Most of the surface is standard and specified elsewhere: where a method
behaves exactly as Ethereum's or the OP Stack's own specification says, this
document says so and links, rather than restating. What it does spell out is
every place this chain **deviates**, because those are precisely the places
a second implementation would otherwise get subtly wrong, and the places a
client written against geth will be surprised.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, RECOMMENDED and MAY are to
be interpreted as described in RFC 2119.

## 1. Listeners and authentication

A node serves two HTTP listeners. They differ in exactly one thing: whether
the `engine_` namespace is present, and hence whether the connection is
authenticated.

| | default address | namespaces | auth |
|---|---|---|---|
| **engine** | `127.0.0.1:8551` | `engine_`, `eth_`, `cartesi_`, `miner_` | JWT, when a secret is configured |
| **public** | `127.0.0.1:8545` | `eth_`, `cartesi_`, `miner_` | none |

`engine_` is the only namespace exclusive to the engine port. `eth_` and
`cartesi_` are on both because `op-node` and `op-batcher` read `eth_` over
the authenticated connection while ordinary clients read it over the public
one. `miner_` is on both because `op-batcher` calls
`miner_setMaxDASize` on the sequencer's **L2** endpoint and treats an engine
that does not serve it as a fatal error.

Authentication is the Engine API's own scheme
([specification](https://github.com/ethereum/execution-apis/blob/main/src/engine/authentication.md)):
an HS256 JWT in `Authorization: Bearer <token>`, whose `alg` header MUST be
`HS256` and whose `iat` claim MUST be within **60 seconds** of the server's
clock. A request without a valid token gets HTTP 401 and no JSON-RPC body.

An implementation MAY allow the engine port to run unauthenticated for
development, as the reference implementation does when no secret is given,
but MUST NOT do so by default in any deployment context.

## 2. Conformance profiles

Not every consumer needs every method. An implementation that serves less
than the whole surface should know which capability it is giving up.

| Profile | Methods | Needed by |
|---|---|---|
| **Sequencer** | §4 all, §5.1–§5.3, §7 | `op-node` in sequencer mode, `op-batcher` |
| **Verifier** | §4 all, §5.1–§5.3 | `op-node` following L1 |
| **Proposer** | the verifier profile | `op-proposer` (it reads `op-node`, not the engine) |
| **Wallet** | §5.1–§5.6 | viem, ethers, `cast` |
| **Withdrawals** | the wallet profile plus `eth_getProof` (§5.7) | viem's OP Stack withdrawal actions |
| **Cartesi tooling** | §6 | output proofs, inspect, contract discovery |

The narrowest useful implementation is the **verifier** profile: it is what
the devnet's second node needs, it requires no mempool and no proof
machinery, and a divergence from the Go node shows up immediately as a
refused payload.

## 3. Conventions

- **Encoding** is Ethereum's JSON-RPC convention throughout: quantities are
  minimal-length `0x`-prefixed hex, byte strings are `0x`-prefixed hex of
  even length, addresses and hashes are their fixed-width hex forms.
- **Block identifiers** are a block hash, a hex block number, or one of the
  tags `latest`, `safe`, `finalized`, `earliest`, `pending`. `pending` is an
  alias for the unsafe head: there is no pending-block simulation. Any other
  negative block number is an error.
- **Unknown blocks** are a `null` result for the block-returning methods and
  an error for methods that must read state at a block.
- **Errors** are ordinary JSON-RPC errors. The one structured error is the
  revert of §5.4.
- **Absent objects** — an unknown transaction, an unknown receipt — are
  `null`, not an error, matching geth.

## 4. `engine_` — the Engine API

Served on the engine port only. Semantics are the OP Stack's
([execution engine](https://specs.optimism.io/protocol/exec-engine.html#engine-api));
what follows is the delta.

Only these three methods, and only these versions, are served. `op-node`
selects the version by fork, and this chain is Isthmus from genesis
([BLOCKS-SPEC §4.4](BLOCKS-SPEC.md)), so it never calls the others. An
implementation MUST NOT serve the V1–V3 payload methods.

### 4.1 `engine_forkchoiceUpdatedV3`

Sets the unsafe, safe and finalized heads and, when payload attributes are
supplied, starts building the next block and returns a payload id. Behaviour
is [BLOCKS-SPEC §14](BLOCKS-SPEC.md):

- unknown head → `SYNCING`;
- a non-canonical safe or finalized hash → invalid-forkchoice-state error;
- a zero safe or finalized hash leaves that pointer unchanged;
- attributes whose timestamp is not strictly after the head's →
  invalid-payload-attributes error.

The returned payload id is opaque and need only be stable within one node
([BLOCKS-SPEC §15](BLOCKS-SPEC.md)).

### 4.2 `engine_getPayloadV4`

Returns the envelope built for a payload id, or the standard unknown-payload
error.

The envelope's `blockValue` is zero — nothing is paid for building a block
here — and its blobs bundle is present with three empty arrays. The payload
carries `withdrawals` as an **empty list** and `withdrawalsRoot` as its own
field ([BLOCKS-SPEC §12.1](BLOCKS-SPEC.md)).

### 4.3 `engine_newPayloadV4`

Imports a payload and re-executes it. The full validation order, including
which failures are `INVALID` and which are `SYNCING`, is
[BLOCKS-SPEC §13](BLOCKS-SPEC.md). Three argument-level rules belong here:

- `expectedBlobVersionedHashes` MUST be empty — this chain accepts no blob
  transactions;
- `executionRequests` MUST be empty — this chain produces no EL-triggered
  requests, and `op-node` reconstructs the header with an empty requests
  hash to match;
- `parentBeaconBlockRoot` MUST be present. A missing one is an
  invalid-params error, **not** a silent default to the zero hash, which
  would produce a different block hash.

## 5. `eth_` — the subset that is served

This is a deliberately small surface: what `op-node`, `op-batcher` and
`op-proposer` actually call, plus what ordinary wallets and `cast` need.
Anything not listed here is not served (§8).

### 5.1 Chain and block reads

| Method | Behaviour |
|---|---|
| `eth_chainId` | the configured L2 chain id |
| `eth_blockNumber` | the height of the unsafe head |
| `eth_syncing` | **always `false`.** The node has no sync protocol: it follows `op-node`, or replays from a checkpoint at startup. A node that cannot serve a block says so per-call. |
| `eth_getBlockByHash` | a block, with transaction hashes or full transactions |
| `eth_getBlockByNumber` | the same, by number or tag |

Block objects are geth's, with two requirements a client depends on:

- **`requestsHash` MUST be present.** It is part of the header from Isthmus
  onward and therefore part of the block hash. `op-batcher` reconstructs
  blocks from this JSON to chain them together; omitting the field makes it
  compute a different hash and reject the chain.
- **`withdrawalsRoot` MUST be present**, and `withdrawals` MUST be the empty
  array. The root is the withdrawal trie's
  ([BLOCKS-SPEC §11](BLOCKS-SPEC.md)), not a hash of the empty list.

`stateRoot` is the machine's Merkle root; `receiptsRoot` and `logsBloom` are
always empty ([BLOCKS-SPEC §12.1](BLOCKS-SPEC.md)).

### 5.2 Transactions

**`eth_sendRawTransaction`** is the only way into the chain: the OP Stack
has no public L2 mempool, so the sequencer's RPC is the sole ingress, and
the transaction lands in a bounded FIFO the next block drains. A node
serving no mempool MUST refuse with an error rather than accepting and
dropping.

**`eth_getTransactionByHash`** returns a transaction from the canonical
chain with its block coordinates, from the pool with explicit `null`
coordinates, or `null`. The `from` field MUST be present: geth's plain
transaction marshalling omits it, so an implementation has to recover the
signer (deposits carry theirs).

Serving this is not optional for a chain that wants to work with standard
tooling: viem's `waitForTransactionReceipt` fetches the transaction *before*
the receipt, for its replacement detection, and treats a missing **method**
as fatal — so a chain that serves receipts but not this stalls every
viem-driven script at the first wait.

### 5.3 Receipts

`eth_getTransactionReceipt` and `eth_getBlockReceipts` serve receipts
**synthesized** from what the machine emitted: provable outputs become logs,
acceptance becomes `status`, and mcycles become `gasUsed`
([BLOCKS-SPEC §9.1, §10.1](BLOCKS-SPEC.md)).

Nothing on the OP Stack's critical path reads L2 receipts — derivation
fetches L1 receipts, the batcher reads blocks — so these serve users rather
than the protocol, and the header commits an empty receipts root and bloom
so the encoding is not frozen into consensus while it is still moving. An
implementation MAY therefore differ here, and SHOULD NOT be relied on not
to.

The reference encoding, per transaction:

- `status` is 1 when the machine accepted the input and 0 when it rejected
  it;
- `gasUsed` is that transaction's cycles over `CyclesPerGas`, uncapped
  ([BLOCKS-SPEC §16.5](BLOCKS-SPEC.md));
- `cumulativeGasUsed` accumulates it across the block;
- `effectiveGasPrice` is the chain's constant base fee;
- `contractAddress` is always `null` — there is no contract creation;
- `from`, `to` and `type` come from decoding the transaction.

And per provable output, one log, in emission order:

- **A guest event** — an output that is a `Notice` wrapping
  `EvmLog(address,bytes32[],bytes)` — decodes into a real log with the
  guest's own emitter, topics and data. This is what makes event-driven
  tooling work: viem's `getWithdrawals` matching `MessagePassed`, indexers
  reading `Transfer`.
- **Anything else** becomes a log from the outputs emitter
  `0x4200000000000000000000000000000000000cA1`, with topics
  `[keccak256("CartesiOutput(uint64,bytes)"), bytes32(outputIndex)]` and the
  raw output as data. The chain-wide output index as a topic is what lets a
  reader build an on-chain output proof from the receipt alone.

Either way the index stays available through
`cartesi_getTransactionEmissions` (§6.1).

Reports are **not** logs and MUST NOT be served as logs: a log implies
provability. They are served through `cartesi_` instead.

### 5.4 `eth_call`

A read-only query, answered by running the machine's inspect protocol
against the state at the requested block, on a fork that is discarded
afterwards.

The call travels to the guest as the `EvmCall` envelope
([EVM-COMPAT §7](EVM-COMPAT.md)): the selector of

```
EvmCall(uint256,address,address,uint256,bytes)
```

followed by the ABI encoding of `chainId`, `from`, `to`, `value`, `data`. An
absent `from` is the zero address; an absent value is zero. `to` is
**required** — there is no contract creation to simulate. Gas fields in the
argument object are accepted and ignored: there is no fee market to price a
read.

The guest answers with **tagged reports**: one framing byte before each
report body.

| Tag | Meaning |
|---|---|
| `0x00` | application diagnostic — passed through, not part of the answer |
| `0x01` | `eth_call` return data |
| `0x02` | revert data (`Error(string)` where the guest produces it) |
| `0x03` | a handler failure that **kept** its state changes (EVM-COMPAT §5's FAIL) |

From those:

- **Accepted** → the return value is the concatenation of the bodies of the
  `0x01`-tagged reports. (The guest emits exactly one; the concatenation
  degenerates to it.) `0x00`-tagged diagnostics are not `eth_call`'s to
  return.
- **Rejected** → the **last** report tagged `0x02` or `0x03` supplies the
  error data, and the method MUST fail with the standard Ethereum revert
  error: code **3**, message `execution reverted`, extended to
  `execution reverted: <reason>` when the data decodes as `Error(string)`,
  and the raw revert bytes in the error's `data` field. That is what lets
  viem, ethers and `cast` surface `require`-style messages verbatim.
- **Rejected with no error-tagged report** → a plain error.

`0x02` and `0x03` both revert the call — the answer to "would this succeed"
is no either way — but they are distinguished on the wire because a revert
is safe to treat as "nothing happened" and a fail is not.

`cartesi_inspect` (§6.4) is the raw form of the same mechanism, for guests
and queries that speak their own dialect.

### 5.5 Account state

`eth_getBalance` and `eth_getTransactionCount` are answered out of the
guest's **accounts drive**, read straight from the parked machine's memory
at the requested block — no fork, no execution
([ACCOUNTS.md §6.2](ACCOUNTS.md),
[ACCOUNTS-DRIVE-SPEC §11](ACCOUNTS-DRIVE-SPEC.md)).

The nonce served is the next nonce a wallet must sign with: the guest
enforces and bumps it as part of the state transition, so the drive's record
is authoritative.

A machine with **no accounts drive** — a development mock, or a guest
predating the drive — MUST answer zeros rather than erroring. An RPC
consumer cannot distinguish an empty account from a missing ledger anyway,
and erroring would break every wallet pointed at a mock node.

### 5.6 `eth_getCode`

There is no EVM bytecode on this chain, but `eth_getCode`'s real job for
tooling is the contract/EOA distinction, and wallets and SDKs check exactly
that before calling. So a **routed** address answers a four-byte marker and
everything else answers `0x`:

```
0xEF 0xC7 0x51 <kind>        kind: 0x00 system, 0x01 application, 0x02 token façade
```

`0xEF` because EIP-3541 forbids deployed code starting with it, so no real
EVM contract can collide with the marker.

Which addresses are routed is read off the machine's drives with no
execution: system and application contracts from the ABI drive
([ABI-DRIVE-SPEC](ABI-DRIVE-SPEC.md)), token façades derived from the
accounts drive's registry. A façade's address is

```
last20( keccak256( "ctsi.erc20.v1" ‖ l1Token ) )
```

as the guest derives it ([EVM-COMPAT §6](EVM-COMPAT.md)).

### 5.7 `eth_getProof`

Serves storage proofs for **exactly one address**: the `L2ToL1MessagePasser`
`0x4200000000000000000000000000000000000016`. Any other address MUST be
refused with an error pointing at `cartesi_getAccountProof` (§6.5).

That is the only account this chain maintains a storage trie for, and the
trie is the block's withdrawal commitment
([BLOCKS-SPEC §11](BLOCKS-SPEC.md)). This is the call viem's
`buildProveWithdrawal` makes.

The reply is geth's `eth_getProof` shape, with two fields that are not what
a geth client might assume:

- **`storageHash` is the block's `withdrawalsRoot`** — which is what
  `op-node` publishes as the L2 output root's `messagePasserStorageRoot`,
  so a storage proof from here is exactly what
  `OptimismPortal.proveWithdrawalTransaction` verifies.
- **`accountProof` is empty**, and `balance`, `nonce` and `codeHash` are the
  empty-account values. There is no Ethereum account trie on this chain, so
  the account cannot be proven against `stateRoot` — which nothing on the
  Isthmus withdrawal path needs. A client MUST read `storageHash` and
  `storageProof`, as viem and the portal do, and MUST NOT attempt to verify
  the account against `stateRoot`.

Storage keys shorter than 32 bytes are left-padded, as geth pads them. A
proof for an absent slot is an exclusion proof, as geth serves one.

### 5.8 Fees

There is no fee market ([BLOCKS-SPEC §9.2](BLOCKS-SPEC.md)), so these
methods describe a constant honestly rather than estimating.

| Method | Answer |
|---|---|
| `eth_gasPrice` | the **head header's** base fee plus a zero tip — read from the header rather than the config so the answer always matches what the chain committed to. This is what lets `cast send` run without gas flags. |
| `eth_maxPriorityFeePerGas` | zero: nothing charges or pays priority fees, so a tip would buy no ordering |
| `eth_feeHistory` | geth's exact wire shape, synthesized from headers |
| `eth_estimateGas` | the per-input cycle budget as gas |

**`eth_feeHistory`** returns `blockCount` entries ending at `lastBlock`,
clamped to 1024 and to the blocks the node still holds — after a restart
only the blocks from the newest checkpoint onward are in memory, and the
available suffix is served rather than erroring, as geth serves the
retrievable part of a range. `baseFeePerGas` carries one extra entry for the
block after the window: the next block's header if it exists, otherwise the
configured constant. `gasUsedRatio` is each header's metered mcycles over
its gas limit. `reward` is present only when percentiles are requested and
is a matrix of zeros — with no fee market that is the true tip distribution,
not a stub. A `blockCount` of zero is an empty result, not an error.
Percentiles must be in [0, 100] and strictly increasing.

**`eth_estimateGas`** returns `maxCyclesPerInput / CyclesPerGas` — with the
defaults, 1,000,000 — clamped to the block gas limit and floored at 21,000.
It is an upper bound the chain will accept, which is what a gas limit means
here, **not** a measurement of this call: an input never gets more than that
budget, so a wallet sending the bound can never be cut short. The arguments
are accepted for wire compatibility and ignored; the block tag is resolved,
so an unknown tag errors as it does elsewhere.

A true estimate would run the payload on a discarded fork and report its
cycles, the way `eth_call` runs inspect. But an estimate arrives unsigned,
and the guest recovers the sender and enforces the nonce on every ordinary
transaction, so an unsigned replay would measure the rejection. Measurement
needs a signature-less simulation entry point in the guest; until then this
is the defensible answer.

## 6. `cartesi_` — the machine's own vocabulary

Two things need a namespace of their own. **Reports** are diagnostic and
explicitly not provable, so they must not be dressed up as logs — logs are
what outputs become, and conflating them would suggest reports can be proven
on L1. And an output's **chain-wide index**, and the accumulator it belongs
to, are what an on-chain proof is built from, and no standard receipt field
carries them.

All six methods are read-only and are served on both listeners.

### 6.1 `cartesi_getTransactionEmissions(txHash)`

Everything the machine produced for one transaction, or `null` if it is
unknown or was reorged out: the block coordinates, whether the input was
`accepted`, the `cycles` consumed, the provable `outputs` each with its
chain-wide `index`, and the `reports`.

The index is the `outputIndex` of a Cartesi output validity proof
([BLOCKS-SPEC §10.3](BLOCKS-SPEC.md)). Reports are typically the only
account of why a rejected input failed, which is why they are served for
rejected inputs even though the outputs are not
([BLOCKS-SPEC §10.1](BLOCKS-SPEC.md)).

### 6.2 `cartesi_getOutputsRoot(block)`

The chain-wide outputs commitment as of a block: the block coordinates, the
`root`, and the `count` of outputs the tree holds — which bounds the valid
output indices at that block.

### 6.3 `cartesi_getOutputProof(index, block?)`

Everything needed to execute an output on L1, in one reply:

| Field | What it is |
|---|---|
| `output` | the raw emitted bytes — `executeOutput`'s first argument |
| `outputIndex`, `outputHashesSiblings` | Cartesi's `OutputValidityProof`, named as its ABI names it so the reply passes straight through |
| `outputsMerkleRoot` | the root the proof reproduces |
| `withdrawalsRoot` | the block's committed withdrawal trie root — the `messagePasserStorageRoot` an L1 proposal holds |
| `outputsRootProof` | the storage proof of the reserved outputs-root slot against `withdrawalsRoot`: the RLP trie nodes `SecureMerkleTrie` takes |
| `provenAgainst`, `blockNumber` | the block this proof is against |
| `emittedIn`, `emittedBy` | where the output came from |

The two proofs together are what `OPOutputsMerkleRootValidator.accept` and
`Application.executeOutput` need: the storage proof gets an L1 validator
from a proposal's `withdrawalsRoot` to the outputs root, and the Merkle
proof gets it from there to the output.

The **block argument matters**. The outputs tree is cumulative, so an output
is provable against the commitment of the block that emitted it and of every
block after; a caller executing on L1 wants a proof against a block that has
actually been proposed, which is usually not the emitting one. It therefore
defaults to the **safe head**, since proposals follow the safe chain.

An implementation SHOULD verify a proof it just built against the committed
root before serving it: a proof that does not reproduce the root is its own
bug, not the caller's problem.

### 6.4 `cartesi_inspect(query, block?)`

A read-only query against the machine state at a block, run on a fork that
is discarded. The payload is passed through untouched **both ways**, and the
reports are returned individually rather than concatenated — the raw form of
the mechanism `eth_call` (§5.4) wraps in the `EvmCall` envelope and the
report tags.

The reply carries `accepted`, `cycles` and `reports`. A rejected query is
the inspect analogue of a reverted call.

### 6.5 `cartesi_getAccountProof(address, block?)`

The account record — or its provable **absence** — with machine Merkle
proofs against the block's `stateRoot`. This is the chain's `eth_getProof`
analogue for accounts, and it exists because there is no MPT to prove them
against and never will be.

The reply hands an external verifier everything
[ACCOUNTS-DRIVE-SPEC §12](ACCOUNTS-DRIVE-SPEC.md) asks for:

- the **geometry** echoed from the drive header (seed, capacities, offsets,
  slot sizes, profile) *plus the proven header page itself*, so nothing is
  taken on the node's word;
- the **pages**: `headerPage` and `walkPages`, each one 4 KiB page
  (`log2Size` 12) carried both drive-relative and as an absolute machine
  address, with its raw bytes and its proof of 64 − 12 = 52 siblings against
  `stateRoot`;
- the **walk**: `homeSlot` and `walkLength`, so that re-running the lookup
  inside the proven pages must terminate exactly there — found holding the
  returned record, or absent because the walk ends at an empty slot or an
  early termination, both inside the proven range.

A machine that cannot produce Merkle proofs (a development mock) MUST error
rather than serve proofless "proofs".

### 6.6 `cartesi_getContracts(block?)`

Every address the guest routes as of a block: system built-ins and
application contracts with their recorded standard JSON ABIs, plus the token
façades the accounts drive's registry implies. Each entry carries its
address, a `kind` of `system`, `app` or `token`, the ABI where one is
recorded, and the L1 token a façade serves.

It is read off the parked machine's drives with no execution and no
knowledge of the application's implementation. With the accounts drive
serving balances and this serving interfaces, a node answers "what does this
chain speak?" from drive bytes alone.

A recorded ABI that is not well-formed JSON MUST be omitted rather than
embedded: a drive written by a broken guest must not corrupt the reply.

## 7. `miner_setMaxDASize`

`op-batcher`'s backpressure: when batches back up on L1 it asks the
sequencer to build smaller blocks, passing a maximum transaction size and a
maximum block payload size. Zero means unlimited. The method returns `true`.

The limits apply to mempool transactions only; deposits are forced by
`op-node` and cannot be shed ([BLOCKS-SPEC §8.4](BLOCKS-SPEC.md)).

Serving it is **not optional**: `op-batcher` treats an engine that does not
as a fatal error and shuts down. It is served on both listeners for that
reason.

## 8. What is not served, and why

There are no `debug_`, `txpool_`, `net_` or `web3_` methods, no filters, no
subscriptions, and no log queries. Specifically absent, with the reason:

| Absent | Why |
|---|---|
| `eth_getLogs`, `eth_newFilter` and the filter family, `eth_subscribe` | there is no log index. Logs are synthesized per receipt (§5.3) and uncommitted; an implementation that adds queries is adding an index, not exposing one |
| `eth_getStorageAt` | there is no storage. The one storage trie is the withdrawal commitment, reached through `eth_getProof` |
| `eth_getTransactionByBlock*AndIndex`, `eth_getUncle*` | unused by anything in the stack; blocks carry no uncles |
| `eth_accounts`, `eth_sign`, `eth_sendTransaction` | the node holds no keys |
| `debug_*` | the machine's own vocabulary is `cartesi_` |
| `net_version`, `web3_clientVersion` | not served today. Some older tooling expects `net_version`; an implementation MAY add it, and doing so changes nothing about the chain |

An implementation MAY serve more than this. It MUST NOT serve *less* than
the profile it claims in §2.

## 9. Known underspecification

**9.1 The receipt encoding is described, not fixed.** §5.3 documents what
the reference implementation emits, and the header deliberately does not
commit to it ([BLOCKS-SPEC §15](BLOCKS-SPEC.md)). Two implementations could
differ here and both be conforming, which is fine for now and will not be
once anything depends on it.

**9.2 Error messages are not specified.** Only the revert error (§5.4) has a
required shape — code 3 with data. Everything else is free-form, so a client
MUST NOT match on message text.

**9.3 `eth_syncing` is always false** (§5.1), including while a node is
replaying from a checkpoint at startup and cannot yet serve. The per-call
answer is honest; the aggregate one is not.

**9.4 `eth_getProof`'s storage values are quantities.** The reply encodes a
slot value as a JSON quantity, so the outputs root arrives as a number with
leading zeros stripped. That is geth's own encoding, and a client
reassembling a 32-byte root must left-pad it.

## 10. Conformance vectors

Like [BLOCKS-SPEC §17](BLOCKS-SPEC.md), this document has fixtures rather than
only prose. Two of its sets exist today in [`conformance/`](../conformance),
generated by the node and replayed by both the node and the guest:

| Vector set | Pins | Section |
|---|---|---|
| [`encodings/evmcall.json`](../conformance/encodings/evmcall.json) | the `eth_call` envelope and the report tags | §5.4 |
| [`encodings/evmlog.json`](../conformance/encodings/evmlog.json) | guest events as notices, and what is not one | §5.3 |

The rest are still to be built, in this order of usefulness:

| Vector set | Shape |
|---|---|
| `engine` | recorded request/response pairs for a whole sequencing run, and for each §4.3 rejection mode |
| `eth-block` | a stored block, and its exact `eth_getBlockBy*` JSON in both `fullTx` modes |
| `eth-receipt` | recorded emissions, and the receipts and logs synthesized from them |
| `getproof` | a withdrawal trie, and the `eth_getProof` reply for a sent and an unsent withdrawal |
| `cartesi` | recorded emissions and trees, and each §6 reply |
| `jwt` | tokens that must be accepted and rejected, including `iat` at the drift boundary |

The `engine` set is the one that matters most: driven over authenticated HTTP
it is implementation-agnostic by construction, which is what turns the existing
[`integration`](../integration) suite — today an in-process harness using
op-node's own wire types — into a cross-implementation test. Making that
harness take an endpoint is a prerequisite for a second engine, and can be
done against the Go one first.

## References

- [BLOCKS-SPEC.md](BLOCKS-SPEC.md) — what a node computes.
- [EVM-COMPAT.md](EVM-COMPAT.md) — the guest side of `eth_call`, the report
  tags, routing and the built-in contracts.
- [ACCOUNTS.md](ACCOUNTS.md) · [ACCOUNTS-DRIVE-SPEC.md](ACCOUNTS-DRIVE-SPEC.md)
  — the account model and the drive `eth_getBalance` reads.
- [ABI-DRIVE-SPEC.md](ABI-DRIVE-SPEC.md) — the drive `eth_getCode` and
  `cartesi_getContracts` read.
- [Engine API authentication](https://github.com/ethereum/execution-apis/blob/main/src/engine/authentication.md)
  · [OP Stack execution engine](https://specs.optimism.io/protocol/exec-engine.html#engine-api)
  · [withdrawals](https://specs.optimism.io/protocol/withdrawals.html).
