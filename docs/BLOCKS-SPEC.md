# Blocks and the state transition — format specification, v1

**Status: draft specification, descriptive of the running chain.**

This document specifies how an op-cartesi L2 block is built, executed,
committed to and validated: the parameters that define a chain, the envelope
an input reaches the machine in, the rules that turn a list of transactions
plus a machine into a header, and the two commitments that header carries.

It exists so that the state transition is a *specification* rather than
whatever [`host/go/chain/`](../host/go/chain) happens to do. Two audiences need that:

- **An independent execution engine** — a second implementation, in another
  language, that must produce byte-identical blocks. The wire surface it
  serves is a separate document,
  [ENGINE-RPC-SPEC.md](ENGINE-RPC-SPEC.md); this one is what it must
  compute.
- **A fault-proof or ZK settlement scheme**, which cannot dispute a
  computation nobody has written down. [DESIGN §8](DESIGN.md) is the
  argument; the parts of it that are already fixed are here.

Companion specifications: [ACCOUNTS-DRIVE-SPEC.md](ACCOUNTS-DRIVE-SPEC.md)
and [ABI-DRIVE-SPEC.md](ABI-DRIVE-SPEC.md) for the drives this chain reads
out of machine memory, and [EVM-COMPAT.md](EVM-COMPAT.md) for what the guest
does with an input once it has one.

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, RECOMMENDED and MAY are to
be interpreted as described in RFC 2119.

## 1. Model and terminology

- **Machine** — a Cartesi Machine: a deterministic RISC-V machine whose
  entire address space is committed to a Merkle tree (Keccak-256, 32-byte
  leaves, 64-bit address space). Its root hash is the block's `stateRoot`.
- **Engine** — the process this document specifies: it speaks the Engine
  API to `op-node` on one side and drives a machine on the other. It is
  what `op-geth` would otherwise be.
- **Input** — one unit of work delivered to the machine, after which the
  machine returns to a quiescent state. Every L2 transaction becomes
  exactly one input.
- **Sequencer** and **verifier** are the two roles an engine plays: the
  sequencer *builds* payloads from payload attributes (§8.2), the verifier
  *imports* them and re-executes (§8.3). One engine is usually both.
- **Output** and **report** are the machine's two kinds of emission.
  Outputs are provable and enter commitments; reports are diagnostic and
  MUST NOT enter any commitment. See §10.
- **Accepted** / **rejected** describe how the machine ended an input: an
  accepted input's state changes stand, a rejected input's are rolled back.

## 2. Encoding conventions

- **ABI encoding** means Ethereum's standard contract ABI encoding, and a
  **selector** is the first four bytes of the keccak256 of the canonical
  signature string given here verbatim.
- **keccak256** is Ethereum's Keccak-256 (pre-FIPS padding), the same
  function the machine's Merkle tree uses.
- **RLP**, the **Merkle-Patricia trie** and the **block hash** are
  Ethereum's, unmodified. A block hash is the keccak256 of the RLP encoding
  of the header field list in §12.
- `‖` denotes byte concatenation.
- Addresses are 20 raw bytes; hashes are 32.

## 3. Conformance summary

An implementation conforms if, for every chain configuration (§4) and every
sequence of payload attributes:

1. it computes the same genesis block hash (§6);
2. it feeds the machine byte-identical inputs, in the same order (§7, §8);
3. it computes the same `stateRoot`, `gasUsed`, `withdrawalsRoot` and hence
   the same block hash for every block (§9–§12);
4. it accepts exactly the payloads §13 says are valid, and rejects the rest.

Everything in §15 is explicitly **not** part of that agreement: two
conforming implementations may differ freely there.

## 4. Chain parameters

A chain is defined by the parameters below plus the machine snapshot it
starts from. They split three ways, and the split matters more than the
values: only the first group is covered by the genesis block hash, so only
it is checked when `op-node` compares its rollup config against the engine.

### 4.1 Genesis parameters

These enter the genesis header and therefore the genesis block hash. A node
started with different values serves a different chain, and `op-node`
refuses it.

| Parameter | Type | Default | Where it lands |
|---|---|---|---|
| `genesisTimestamp` | uint64 | 0 | header `timestamp` |
| `gasLimit` | uint64 | 30,000,000 | header `gasLimit` |
| `baseFee` | uint256 | 1,000,000,000 | header `baseFeePerGas`, on **every** block (§9.2) |
| `eip1559Denominator` | uint64 | 250 | header `extraData` |
| `eip1559Elasticity` | uint64 | 6 | header `extraData` |

### 4.2 State-transition parameters

These do not appear in the genesis header, but they change what the machine
computes. Nodes that disagree on them diverge at the first block that
notices — which may be much later than startup, and which no handshake
catches.

| Parameter | Type | Default | Effect |
|---|---|---|---|
| `chainId` | uint64 | 901 | `EvmAdvance.chainId`; the signer sender recovery uses (§7.2) |
| `appContract` | address | zero | `EvmAdvance.appContract` (§7.1) |
| `maxCyclesPerInput` | uint64 | 1,000,000,000 | the per-input cycle budget (§8.1) |
| `CyclesPerGas` | constant | 1000 | mcycles-to-gas ratio (§9.1) |

`CyclesPerGas` is a compile-time constant rather than a parameter. An
implementation MUST use 1000 until this specification says otherwise.

### 4.3 Local parameters

These are node policy. Two nodes of the same chain MAY differ, and no
observable block content depends on them.

| Parameter | Default | Effect |
|---|---|---|
| `maxSnapshots` | 32 | how many recent blocks keep a live machine fork |
| `checkpointInterval` | 100 | blocks between whole-machine checkpoints; 0 disables |
| `checkpointRetention` | 3 | checkpoints kept on disk |
| mempool capacity | 256 | ingress FIFO bound |

### 4.4 The fork schedule is not a parameter

Every OP Stack fork through **Isthmus** is active from genesis, and none of
them is configurable. A new chain has no pre-fork history to preserve, and
Isthmus is not optional: pre-Isthmus, `op-node` derives the L2 output root
by proving the `L2ToL1MessagePasser` *account* against the block's state
root, which cannot work when that root is a Cartesi hash tree rather than an
Ethereum MPT. A pre-Isthmus chain of this kind could never be proposed.

Jovian and later MUST NOT be activated: Jovian adds a minimum-base-fee
header field this version does not define.

An implementation MUST therefore serve `engine_forkchoiceUpdatedV3` and the
**V4** payload methods, and no other versions
([ENGINE-RPC-SPEC §4](ENGINE-RPC-SPEC.md)).

### 4.5 The chain configuration document

Every node of a chain, and the `rollup.json` generator, MUST be given the same
§4.1 and §4.2 values. A mismatch in §4.1 surfaces as a genesis hash op-node
rejects; a mismatch in §4.2 surfaces as a state root divergence at the first
block that notices, and nothing catches it earlier.

They therefore travel as one document, which an implementation SHOULD accept
in this form:

```json
{
    "chainId": 901,
    "genesisTimestamp": 1700000000,
    "gasLimit": 30000000,
    "baseFee": "1000000000",
    "maxCyclesPerInput": 1000000000,
    "appContract": "0x0000000000000000000000000000000000000000",
    "eip1559Denominator": 250,
    "eip1559Elasticity": 6
}
```

`baseFee` is a decimal **string**: it is a uint256, and a JSON number is a
float to most parsers. A field that is absent takes its default from the table
in §4.1 or §4.2; a field that is present is what the document says, so an
explicit `"baseFee": "0"` is zero rather than the default. An implementation
SHOULD reject a key it does not define, since a misspelled consensus parameter
would otherwise be dropped and the node would serve a chain nobody described.

Node policy (§4.3) MUST NOT appear in the document: two nodes of the same
chain may differ there, and carrying it would suggest otherwise.

The reference implementation writes the document with `op-cartesi config`,
reads it with `-chain-config`, and refuses a command line that sets both it
and an individual consensus flag.

## 5. The machine backend

The engine does not boot a machine. It is given one that is **already parked
at its first input-wait yield**, and that machine's root hash is genesis
state (§6). Booting is the snapshot builder's job (`cartesi build`), which
is what makes the guest's own first-boot writes — the accounts and ABI drive
headers — part of genesis rather than of block 1.

An implementation requires exactly these operations of its machine backend:

| Operation | Contract |
|---|---|
| `advance(input, maxCycles)` | feed one input, run to the next input-wait yield, collect emissions; report accepted/rejected and mcycles consumed. Exceeding `maxCycles` is an error, and the instance MUST be discarded. |
| `inspect(query, maxCycles)` | feed one read-only query, collect reports. Read-only in intent only — the guest may write memory — so callers MUST run it on a fork they discard. |
| `readMemory(address, length)` | copy bytes out of a parked machine with no execution. How the drives are read (§ACCOUNTS-DRIVE-SPEC §11). |
| `rootHash()` | the Merkle root of the entire machine state. |
| `fork()` | an independent copy sharing no mutable state; both remain usable. |
| `store(directory)` | write the state somewhere it can be loaded from; the receiver stays usable and unchanged. |
| `close()` | release the instance. |

**Forking is load-bearing, not an optimization.** Every input in §8 runs on
a fork of the pre-input machine, and the fork is dropped unless the input is
accepted. That is what makes "a rejected input has no state effect" true at
the engine level rather than only inside the guest.

The CMIO vocabulary the engine reads, mirroring `cm.h`:

| Constant | Value | Meaning |
|---|---|---|
| automatic yield, reason `tx-output` | 2 | a **provable** emission (voucher or notice) |
| automatic yield, reason `tx-report` | 4 | a **diagnostic** emission |
| manual yield, reason `rx-accepted` | 1 | the input was accepted |
| manual yield, reason `rx-rejected` | 2 | the input was rejected |
| manual yield, reason `tx-exception` | 4 | the guest raised an exception; treated as rejected |

## 6. The genesis block

Genesis has no transactions and no parent. Its header fields are fixed
except for `stateRoot`, which is the parked machine's root hash:

| Field | Value |
|---|---|
| `parentHash` | zero |
| `sha3Uncles` | `EmptyUncleHash` |
| `miner` | zero address |
| `stateRoot` | the machine's root hash |
| `transactionsRoot` | `EmptyTxsHash` |
| `receiptsRoot` | `EmptyReceiptsHash` |
| `logsBloom` | zero |
| `difficulty` | 0 |
| `number` | 0 |
| `gasLimit` | `gasLimit` (§4.1) |
| `gasUsed` | 0 |
| `timestamp` | `genesisTimestamp` (§4.1) |
| `extraData` | the 9-byte Holocene encoding of the chain's default parameters (§12.2) |
| `mixHash` | zero |
| `nonce` | zero |
| `baseFeePerGas` | `baseFee` (§4.1) |
| `withdrawalsRoot` | the genesis withdrawal trie root (§11.4) |
| `blobGasUsed`, `excessBlobGas` | 0 |
| `parentBeaconBlockRoot` | zero |
| `requestsHash` | `EmptyRequestsHash` |

The genesis block hash is therefore a pure function of the §4.1 parameters
and the snapshot's root hash. It is **not** a function of `chainId`: two
chains differing only in chain id have the same genesis hash, and nothing in
the `op-node` handshake catches the difference.

## 7. Input framing — the `EvmAdvance` envelope

### 7.1 The envelope

Each transaction reaches the machine wrapped in Cartesi's `EvmAdvance`
envelope: the selector of

```
EvmAdvance(uint256,address,address,uint256,uint256,uint256,uint256,bytes)
```

followed by the ABI encoding of

| Argument | Value |
|---|---|
| `chainId` | `chainId` (§4.2) |
| `appContract` | `appContract` (§4.2) |
| `msgSender` | the transaction's own sender (§7.2) |
| `blockNumber` | the block being built |
| `blockTimestamp` | the block's timestamp |
| `prevRandao` | the block's `mixHash` |
| `index` | the input's **chain-wide** index (§7.3) |
| `payload` | the raw, unmodified transaction bytes |

This is the encoding Cartesi's guest tools already decode, so a stock
guest-tools rootfs runs unmodified. Only what the guest sees is wrapped —
the block body stays a list of ordinary RLP transactions, which is what lets
stock `op-batcher` and `op-node` handle it.

Every field is derivable from the block header and the chain's parameters,
which is what lets a verifier reconstruct the exact inputs the builder used
without being told them.

### 7.2 `msgSender`

`msgSender` is the transaction's own sender, not a relaying portal:

- for an **OP deposit transaction**, the (aliased) L1 originator the
  transaction carries;
- for an **ordinary signed transaction**, the recovered signer, using the
  signer for `chainId` (§4.2) — *the chain's* id, not the transaction's,
  because a deposit reports chain id 0 and the signer would reject it;
- if the transaction does not decode, or the sender cannot be recovered,
  the fixed **system sender** `0x4200000000000000000000000000000000000cA0`.

The system sender exists so that such an input stays distinguishable from
one genuinely sent by the zero address. It SHOULD NOT occur for a
well-formed block.

### 7.3 Input indices

`index` is **chain-wide and gapless**: the guest sees one continuous input
sequence for the life of the chain, exactly as it would reading an
`InputBox`. The index of a block's first input is the total number of inputs
consumed by all preceding blocks; within a block, indices increase by one
per **included** transaction, in block order.

A transaction excluded from a block consumes no index (§8.2). A transaction
that is included but rejected **does** consume one.

## 8. Executing a block

### 8.1 One input

For each transaction, in order:

1. Fork the current machine.
2. Encode the envelope (§7) with this transaction's offset.
3. Advance the fork with a budget of `maxCyclesPerInput`.
4. On success, record the result (§10.1) and, if the input was **accepted**,
   adopt the fork as the current machine; otherwise discard it.

Exceeding the cycle budget, or the machine halting, is a **hard failure**:
the fork is discarded. What happens next differs between the two roles, and
that is the only place where they legitimately differ.

### 8.2 Building (sequencer)

Payload attributes carry a mandatory list of deposit transactions, which
`op-node` injects; the rest of the block is drawn from the mempool.

- **Deposits are mandatory.** Every deposit in the attributes is included,
  in order, whether the machine accepts it or not. A deposit that fails
  *hard* aborts the whole build — a block that silently dropped a forced
  transaction would not be the block `op-node` asked for.
- **Mempool transactions are discretionary.** One is skipped when the
  block's `gasUsed` has already reached `gasLimit`, or when the
  data-availability budget (§8.4) refuses it. One that rejects or fails hard
  is **excluded from the block** and dropped from the pool.
- The block's transaction list is exactly the transactions that were
  included, in the order they were fed to the machine.

### 8.3 Importing (verifier)

The block's transaction list is given. Every transaction in it is applied,
in order, with no admission decisions of any kind: the sequencer's choices
are already encoded in the list.

A transaction that fails **hard** on import is skipped: it contributes no
outputs, no reports and **no gas**, and the machine carries on with the next
one. Note that §8.2 says a sequencer can never *produce* such a block —
a hard-failing deposit aborts the build, a hard-failing mempool transaction
is excluded — so this branch is unreachable for honestly built blocks. It is
nonetheless part of the transition function, and §16.1 records that as a
hazard rather than a feature.

### 8.4 Data-availability backpressure

`op-batcher` may ask the sequencer to build smaller blocks
(`miner_setMaxDASize`, [ENGINE-RPC-SPEC §7](ENGINE-RPC-SPEC.md)), giving a
maximum single-transaction size and a maximum total transaction payload per
block. Zero means unlimited.

The limits apply to **mempool transactions only**. Deposits are forced by
`op-node` and MUST NOT be shed. A transaction refused by the budget is left
in the pool, not dropped: the batcher is asking for a smaller block, not for
the transaction to be discarded.

This is sequencer policy and has no verifier counterpart — it changes which
blocks get built, never whether a given block is valid.

## 9. Gas

### 9.1 Metering

Gas is machine mcycles, divided:

```
gasUsed(tx)    = cycles(tx) / CyclesPerGas          (integer division)
gasUsed(block) = min( Σ gasUsed(tx), gasLimit )
```

The sum runs over every transaction that contributed a result — which, per
§8.3, excludes hard failures on the import path. The cap is applied on both
paths and is consensus-critical: a verifier computes the capped figure and
compares it to what the payload claims.

Deposits are metered like everything else. There is no intrinsic gas, no
per-byte cost, and nothing is charged to any account by the engine; the
guest debits its own flat fee inside the state
([ACCOUNTS.md §5.7](ACCOUNTS.md)).

### 9.2 There is no fee market

`baseFeePerGas` is the constant from §4.1, stamped on every header. The
Holocene EIP-1559 parameters in `extraData` are recorded because the
protocol requires them to be recorded, and are **not** used to compute
anything: no base fee adjustment is performed. Priority fees do not exist.

## 10. Outputs, reports and the outputs tree

### 10.1 What is recorded per transaction

Executing one input yields a record holding the transaction's index and
hash, whether it was accepted, the mcycles consumed, its **outputs** and its
**reports**.

The split is the Cartesi provability boundary, and it decides what may ever
be committed to:

- **Outputs** — automatic yields with reason `tx-output` (vouchers and
  notices). Provable; they are the raw material of the outputs commitment.
- **Reports** — automatic yields with reason `tx-report`. Diagnostic, and
  explicitly not provable. They MUST NOT enter any Merkle root or header
  field.

Two rules follow:

- **Outputs of a rejected input are dropped.** A rejection rolls the machine
  back, so the emissions never became state.
- **Reports of a rejected input are kept**, because a rejected input's
  report is usually the only account of why it failed.

### 10.2 The tree

Provable outputs accumulate into a fixed-height append-only Merkle tree that
matches Cartesi's on-chain tree exactly, so existing voucher proofs verify
against it unchanged:

- **height 63** (`CanonicalMachine.LOG2_MAX_OUTPUTS`), capacity 2⁶³ leaves;
- **leaf** = `keccak256(output)`, over the raw emitted bytes;
- **parent** = `keccak256(left ‖ right)`;
- the unfilled right side is padded with **zero subtrees**: the level-0
  default is the 32-byte zero hash, and each level's default is the pair
  hash of the level below's default with itself.

The tree is **cumulative over the whole chain**, not per block. Both sides
require it: Cartesi indexes outputs globally, and the OP Stack's
`messagePasserStorageRoot` is the state of all withdrawals up to a block, so
an output must stay provable against the commitment of every later block.

An implementation MAY keep only the frontier (the pending left-hand node at
each level) to carry the accumulator from block to block; proofs need more
(§10.3), but the root does not.

The **empty** tree's root is the root of the all-zero height-63 tree, not
the zero hash. Genesis commits to it (§11.4).

### 10.3 Output proofs

A Cartesi output validity proof for chain-wide index *i* against the tree as
of a block is the co-path: 63 siblings, leaf level first, taking the level's
default node wherever the level is short. Verification folds the leaf with
the siblings and MUST reproduce the tree's root — which is the on-chain
`LibBinaryMerkleTree.merkleRootAfterReplacement` computation.

## 11. The withdrawal commitment

### 11.1 What it is

The header's `withdrawalsRoot` is a **genuine Ethereum storage trie**: the
storage trie `L2ToL1MessagePasser` (`0x4200…0016`) would have if this chain
had that predeploy. It is not derived from a withdrawals list — the
withdrawals list in every payload is empty — and it is not the outputs root
(which it *contains*, §11.3).

Under Isthmus, `op-node` folds this root into the L2 output root as
`messagePasserStorageRoot`, so a storage proof taken from it satisfies
`OptimismPortal.proveWithdrawalTransaction` unchanged, and nothing forks the
portal.

Like the outputs tree, the trie is cumulative over the chain.

### 11.2 Withdrawals

A withdrawal is recognized in a raw machine output by shape, not by
provenance: an output whose bytes are

```
Notice(bytes)                                                    ← selector
  └── Withdrawal(uint256,address,address,uint256,uint256,bytes)  ← selector
        nonce, sender, target, value, gasLimit, data
```

is a withdrawal; anything else — vouchers, other notices, malformed bytes —
is not, and contributes nothing to the trie. A `Notice` rather than a
voucher deliberately: a message finalizable through `OptimismPortal` must
not also be executable by Cartesi's output executor. It still enters the
outputs tree like every provable output, so it can be *proven* both ways and
*executed* only by the portal.

The permissiveness matches the predeploy, where anyone may call
`initiateWithdrawal`: what a withdrawal *pays* is decided on L1 by what the
portal holds, and on L2 by the guest debiting the sender before emitting.

The **withdrawal hash** is `Hashing.hashWithdrawal`: the keccak256 of the
ABI encoding of the six fields. Its slot in the trie is

```
slot = keccak256( withdrawalHash ‖ bytes32(0) )
```

— `sentMessages`' mapping slot, base 0 — holding the value `0x01`, which is
what the portal's inclusion proof verifies. Inserting the same hash twice
writes the same slot to the same value; replay protection lives on L1.

### 11.3 The reserved outputs-root slot

The Cartesi outputs root (§10.2) is stored in the same trie at

```
OUTPUTS_ROOT_SLOT = keccak256("op-cartesi.outputsMerkleRoot")
```

Its value is the outputs root **as of that block**, updated in every block.
One storage proof against `withdrawalsRoot` therefore opens the outputs
root, and Cartesi output validity proofs verify against that — which is how
a single header field serves both the portal's withdrawal proofs and
Cartesi's output proofs. The slot is the hash of a name rather than a small
integer so that colliding with a `sentMessages` slot would require a keccak
collision.

### 11.4 Encoding

Slots are written the way geth writes account storage: the trie **key** is
`keccak256(slot)`, and the **value** is the RLP encoding of the value's
significant bytes — leading zero bytes trimmed. A withdrawal's value is
therefore RLP(`0x01`); the outputs root's is RLP of the root with leading
zeros trimmed.

The genesis trie holds exactly one entry: the outputs-root slot, set to the
empty tree's root (§10.2). It is never an empty trie.

A block's trie is its parent's trie plus that block's withdrawal
insertions, with the outputs-root slot overwritten. The root is the
resulting trie hash.

## 12. Header construction

### 12.1 Fields

For a block built on `parent` from payload attributes:

| Field | Value |
|---|---|
| `parentHash` | `parent`'s hash |
| `sha3Uncles` | `EmptyUncleHash` |
| `miner` | the attributes' suggested fee recipient |
| `stateRoot` | **the machine's root hash after the block's inputs** |
| `transactionsRoot` | `DeriveSha` over the raw transaction bytes as opaque items, or `EmptyTxsHash` for an empty block |
| `receiptsRoot` | `EmptyReceiptsHash` — always (§15) |
| `logsBloom` | zero — always (§15) |
| `difficulty` | 0 |
| `number` | `parent.number + 1` |
| `gasLimit` | the attributes' gas limit if present, else `parent`'s |
| `gasUsed` | §9.1 |
| `timestamp` | the attributes' timestamp |
| `extraData` | §12.2 |
| `mixHash` | the attributes' `prevRandao` |
| `nonce` | zero |
| `baseFeePerGas` | `baseFee` (§4.1) |
| `withdrawalsRoot` | **the withdrawal trie root** (§11) |
| `blobGasUsed`, `excessBlobGas` | 0 |
| `parentBeaconBlockRoot` | the attributes' beacon root, or zero if absent |
| `requestsHash` | `EmptyRequestsHash` |

Note that `transactionsRoot` hashes the transaction bytes as **opaque
items**, which is what `op-node` does; it does not re-encode them.

The corresponding execution payload carries `withdrawals` as an **empty
list** and `withdrawalsRoot` as its own field, rather than deriving one from
the other. `op-node`'s `ExecutionPayloadEnvelope.CheckBlockHash`
independently reconstructs this header, so any deviation is caught there
rather than at a proposal.

### 12.2 `extraData`

Holocene is active from genesis, so `op-node` supplies the EIP-1559
parameters in the payload attributes as 8 bytes: a big-endian `uint32`
denominator followed by a big-endian `uint32` elasticity. **Zeroed
parameters mean "use the chain defaults"**, and a pair with exactly one zero
is invalid.

`extraData` is the 9-byte Holocene header encoding of the resolved pair:

```
0x00 ‖ uint32be(denominator) ‖ uint32be(elasticity)
```

With the §4.1 defaults that is `0x00 000000fa 00000006`.

## 13. Importing a payload

`engine_newPayloadV4` MUST perform these checks, and MUST NOT skip any of
them on the grounds that a well-behaved peer would not fail them. The order
matters only in that the cheap structural checks come first.

**Structural** — failing any of these makes the payload `INVALID` with no
latest-valid-hash:

1. `logsBloom` is 256 bytes.
2. `withdrawals` is present and **empty**.
3. `baseFeePerGas` is present.
4. `withdrawalsRoot` is present.
5. `extraData` is a valid Holocene encoding with both parameters nonzero.
6. `parentBeaconBlockRoot` is present. A missing one is rejected rather than
   defaulted to zero, because defaulting would yield a different block hash.
7. `versionedHashes` is empty — there are no blob transactions on this
   chain — and `executionRequests` is empty.
8. The header reconstructed from the payload hashes to the payload's
   `blockHash`.

**Contextual**:

9. A payload for a block already known is `VALID` immediately.
10. An unknown parent is `SYNCING`, not `INVALID`.
11. `number` is `parent.number + 1`, and `timestamp` is strictly greater
    than the parent's; otherwise `INVALID`.

**Execution** — if the payload is one this node just built, its already-
computed machine and records are adopted and no re-execution happens.
Otherwise, re-execute per §8.3 and compare:

12. the resulting machine root hash against the payload's `stateRoot`;
13. the capped gas figure (§9.1) against the payload's `gasUsed`;
14. the withdrawal trie root re-derived from the outputs this node just
    observed (§11) against the payload's `withdrawalsRoot`.

Check 14 is what keeps the whole proof story honest: since the trie root
covers both the withdrawal set and the outputs root it embeds, a payload
claiming withdrawals or outputs the machine did not produce is rejected
here. Nothing else re-derives them.

A node that cannot re-execute because the parent's machine snapshot has
fallen out of its retention window MUST answer `SYNCING` rather than
guessing. Its retention window is local policy (§4.3); its honesty about it
is not.

## 14. Forkchoice, reorgs and retention

`engine_forkchoiceUpdatedV3` makes a known block canonical, records the safe
and finalized heads, and optionally starts building on the new head.

- An unknown head is `SYNCING`.
- A safe or finalized hash that is not canonical under the new head is an
  invalid forkchoice state, an error rather than a status.
- A zero safe or finalized hash leaves that pointer unchanged.
- With payload attributes whose timestamp is not strictly greater than the
  head's, the call is an invalid-payload-attributes error.

A reorg is honoured by rewinding to the machine snapshot of the new head's
ancestor and building forward; it therefore succeeds only within the
snapshot retention window (§4.3). Outside it, the node is stuck rather than
wrong: it MUST refuse rather than build on a state it cannot reproduce.

## 15. Not part of this specification

Two conforming implementations may differ completely in all of the
following, and an implementation SHOULD NOT treat any of it as an
interoperability surface:

- **The store.** Block and record persistence, its on-disk format, machine
  checkpointing, and how a restart replays. The reference implementation
  uses go-ethereum's `rawdb` over pebble plus whole-machine `cm_store`
  checkpoints; none of that is observable.
- **Receipts and logs.** The header commits an empty `receiptsRoot` and an
  empty bloom precisely so that the receipt encoding is *not* frozen into
  consensus while it is still moving. Receipt synthesis is specified for the
  wire in [ENGINE-RPC-SPEC §5](ENGINE-RPC-SPEC.md), and remains changeable.
- **Payload identifiers.** `engine_getPayloadV4` round-trips whatever
  `engine_forkchoiceUpdatedV3` returned; the bytes need only be stable
  within one node.
- **Mempool policy.** Capacity, ordering, replacement and the nonce gate at
  ingress are ingress courtesy and a DoS bound. The guest is the
  authoritative nonce enforcer, inside the state the root commits to.
- **Snapshot and checkpoint retention** (§4.3).
- **Machine transport.** Whether the machine is in-process, over the
  emulator's JSON-RPC protocol, or a deterministic mock.

## 16. Known underspecification

This section is the honest list: places where the reference implementation
has made a choice that this specification records but does not yet justify,
or where two implementations could diverge while both looking correct. Each
is a candidate for a conformance vector (§17) or a fix.

**16.1 The verifier tolerates a block no builder can make.** §8.3 skips a
hard-failing transaction; §8.2 can never produce one. The branch is
unreachable in practice and unproven in principle, and it is exactly the
kind of asymmetry a fault proof would have to adjudicate. Either the
verifier should reject such a block, or the rule should be stated as
intentional.

**16.2 Import does not check `baseFeePerGas` or `gasLimit`.** §13 validates
the state root, the gas used and the withdrawal commitment, but accepts
whatever base fee and gas limit the payload carries. Since the sequencer
stamps a constant base fee (§9.2), a payload with any other base fee is a
block this chain would never build, and the verifier admits it.

**16.3 Per-transaction gas can exceed the block's.** The block's `gasUsed`
is capped at the gas limit (§9.1); the per-transaction figures reported in
receipts are not, so their sum can exceed the header's. Receipts are
uncommitted (§15), so this is a reporting inconsistency rather than a
consensus one.

**16.4 The outputs root is computed outside the machine.** The engine
computes the commitment in host code and cross-checks it against the root
the guest reports where the guest maintains one. A referee cannot dispute a
value that is not in the proven state; moving it inside the machine is
[roadmap step 4](../README.md#roadmap), and it will change this
specification when it happens.

## 17. Conformance vectors

This specification is not testable as prose, so it has a companion corpus:
[`conformance/`](../conformance), fixtures both implementations read, in the
shape [`lib/accounts-drive/testdata/golden.json`](../lib/accounts-drive/testdata/golden.json)
already has for the drives.

| Vector set | Pins | Section |
|---|---|---|
| [`blocks/genesis.json`](../conformance/blocks/genesis.json) | the genesis header, its RLP and its hash | §6 |
| [`blocks/extradata.json`](../conformance/blocks/extradata.json) | the 9 `extraData` bytes, valid and invalid | §12.2 |
| [`encodings/evmadvance.json`](../conformance/encodings/evmadvance.json) | the input envelope, `msgSender` recovery, chain-wide indices | §7 |
| [`encodings/withdrawal.json`](../conformance/encodings/withdrawal.json) | the withdrawal notice, its hash and its slot | §11.2 |
| [`commitments/outputs-tree.json`](../conformance/commitments/outputs-tree.json) | leaves, roots and proofs | §10 |
| [`commitments/passer-trie.json`](../conformance/commitments/passer-trie.json) | trie roots block by block, and storage proofs | §11 |
| [`blocks/block.json`](../conformance/blocks/block.json) | whole blocks: admission, metering, commitments, the header | §8–§12 |
| [`blocks/import.json`](../conformance/blocks/import.json) | the verdict, one payload per validation rule | §13 |

The block and import sets carry **recorded machine responses** — accepted or
rejected, cycles, emissions, the post-state root — consumed one per attempted
input in call order. An implementation replays them against a stub built from
that list, so a vector pins the header rules without needing an emulator;
[`conformance/README.md`](../conformance/README.md) gives the stub's exact
contract. Vectors that do need a real machine belong with the existing
snapshot-gated tests instead.

Two things this corpus deliberately does not fix: error message text (only the
status in `import.json` is normative, per
[ENGINE-RPC-SPEC §9.2](ENGINE-RPC-SPEC.md)), and anything in §15.

## References

- [DESIGN.md](DESIGN.md) — the architecture, and §8 on what settlement
  requires of the definition in this document.
- [ENGINE-RPC-SPEC.md](ENGINE-RPC-SPEC.md) — the wire surface an engine
  serves.
- [EVM-COMPAT.md](EVM-COMPAT.md) — what the guest does with an input.
- [ACCOUNTS-DRIVE-SPEC.md](ACCOUNTS-DRIVE-SPEC.md) ·
  [ABI-DRIVE-SPEC.md](ABI-DRIVE-SPEC.md) — the drives read out of machine
  memory.
- OP Stack specs: [execution engine](https://specs.optimism.io/protocol/exec-engine.html),
  [deposits](https://specs.optimism.io/protocol/deposits.html),
  [withdrawals](https://specs.optimism.io/protocol/withdrawals.html),
  [Holocene EIP-1559 parameters](https://specs.optimism.io/protocol/holocene/exec-engine.html#eip-1559-parameters-in-block-header),
  [Isthmus `L2ToL1MessagePasser` storage root](https://specs.optimism.io/protocol/isthmus/exec-engine.html#l2tol1messagepasser-storage-root-in-header),
  [output commitment construction](https://specs.optimism.io/protocol/proposals.html#l2-output-commitment-construction).
- Cartesi: `CanonicalMachine`, `LibOutputValidityProof` and
  `Application.executeOutput` in
  [rollups-contracts](https://github.com/cartesi/rollups-contracts).
