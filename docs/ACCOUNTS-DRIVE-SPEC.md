# Accounts Drive Format, version 1

**Status: draft specification.**

This document specifies a byte-level format for an *accounts drive*: a
flash drive (or other fixed memory range) inside a Cartesi Machine in
which the guest maintains per-account state — nonces and balances — laid
out so that a process outside the machine can find, read and prove any
record without executing the guest.

It is written for implementers on all three sides of the drive:

- **Guest** — the program inside the machine that owns the drive and is
  its only writer;
- **Host** — any process that reads the drive from outside: a node
  serving RPC, an indexer, an off-chain prover reading a stored machine;
- **Verifier** — any party checking a record against a machine Merkle
  root hash: an L1 contract, a light client, a dispute referee.

The format is independent of any particular chain or rollup stack. It is
usable wherever a Cartesi Machine's state is the source of truth: a
Cartesi Rollups application, an emergency-withdrawal scheme, or a chain
whose execution layer is a machine. Rationale for the design choices is
deliberately not here; consumers of this spec in a particular system
should document their own deployment choices (see §4).

The key words MUST, MUST NOT, SHOULD, SHOULD NOT, RECOMMENDED and MAY
are to be interpreted as described in RFC 2119.

## 1. Model and terminology

- **Machine** — a Cartesi Machine: a deterministic RISC-V machine whose
  entire address space is committed to a Merkle tree (Keccak-256,
  32-byte leaves, 64-bit address space).
- **Drive** — one memory range of the machine, dedicated to this format.
- **Quiescent state** — machine state at a point where the machine is
  not executing: parked at a yield, or stored to disk. All host reads
  and all proofs refer to a quiescent state.
- **Input** — one unit of work delivered to the guest, after which the
  machine reaches a quiescent state. In Cartesi Rollups terms, one
  advance-state request; **rejecting** an input means ending it such
  that the machine reverts to its state before the input (rx-rejected).
- **Record** — one fixed-size entry in a table.
- **Slot** — one record-sized position in a table, occupied or empty.
- **Application** — the semantics layered on top of this format: what a
  nonce means, which asset a balance denominates, who may cause which
  state change. Out of scope here except where marked.

## 2. Encoding conventions

- **Integers in the header, and nonces**, are **little-endian**.
- **Balances are big-endian**, at their declared width (§7): a 32-byte
  balance is byte-identical to an EVM ABI `uint256` word.
- **Addresses** are 20 raw bytes, as on Ethereum.
- **keccak256** is Ethereum's Keccak-256 (the pre-FIPS padding), the
  same function the machine's Merkle tree uses.
- **Padding and reserved bytes MUST be zero.** A guest MUST write them
  as zero; a host or verifier MAY treat a nonzero padding or reserved
  byte as a malformed drive.
- `LE64(b)` denotes the little-endian interpretation of 8 bytes as an
  unsigned 64-bit integer. `a ‖ b` denotes byte concatenation.

## 3. Conformance summary

A conforming **guest**:
1. maintains every structure exactly as specified in §5–§9, using the
   algorithms of §6 verbatim;
2. keeps the drive bytes device-current at every quiescent state (§10);
3. makes every state change a deterministic function of the input
   sequence, including every rejection this spec mandates.

A conforming **host** reads only quiescent state (§11) and interprets
bytes exactly as specified.

A conforming **verifier** checks records with the algorithms of §12
against a machine root hash it trusts for external reasons.

## 4. The drive

- The drive MUST have a power-of-two length and a start address
  naturally aligned to that length. (This is what makes the drive a
  single subtree of the machine's Merkle tree, with one hash as its
  root — see §12.)
- The drive's start address, length, and every header geometry field
  (§5) are **deployment constants**: fixed when the machine template is
  built, immutable for the lifetime of a deployment, and part of what
  the template's root hash commits to.
- The drive SHOULD be declared with the label `accounts` where the
  machine configuration supports labels, so hosts can discover it via
  the machine's configuration (`machine.get_initial_config` /
  `machine.get_address_ranges`) rather than by convention alone.
- The drive SHOULD NOT be configured with a shared backing store
  (`shared = false`). Rationale, non-normative: a `MAP_SHARED` backing
  file is written by *every* fork of the machine, including forks that
  are executed and discarded; the file then corresponds to no machine
  state. Hosts read through §11's paths instead.

## 5. The header page

The first 4096 bytes of the drive. All integers little-endian.

| Offset | Size | Field | Constraints |
|---|---|---|---|
| 0x00 | 8 | magic: ASCII `ctsiacct` (`63 74 73 69 61 63 63 74`) | required |
| 0x08 | 4 | version | 1 |
| 0x0c | 4 | flags | 0 in version 1 |
| 0x10 | 8 | `capacity` — slot count of the accounts table | ≥ 2 |
| 0x18 | 8 | `tableOffset` — byte offset of accounts slot 0 from drive start | multiple of `slotSize`; ≥ 4096 |
| 0x20 | 8 | `loadLimit` — max occupied accounts slots | 1 ≤ loadLimit ≤ capacity − 1 |
| 0x28 | 8 | `liveCount` — occupied accounts slots | maintained exactly (§6.5) |
| 0x30 | 32 | `seed` — hash key (§6.1) | any value; fixed at deployment |
| 0x50 | 4 | `profile` — 0 single-asset, 1 wide, 2 sparse | |
| 0x54 | 4 | `slotSize` — accounts-table slot size in bytes | 64 in profiles 0 and 2; a power of two ≥ 128 in profile 1 |
| 0x58 | 8 | `registryOffset` | profiles 1–2; multiple of 32; may point into the header page tail (see geometry rules) |
| 0x60 | 8 | `registryCapacity` — max token entries | profiles 1–2; ≤ 65536 |
| 0x68 | 8 | `tokenCount` — registered tokens | ≤ registryCapacity |
| 0x70 | 8 | `sparseOffset` | profile 2; multiple of `sparseSlotSize` |
| 0x78 | 8 | `sparseCapacity` — slot count of the sparse table | profile 2; ≥ 2 |
| 0x80 | 8 | `sparseLoadLimit` | profile 2; 1 ≤ … ≤ sparseCapacity − 1 |
| 0x88 | 8 | `sparseLiveCount` | maintained exactly |
| 0x90 | 4 | `sparseSlotSize` | 64, or 32 iff every registered width is 8 (§9) |
| 0x94 | — | reserved | zero — up to `registryOffset`, when the registry is in-header |

Geometry rules:

- Every region (registry, accounts table, sparse table) MUST lie wholly
  within the drive and MUST NOT overlap another region or the header
  page — with one exception: **the registry MAY occupy the tail of the
  header page**, with `registryOffset ≥ 0x100` and
  `registryOffset + 32·registryCapacity ≤ 4096` (up to 120 entries at
  offset 0x100). This placement is RECOMMENDED whenever the capacity
  fits: the registry then travels with the header in a single cached
  read, and one header-page proof covers both (§12). A registry in its
  own region is for sparse deployments whose token ceiling exceeds the
  page.
- Fields for profiles the drive does not use MUST be zero.
- `loadLimit` (and `sparseLoadLimit`) MUST be strictly less than the
  table's capacity, so a table always retains at least one empty slot.
  RECOMMENDED: at most ⅞ of capacity.
- All geometry fields — everything except `liveCount`, `tokenCount` and
  `sparseLiveCount` — are constants after deployment. The guest MUST
  NOT change them.

## 6. Tables: hashing, probing, and the four operations

Both hash tables (accounts, §7; sparse, §9) use the same open-addressing
scheme with robin-hood linear probing. This section is normative for
both; `C` is the table's capacity, `S` its slot size, `base` its byte
offset from drive start. Slot `i` occupies drive bytes
`[base + S·i, base + S·(i+1))`. An **empty** slot is all-zero; a record
is **live** iff its address field is nonzero (the zero address is
invalid in every record type).

### 6.1 Home slot and displacement

```
home(key)          = LE64(keccak256(seed ‖ key)[0..8]) mod C
displacement(r@i)  = (i − home(key(r))) mod C
```

`key` is the table's key encoding: the 20-byte address (accounts
table), or the 2-byte little-endian token id followed by the 20-byte
address, 22 bytes total (sparse table).

### 6.2 Lookup

```
d := 0
loop:
  s := (home(k) + d) mod C
  if slot s is empty:                     return ABSENT
  if key(slot s) == k:                    return FOUND at s
  if displacement(record at s) < d:       return ABSENT
  d := d + 1                              (d < C always terminates)
```

### 6.3 Insert

Insert is only for keys not present (updates to an existing record are
in-place writes at its slot, found by lookup). Before inserting, the
guest MUST check the table's live count: if it equals the load limit,
the insert MUST NOT happen and the guest MUST **reject the input** that
required it.

```
carry := new record;  d := 0
loop:
  s := (home(key(carry)) + d) mod C
  if slot s is empty:
      write carry to s;  done
  if displacement(record at s) < d:
      swap carry and the record at s
      d := displacement(carry)            (its distance from its own home at s)
  d := d + 1
```

The strict `<` makes chains first-come-first-placed among equal
displacements; implementations MUST NOT vary this.

### 6.4 Delete (backward shift)

Deletion policy — *when* a record may be deleted — is the application's
(§7 and §8 constrain specific cases). The mechanism is:

```
find slot i of the record (lookup)
loop:
  j := (i + 1) mod C
  if slot j is empty, or displacement(record at j) == 0:
      zero slot i;  done
  move record at j to slot i (unchanged bytes);  i := j
```

There are no tombstones; after deletion, empty slots are all-zero.

### 6.5 Counters

`liveCount` (and `sparseLiveCount`) MUST equal the number of live slots
in the table at every quiescent state: incremented by a completed
insert, decremented by a completed delete.

### 6.6 Derived guarantees (for hosts and verifiers)

A table maintained by §6.2–6.4 satisfies, at every quiescent state:

- **No duplicates**: at most one live record per key.
- **Lookup soundness**: if the walk of §6.2 reaches an empty slot, or a
  record whose displacement is smaller than the current distance, the
  key is not present anywhere in the table.
- **Contiguity**: the walk for key `k` touches only slots
  `home(k) … home(k)+d` (cyclic), a single contiguous byte range (two
  ranges when it wraps the table end).

These are what make the proof procedure of §12 sound.

## 7. The accounts table

`capacity` slots of `slotSize` bytes at `tableOffset`. Record, first 64
bytes of a slot:

| Offset | Size | Field |
|---|---|---|
| 0 | 20 | account address (nonzero) |
| 20 | 4 | zero padding |
| 24 | 8 | nonce, `uint64` little-endian |
| 32 | 32 | balance, `uint256` big-endian |

- In **profile 0** the balance denominates the application's single
  asset — the native asset or a single token; which one is application
  semantics, and SHOULD be declared as registry entry 0 (§8) with the
  zero address standing for the native asset.
- In **profiles 1 and 2** the balance denominates the native asset; an
  application without one keeps it zero.
- An application that uses the nonce for transaction replay protection
  MUST NOT delete a record whose nonce is nonzero: deleting it would
  re-enable replay of every past transaction of that account. (Balances
  are unconstrained here — an application MAY delete a zero-nonce
  record whose balances are zero or below an application-defined
  minimum.)
- In **profile 1 (wide)**, token balance columns follow at offset 64:
  the column for token id `i` is `width_i` bytes, big-endian, at offset
  `64 + Σ_{j<i} width_j`. Columns are packed in id order with no gaps;
  bytes past the last column up to `slotSize` MUST be zero. The sum of
  all registered widths MUST NOT exceed `slotSize − 64`; a registration
  that would exceed it MUST be refused (by rejecting the input that
  carries it).

## 8. The token registry (profiles 1 and 2)

An append-only array at `registryOffset` of 32-byte entries; the id of
a token is its index, starting at 0. The array lives in the tail of the
header page when its capacity fits there, and in its own region
otherwise (§5, geometry rules); the entry format and every rule below
are identical in both placements:

| Offset | Size | Field |
|---|---|---|
| 0 | 20 | token address |
| 20 | 1 | balance width in bytes: 8, 16 or 32 |
| 21 | 11 | zero padding (available to applications for declared policy, e.g. a minimum balance; zero if unused) |

- Entries are appended only; ids are never reused, entries are never
  modified or removed. `tokenCount` MUST equal the number of entries.
- A token address MUST appear at most once.
- *How* tokens come to be registered — by a privileged input, or
  automatically on first deposit — is application policy, but it MUST
  be a deterministic function of the input sequence.
- A registration when `tokenCount == registryCapacity` MUST be refused
  by rejecting the input.
- **Width semantics**: a credit that would make a balance exceed the
  largest value representable in the token's declared width MUST be
  refused by rejecting the input. (Choosing a width no honest supply
  can overflow is the registrar's responsibility.)

## 9. The sparse balance table (profile 2)

`sparseCapacity` slots of `sparseSlotSize` bytes at `sparseOffset`,
keyed by token id ‖ address (§6.1), holding one record per **nonzero**
balance. The native asset does not appear here (§7).

`sparseSlotSize = 64`:

| Offset | Size | Field |
|---|---|---|
| 0 | 2 | token id, `uint16` little-endian |
| 2 | 2 | zero padding |
| 4 | 20 | account address (nonzero) |
| 24 | 8 | zero padding |
| 32 | 32 | balance, `uint256` big-endian, nonzero |

`sparseSlotSize = 32` — permitted only when every registered token
declares width 8:

| Offset | Size | Field |
|---|---|---|
| 0 | 2 | token id, `uint16` little-endian |
| 2 | 2 | zero padding |
| 4 | 20 | account address (nonzero) |
| 24 | 8 | balance, `uint64` big-endian, nonzero |

A token id in a record MUST be less than `tokenCount`. A balance
reaching zero MUST delete the record (§6.4). Records with a zero
balance are invalid.

## 10. Guest write discipline

- The drive's bytes MUST be current in machine state at every quiescent
  state. On a pmem-backed flash drive, guest I/O goes through the
  kernel page cache, so the guest MUST flush before finishing each
  input: `msync(MS_SYNC)` on the dirtied ranges of a memory-mapped
  device, or `fsync` on the device file descriptor. On a memory range
  the guest maps without a page cache (e.g. a Cartesi NVRAM/UIO range),
  no flush step exists and stores are current immediately.
- All ordering above is per input: within one input the intermediate
  drive state is unobservable, and an input the guest rejects reverts
  the drive with the rest of the machine — the guest need not undo
  anything itself.
- Every write, and every refusal this spec mandates, MUST be a
  deterministic function of the input sequence: no dependence on
  wall-clock time, host randomness, memory addresses, or iteration
  order of any non-deterministic container.

## 11. Host read discipline

A host MUST take the drive's bytes only from a quiescent state:

1. a machine parked at a yield, read via the machine API
   (`machine.read_memory`), which never executes the machine; or
2. a stored machine image, whose drive image file is quiescent by
   construction (its location is given by the stored machine's
   configuration); or
3. a byte-identical copy of either (e.g. a mirror maintained by a
   process that itself reads through 1 or 2).

A host MUST NOT interpret bytes from a live machine's shared backing
file (§4) or from any source that may reflect a machine mid-input.

Reading a record is: read the header (cacheable — every field except
the live counters is a deployment constant), compute `home(key)`, read
the covering byte range, walk §6.2 locally. With a 4 KiB read starting
at the slot's page, one read suffices unless the probe chain crosses a
page boundary.

## 12. Proofs

The machine commits its full 64-bit address space to a Merkle tree with
32-byte leaves: leaf hash = keccak256 of the 32-byte word, node =
keccak256(left ‖ right). A proof of an aligned power-of-two range
against the machine root hash consists of `64 − log2(range size)`
sibling hashes (`machine.get_proof`). Because the drive is naturally
aligned (§4), the drive itself is one subtree with a single root at
height `log2(drive length)`, enabling the two-phase pattern: prove the
drive root against the machine root once (`64 − log2(driveLength)`
siblings), then prove records against the drive root
(`log2(driveLength) − log2(range)` siblings each).

**Verifying a claim "key k has record r" (or "key k is absent") against
a trusted machine root:**

1. Establish the header constants (`seed`, capacities, offsets, sizes,
   profile, and for token claims the registry prefix up to the claimed
   id) — either as deployment constants known out of band (they are
   committed by the machine template), or by verifying a proof of the
   header page (and registry range) against the same root — one proof
   for both, when the registry is in-header (§5).
2. Obtain the drive byte range covering slots `home(k) … home(k) + d`
   for the claimed walk length `d`, with its Merkle proof, and verify
   the proof against the root. (The range is contiguous — two ranges if
   it wraps — and MAY be rounded out to aligned boundaries.)
3. Run the lookup of §6.2 inside the proven bytes. The claim holds iff
   the walk terminates inside the proven range with the claimed result
   — FOUND at a slot whose record equals `r`, or ABSENT.

Soundness relies on §6.6, which holds for any drive maintained by a
conforming guest; *that* the guest conforms is exactly what the
machine's execution semantics (and whatever proves them — a fraud
proof, a validity proof, reproducible execution) already guarantee.
This spec adds no authentication of its own, on purpose: the machine
tree is the commitment.

## 13. Versioning

- A drive is governed by this document iff its magic matches and its
  version is 1. Implementations MUST NOT interpret a drive with an
  unknown version.
- Version 1 semantics are frozen by this document. Anything not
  specified here — new profiles, new flags, new header fields in the
  reserved region — requires a version or flag bump and a revision of
  this specification.
- `flags` MUST be zero in version 1; a reader encountering a nonzero
  flag MUST treat the drive as a later, unknown revision.

## Appendix A (non-normative): the Rollups v3 withdrawal drive

Cartesi Rollups v3's emergency-withdrawal machinery defines its own,
simpler accounts drive: packed 32-byte records (`uint64` little-endian
balance ‖ address ‖ zero padding), no header, no nonce, linear scan.
This format is not that one, but it was shaped to be consumable by the
same on-chain machinery:

- Records here are naturally aligned power-of-two slots, so the v3
  `Application` contract's account-validity proof (which re-merkleizes
  a record's bytes and folds them into a proven drive root) applies
  with `log2LeavesPerAccount = log2(slotSize) − 5` — 1 for 64-byte
  slots, 3 for 256-byte wide slots. The v3 `accountIndex` is the slot
  index.
- The v3 record decoding is delegated to a per-application
  `IWithdrawalOutputBuilder`; a builder for this format reads the §7
  record (and, in a wide drive, the columns the registry describes).
  Such a builder MUST treat an empty (all-zero) slot, which the v3
  bitmap machinery will happily accept an index for, as non-withdrawable.
- The two-phase proof of §12 is the same shape v3's
  `proveAccountsDriveMerkleRoot` / `withdraw` split implements.

## Appendix B (non-normative): test vectors

All vectors use `seed` = 32 zero bytes. Addresses `NN..` mean the byte
`NN` repeated 20 times.

**Home slots.** `keccak256(seed ‖ addr)`, its first 8 bytes as `LE64`,
and the home slot at two capacities:

| addr | keccak256(seed ‖ addr) | LE64(first 8) | mod 8 | mod 2^19 |
|---|---|---|---|---|
| `aa..` | `dd69b3a734c8dffb…` | `0xfbdfc834a7b369dd` | 5 | — |
| `bb..` | `693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92` | `0x5440f536af653869` | 1 | 342121 |
| `cc..` | `ff179112c23b5de1…` | `0xe15d3bc2129117ff` | 7 | — |
| `dd..` | `c1609f85523a987e…` | `0x7e983a52859f60c1` | 1 | — |
| `ee..` | `21fe9946f1ec8e11…` | `0x118eecf14699fe21` | 1 | — |

**Sparse key.** token id 3, address `bb..`: key = `0300` ‖ `bb`×20;
`keccak256(seed ‖ key)` =
`e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3`;
`LE64` = `0xbfc3cb1987d1cae5`; mod 2^19 = 117477.

**Records.**

- Account record, address `bb..`, nonce 7, balance 5:
  ```
  bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 00000000 0700000000000000
  0000000000000000000000000000000000000000000000000000000000000005
  ```
- Sparse 32-byte record, token id 3, address `bb..`, balance 1000000:
  ```
  0300 0000 bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb 00000000000f4240
  ```

**Probe walkthrough**, C = 8, empty table, inserting `bb..`, `dd..`,
`ee..` (all home 1), then `aa..` (home 5):

| step | action | table (slot: addr, displacement) |
|---|---|---|
| 1 | insert `bb..` | 1: bb,0 |
| 2 | insert `dd..` — slot 1 occupied, displacement 0 ≮ 0, advance; slot 2 empty | 1: bb,0 · 2: dd,1 |
| 3 | insert `ee..` — slots 1, 2 pass (0 ≮ 0, 1 ≮ 1); slot 3 empty | 1: bb,0 · 2: dd,1 · 3: ee,2 |
| 4 | insert `aa..` | … · 5: aa,0 |
| 5 | lookup absent key with home 1 — walks slots 1 (0 ≮ 0), 2 (1 ≮ 1), 3 (2 ≮ 2), finds slot 4 empty → ABSENT after reading 4 slots | unchanged |
| 6 | delete `dd..` at slot 2 — slot 3's `ee..` has displacement 2 > 0: moves to slot 2; slot 4 empty: zero slot 3 | 1: bb,0 · 2: ee,1 · 5: aa,0 |
