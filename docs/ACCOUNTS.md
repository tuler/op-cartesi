# A standard account model for the guest

**Status: proposal.** Nothing below is implemented; this document is the
research and the design decision it leads to. It fills the gap the README
lists as "no account model in the shim", and it does so where the README
says the durable fix belongs — in the guest, covered by the state root.

The one-line summary: give the guest a dedicated flash drive with a
specified layout — an **accounts drive** — holding one fixed-size record
per account (nonce and balance), maintained by the guest as part of the
state transition and read by the host directly out of machine memory. A
read is then one `machine.read_memory` call instead of a machine fork and
a guest execution, and every record is provable against the block's state
root through the machine's own Merkle tree, because the drive *is* machine
state. Cartesi has already shipped this pattern once — the Rollups v3
emergency-withdrawal accounts drive (ewtools) — and this proposal is
deliberately shaped so that machinery keeps working here.

## 1. The problem, precisely

Three consumers want per-account state, and today none of them can have it:

- **Wallet tooling.** `eth_getTransactionCount` is the first thing `cast
  send` asks for, and the shim cannot answer it: the state is the machine's
  memory, and there is no accounts trie to read a nonce from.
  `devnet/send-l2-tx.sh` works around it by inventing a nonce from the
  clock, which is exactly the kind of workaround that stops at the demo.
- **Replay protection.** The chain does not check a nonce, so an included
  transaction can be resubmitted and executed again. The durable check must
  live in the guest — it is part of the state transition function, and
  anything a referee might one day dispute has to be inside the proven
  state (the same argument DESIGN §7e makes for the outputs root).
- **A fee market.** "Inputs are free" is the README's first known gap. The
  moment an input costs something, the payer needs an identity, a balance
  to charge, and a nonce to order their transactions. An account model is
  the prerequisite, whichever fee design comes later.

The obvious answer — ask the guest — is the wrong tool. `eth_call` runs the
machine's inspect protocol: fork the emulator server, feed a query, execute
the guest until it yields, discard the fork (`chain/inspect.go`). That is a
process spawn plus a guest round trip per query, on a path where the guest
dominates (a devnet block replays in about 1.9 s, almost all of it guest
time). It also answers in whatever ad-hoc encoding the app chose — the
devnet bank app's query format is its own invention — so nothing about it
is standard. Inspect is right for app-shaped questions; it is a bad way to
read two integers.

What is actually needed is a **convention about where those two integers
live**, so the host can read them without running anything.

## 2. Three mechanisms that already exist

The proposal builds on three things that need no new emulator features.

**Reading machine memory is a memcpy.** The remote protocol op-cartesi
already speaks (`cartesi-jsonrpc-machine`, emulator 0.21) has
`machine.read_memory {address, length}`, and its implementation copies
bytes out of the loaded machine — no execution, no hash-tree work. In 0.21
it even reads across range boundaries, zero-filling gaps. The server
processes requests sequentially, so a read against a parked machine is
consistent by construction.

**Every byte is provable.** The machine's hash tree has 32-byte leaves over
the full 2^64 address space, and `machine.get_proof {address,
log2_target_size}` proves any aligned power-of-two range against the root
hash. The proof machinery is incremental — only pages dirtied since the
last tree update are rehashed — so proving costs are paid per dirty page,
not per query. And there is a shipped precedent for "the host and L1 read a
well-known machine address": Cartesi's guest tools write the outputs Merkle
root to the CMIO TX buffer (`0x60800000`) just before each accepted yield,
and `DaveConsensus._validateOutputTree` verifies exactly that 32-byte leaf
against the final machine hash with 59 sibling hashes. Well-known addresses
inside the machine are already how Cartesi binds guest facts to L1.

**op-cartesi already parks a machine per recent block.** The chain keeps a
live snapshot for each of the last `MaxSnapshots` (default 32) blocks so it
can build, verify and reorg. Inspect forks those; a memory read does not
need to — the snapshot is parked at a yield and a read leaves it untouched.
So block-tagged account reads inside the retention window come essentially
free: resolve the block to its machine, issue one `read_memory`.

## 3. Prior art: the Rollups v3 accounts drive (ewtools)

The idea of a flash drive with a standard structure that outsiders read is
not hypothetical — it is how Cartesi Rollups v3 does emergency
withdrawals. The `cartesi/ewtools` repo itself is private, but the format
and the whole mechanism are pinned by public code: `rollups-node` carries a
test literally named `TestEncode_MatchesEwtoolsLayout`, and the v3
contracts implement the verification end.

What v3 does, end to end:

- The app dedicates a flash drive to a table of **32-byte records** — one
  machine tree leaf each: `balance` as a little-endian `uint64` (bytes
  0–7, must be positive and fit `int64`), the 20-byte address (bytes
  8–27), four zero bytes of padding. Empty slot = all zeros. Records are
  **packed from index 0**; deletion moves the last record into the hole.
  Default capacity 2^17 records — a 4 MiB drive.
- The guest maintains it **per input**, so the drive is current at every
  claimable state. The flagship app (perp-dex) writes it through `mmap`
  into `/dev/pmem1`; the reference test guest does the same with `dd` from
  a shell script. The drive's geometry is consensus: the `Application`
  contract carries `log2MaxNumOfAccounts`, `log2LeavesPerAccount` and
  `accountsDriveStartIndex` (the drive's node index at its own tree level)
  as immutables.
- If the app's validators die or go rogue, a guardian **forecloses** it.
  Then anyone proves the drive's subtree root against the last finalized
  machine root (one proof of `64 − log2DriveSize` siblings, done once),
  and each user withdraws by proving their record against that drive root
  (`log2MaxNumOfAccounts` siblings, with a bitmap preventing reuse). The
  off-chain proofs come from the machine's own proof API
  (`cartesi-machine --initial-proof`, i.e. `cm_get_proof`), cross-checked
  against a rebuild of the drive from its backing file.
- The record encoding is explicitly **application-specific** on chain: the
  withdrawal output is built by a configurable `IWithdrawalOutputBuilder`,
  and the stock USD builder ignores bytes past 27 precisely so the raw
  32-byte drive record can be passed verbatim.

What carries over here: the drive as the guest↔outside interface; records
aligned to machine tree leaves; per-input freshness; the two-phase proof
(pay the machine-root siblings once, then cheap per-record proofs); and
the blessing that record encodings may extend beyond 32 bytes
(`log2LeavesPerAccount` exists for exactly that).

What does not: the ewtools record has **no nonce**, one `int64` balance in
a single implicit asset, and a layout shaped for exits, not queries —
lookup is a linear scan, which is fine for an offline prover walking a
4 MiB drive and wrong for a host answering RPC per transaction. The gap
this proposal fills is exactly the delta: nonce, `uint256` balances, O(1)
point lookup, and room to grow — while keeping the proof shape v3 already
verifies.

## 4. Choosing the layout

### The doctrine: flat data, machine-tree commitment

The strongest structural fact available: **the machine's hash tree already
authenticates every drive byte.** Any in-drive Merkle structure — an MPT,
a binary trie, a Verkle tree — would duplicate that, at the cost of guest
cycles per update (hashing, in an emulated CPU) and scattered dirty pages.
The state-DB literature has been converging on the same split from the
other direction: Erigon keeps accounts in a flat `PlainState` table and
derives the trie commitment separately; NOMT pairs a flat B-tree with a
separate binary-trie page store. Here the commitment half comes for free
from the emulator, so the drive should hold **only the flat half**.

Format invariants (sortedness, probe rules) do not need in-drive
authentication either: the guest code that maintains them is part of the
disputed computation, enforced by the same machinery that enforces every
other guest instruction. An in-drive authenticated index earns its keep
only if some verifier must check statements against a *sub-root* without
knowing the machine layout — e.g. an L1 contract tracking an app state
root across machine template upgrades. That is worth naming and is not
v1's problem.

Two more constraints shape the choice:

- **Dirty bytes cost twice.** Every page the guest touches costs guest
  cycles *and* a page rehash at the next tree update. A layout that
  touches one page per account update beats one that memmoves megabytes.
- **No filesystem.** The table lives at fixed offsets on the raw device.
  A file inside ext4 would make host offsets depend on extent allocation,
  dragging filesystem metadata into every read and proof. (It would also
  meet the page cache — see §5.4.)

### The candidates

Costs assume ~64-byte records, up to ~10^6 accounts, and a host that pays
one HTTP round trip per `read_memory`:

| Layout | Point lookup | Guest update | Spec surface for a verifier | Notes |
|---|---|---|---|---|
| Packed sorted array | ~log₂N reads, or 1 with a host-cached index | **O(N) memmove**, dirtying half the table | tiny | The only *content-canonical* layout; insert cost disqualifies it beyond small N |
| ewtools packed array (unsorted) | O(N) scan | O(1) append, O(1) swap-remove | tiny | Exit-shaped: fine to walk offline, wrong for per-tx RPC |
| **Open-addressing hash table** (robin hood, fixed capacity) | **1 contiguous read** (a 4 KiB page holds 64 slots, far beyond expected probe chains at load ≤ 0.8) | **O(1)**, dirties ~1 page | ~1 page: hash fn + probe rule | No ordered iteration; capacity fixed up front (so is the drive) |
| Custom in-place B+tree (4 KiB nodes, root pinned at page 0) | 3–4 scattered page reads | O(1) typical, rare deterministic splits | ~2 pages | Ordered iteration and range scans; ~1 KLoC of C |
| LMDB | 5 scattered page reads (meta → root → branches → leaf) | COW, dirties depth + freelist + meta pages | large: pages, nodes, meta-page election, freelist | Best off-the-shelf: ~85 KiB of C, fixed map size fits a fixed drive. Rejected as a *standard*: the on-disk format is not frozen across versions (the current dev branch changes the page header), layout determinism is an accident of the freelist, and running it against a raw block device is untested |
| SQLite | 3–4 reads + schema walk | few pages | largest: varints, cell arrays, freeblocks, overflow chains | Feasible and pointless — a 900 KiB library and a big spec where 2 KiB suffices |
| CDB / minimal perfect hash | 2 reads | **full rebuild per change**, re-dirtying the whole region | small | Only viable as a per-epoch immutable snapshot |
| LSM (RocksDB etc.) | multi-level read amplification | background compaction | — | Disqualified outright: compaction timing breaks determinism, needs a filesystem, defeats the few-reads budget |

On the question that prompted this document — "maybe an mmap KV database"
— the honest answer is that LMDB *would work* (it really does support a
remote reader fetching five pages), and is still the wrong choice for a
format two independent implementations and eventually an L1 contract must
agree on. A standard wants a layout it owns, small enough to specify
completely, frozen by the spec rather than by a library version.

### The recommendation

**v1: an open-addressing hash table with robin-hood probing.** One
contiguous read per lookup — which is also one compact machine proof,
since the probe path is a single aligned range. O(1) updates dirtying one
page. A specification that fits on a page. Accounts are never deleted (a
nonce only grows), which removes tombstones, the one genuinely fiddly part
of open addressing.

Its known weakness is adversarial clustering: addresses are
attacker-influenced (vanity grinding, CREATE2), and any public hash lets
an attacker spend ~L·C work to build an L-long probe chain. Robin hood
bounds the variance, generous headroom bounds the load, and the failure
mode is longer reads — never wrong answers. If a hard bound is ever
wanted, bucketized cuckoo hashing (two hashes, four-slot buckets, worst
case exactly two small reads) is the upgrade, at the cost of insert
cascades.

The B+tree is the fallback position if ordered enumeration becomes a
requirement (it is not one today: RPC needs point lookups; exits and dumps
can scan the table linearly offline). It buys sorted iteration for 3–4
scattered reads per lookup and an order of magnitude more code.

## 5. The proposed standard, v1

### 5.1 The drive

- **Label `accounts`**, declared in the machine config with an **explicit
  start address**, a power-of-two length, and a non-shared backing store
  (`shared = false`, the default; §6.1 is why that is not a free choice). The recommended start is
  `0x80000000000000` (2^55) — the classic `PMA_DRIVE_START`, which 0.21's
  new auto-placement (drives packed just after RAM) leaves entirely free,
  and which is naturally aligned for any power-of-two size up to 2^55,
  inside the emulator's 56-bit addressable cap. Explicit, because 0.21's
  auto-placement depends on RAM length and drive order — nothing a
  consensus constant should depend on.
- The host discovers the drive by label via `machine.get_initial_config`
  (or `machine.get_address_ranges`), falling back to the well-known start.
  Geometry is consensus: it is part of the machine template, so it is
  covered by the genesis state root like every other chain parameter.

### 5.2 The header page

One 4 KiB page at drive offset 0. All header integers little-endian
(RISC-V native, matching the ewtools precedent):

| Offset | Size | Field |
|---|---|---|
| 0x00 | 8 | magic: ASCII `ctsiacct` |
| 0x08 | 4 | version, `uint32` = 1 |
| 0x0c | 4 | flags, `uint32` = 0 |
| 0x10 | 8 | `capacity` C: slot count of the accounts table |
| 0x18 | 8 | `tableOffset`: byte offset of slot 0 from drive start (= 4096) |
| 0x20 | 8 | `loadLimit`: max live records before the table refuses inserts |
| 0x28 | 8 | `liveCount`: current live records (informational; a host gauge) |
| 0x30 | 32 | `seed`: hash key, chosen per chain at genesis |
| 0x50 | — | reserved (future table directory; see §5.5) |

### 5.3 The accounts table

`capacity` slots of **64 bytes** (two machine tree leaves), at
`tableOffset`, so slot i sits at `driveStart + tableOffset + 64·i` — every
slot 64-byte aligned, provable as one `get_proof` at `log2_target_size=6`
(58 siblings against the machine root).

Record layout:

| Offset | Size | Field |
|---|---|---|
| 0 | 20 | account address |
| 20 | 4 | zero padding |
| 24 | 8 | nonce, `uint64` little-endian |
| 32 | 32 | balance in wei, `uint256` **big-endian** |

An empty slot is 64 zero bytes; a live record must not have the zero
address, so the two are unambiguous. The balance is big-endian —
byte-for-byte the EVM ABI word — because the one reader that cannot
cheaply flip bytes is an L1 contract forwarding it into a transfer; the
guest is a full CPU and flips for free. The nonce stays guest-native.

Placement and probing:

- Home slot: `home(addr) = LE64(keccak256(seed ‖ addr)[0..8]) mod C`.
  Keccak because the stack already speaks it everywhere (the machine tree,
  L1), and a guest enforcing nonces needs keccak anyway for transaction
  hashing. The seed makes pre-genesis grinding pointless; post-genesis
  grinding degrades probe length, nothing else.
- Linear probing with the robin-hood rule: displacement of a record is its
  distance from home (mod C); on insert, a candidate that has probed
  further than the resident record swaps with it. Lookup probes from home
  and stops at a match, an empty slot, or a resident whose displacement is
  smaller than the probe distance. No deletions exist.
- **Fullness is consensus.** An insert that would push `liveCount` past
  `loadLimit` (recommended 7C/8) must deterministically **reject the
  input**. This has the failure mode DESIGN §7d already names — a portal
  deposit's L1 escrow happens whether or not the guest credits it — so the
  answer is capacity planning plus monitoring, not cleverness at the
  brink: the host watches `liveCount` and alarms long before the cliff.

The host's read path: compute the home slot, `read_memory` the 4 KiB page
containing it (64 slots — beyond any honest probe chain), walk the probe
sequence locally; read the next page in the rare case a chain crosses the
boundary. One round trip, sometimes two. A proof-carrying read serves the
covering page(s) with `get_proof` at `log2_target_size=12` (52 siblings)
and lets the verifier re-run the probe walk inside proven bytes — which
also makes **exclusion** provable: the probe chain ending in an empty slot
or an early-termination is right there in the same range.

### 5.4 Write discipline

The contract with the host is: **the drive bytes are current in machine
state at every manual yield.** The host only reads parked machines, and a
rejected input needs no special handling — rejection reverts the whole
machine, drive included, which is the same property the ledger relies on
today.

The trap is the guest kernel's page cache. Cartesi's kernel exposes flash
drives as `/dev/pmemN` **without DAX** (the defconfig enables none of
`ZONE_DEVICE`/`FS_DAX`/`DEV_DAX`), so both `write()` and `mmap` on the
device go through the page cache — bytes can sit in guest RAM at yield
time with the drive itself stale. Determinism does not care (RAM is
machine state too), but readability and provability at the drive address
do. So the rule for a pmem-backed drive is: `msync(MS_SYNC)` the dirtied
range (or `fsync` the device fd) **before finishing each input**.

Emulator 0.21 added the better substrate: **NVRAM ranges**, exposed to the
guest as UIO devices whose `mmap` maps the physical range directly — no
page cache, stores land in machine state immediately, no sync step. The
guest-tools support (`memoryrange`, `readmmap`/`writemmap`) is still on a
feature branch, which is the only reason this spec targets a flash drive
first. The byte layout above is substrate-independent; when NVRAM tooling
ships, the same format moves onto it and §5.4 shrinks to one sentence.

### 5.5 Extensions, named but not specified

- **Token balances.** The devnet ledger is keyed `token ‖ account`; a
  second table of 128-byte slots (token 20 ‖ address 20 ‖ padding ‖
  balance 32) with the same probing serves it. The header's reserved
  region becomes a table directory: per table, its offset, capacity, and
  record-layout id.
- **Per-app namespacing.** DESIGN §8's multi-app dimension folds in by
  keying tables per app id, or one table per app in the directory.
- **A change journal.** A ring buffer of `(table, slot)` touched per
  input would let an indexer maintain a historical mirror without
  rereading tables. Nothing in v1 needs it.

## 6. What the host does with it

### 6.1 Three read paths, and the one that looks free but is not

An obvious question deserves a direct answer first: if the drive is
backed by a host file, why involve a machine at all — why not read the
backing file? The emulator does have the mechanism: a drive declared with
`backing_store.shared = true` is mapped `MAP_SHARED` from its image file,
so the emulator's stores land in the file and are visible to any process
immediately (same page cache; no emulator-side sync involved). The
guest-side `msync` discipline of §5.4 is unchanged either way — it is
what moves bytes into the drive at all — and NVRAM removes it for both.

For **this** chain, the shared backing file is disqualified, for one
fatal reason and two structural ones:

- **Forks alias the file.** The emulator's `fork` is an OS fork, and a
  `MAP_SHARED` mapping survives fork *still shared*. This chain runs a
  swarm of forks off one lineage: a retained snapshot per recent block, a
  work fork per block being built, a candidate fork per input — discarded
  when the input rejects, which is the rollback mechanism — and a fork
  per inspect, whose guest can flush dirty pages mid-query. With a shared
  drive every one of them writes the same file, including forks that are
  then thrown away. Rollback discards a process; it cannot unwrite the
  file. The file ends up corresponding to *no* machine state, and nothing
  errors. This is the same family of silent sharing trap as
  `machine.store`'s sharing mode (DESIGN §7b), which this repo already
  pins a test against.
- **No serialization.** `read_memory` is safe by construction: the server
  handles requests sequentially, so a read cannot interleave with
  `machine.run`. A file read can land mid-input and see a torn record or
  a half-shifted probe chain. Only the shim knows when a machine is
  parked; a file reader does not.
- **No block identity, no proofs.** The file is whatever the writer last
  flushed, not the state at a named block, and a Merkle proof still
  requires asking a machine.

So the drive stays `shared = false`, writes never escape a machine's
transactional boundary, and reading happens one of three ways:

1. **Live, from a parked machine** — `machine.read_memory`, block-tagged
   and proof-capable, one local round trip. The authoritative path, and
   the sync primitive for the next one.
2. **From a mirror, with no machine on the query path.** The shim is the
   one process that knows when each block's machine is parked, so it can
   maintain a local mirror of the drive: read it whole once at startup,
   then per sealed block re-read only the touched ranges — the §5.5
   journal, or the touch set the host can largely compute from the
   block's own transactions. Queries then `mmap` the mirror with zero
   emulator involvement, and the mirror can be handed to any number of
   external consumers. The emulator remains the single writer of truth;
   the file is a projection of a named block, not a side channel.
3. **From stored images, with no server at all.** A `cm_store` directory
   contains the drive's image as a plain file, quiescent by
   construction. This is exactly how the Rollups v3 prover works — it
   locates the accounts drive via the snapshot's `config.json`, reads
   the backing file directly, and cross-checks against
   `--initial-proof`. Right for indexers and exit provers; stale by up
   to the checkpoint interval, so wrong for nonce gating.

v0 needs only path 1. The mirror is the answer if RPC read volume ever
makes even a local HTTP round trip per lookup matter.

### 6.2 The RPC surface

- **`eth_getTransactionCount`, `eth_getBalance`.** Resolve the block tag
  to a parked machine — head for `latest`/`pending`, the retained
  snapshot for a recent block or hash, `ErrNoSnapshot` past the window
  (the same rule inspect applies today) — then one or two `read_memory`
  round trips. No fork, no execution: microseconds of emulator work
  instead of a process spawn and a guest round trip.
- **`cartesi_getAccount` / `cartesi_getAccountProof`.** The record plus
  the machine proof of its covering page(s) against the block's
  `stateRoot`. This is the chain's `eth_getProof` analogue — the real
  `eth_getProof` is unimplementable here for the same reason DESIGN §7
  gives for the portal: there is no MPT and never will be. What replaces
  it is strictly stronger for this chain: a proof against the state root
  the proposals actually commit to.
- **Mempool nonce gating.** `Pool.Add` checks the sender's nonce against
  the head machine and rejects stale or far-future transactions; block
  building re-checks at inclusion. This is a courtesy filter and a DoS
  bound, **not** the enforcement: the authoritative check happens in the
  guest, where it is part of the state transition and covered by whatever
  proof system settles the chain. (Which implies the guest verifies
  sender signatures — it must recover the sender to know whose nonce to
  bump. The devnet guest does neither today; both arrive together.)
- **What stays as it is.** `eth_estimateGas` still wants an inspect on a
  discarded fork (it estimates execution, not state); `eth_gasPrice` and
  `eth_feeHistory` synthesize from headers; op-node, op-batcher and
  op-proposer never asked for accounts in the first place.

## 7. What it buys on L1

**An emergency exit, by construction.** The v3 machinery this format
stays proof-compatible with needs exactly two things on L1: a finalized
machine root, and the drive's geometry. This chain already publishes the
first — an OP proposal's root claim opens (one keccak of four words, the
`OPOutputsMerkleRootValidator` trick) to the block's `stateRoot`, which
*is* the machine root. So an `OPAccountsDriveValidator` can do the v3
two-phase dance against a proposal: prove the drive subtree root once
(`64 − log2DriveSize` siblings), then each user proves their 64-byte
record into it (`log2DriveSize − 6` siblings) and a chain-specific output
builder — the extension point v3 explicitly provides — decodes address
and balance from this layout and pays out. Under the permissioned game
this is worth what the game is worth; the point is that when settlement
lands (roadmap step 4/5), the exit path is already shaped, and users can
leave a dead chain without anyone's cooperation.

**Alignment with Dave.** DESIGN §7e's second requirement is that a value a
referee adjudicates must live inside the machine's proven state. The
accounts drive satisfies it by construction — and under a Dave-style
settlement, whose claim *is* the machine root, these proofs verify
directly against the tournament's resolved hash, which is precisely how
Rollups v3 uses them.

## 8. Sizing and upgrades

Reference numbers for the defaults (64 MiB drive, `C = 2^19`,
`loadLimit = 7C/8`): ~458k accounts, 32 MiB of table. A million-account
chain wants a 128–256 MiB drive; the format does not change, only the
genesis geometry.

Two costs to measure before freezing defaults, both bounded by table size
rather than account count (hashing scatters records across all pages):

- **Checkpoint size.** `cm_store` currently writes ~380 MiB per
  checkpoint; a 64 MiB drive adds at most 64 MiB. Whether zero pages are
  stored compactly decides how much of "at most" is real.
- **Per-block rehash.** ~1 dirty page per touched account per block, paid
  at the first root-hash request after the block — noise next to the
  pages the guest dirties anyway.

Geometry cannot change on a live chain: drives do not resize, and
`replace_memory_range` requires identical geometry (and is a host act
outside consensus besides). Growing the table is a machine template
change — a coordinated upgrade, governed like any prestate change, exactly
as it is for every other Cartesi app.

## 9. Deliberately out of scope

- `codeHash` and `storageRoot`: there are no contract accounts — the
  guest is the application. The record has 4 spare bytes and the header
  has flags if that ever changes.
- MPT-compatible `eth_getProof` (impossible; §6 has the replacement).
- Historical reads past the snapshot window (same policy as inspect; a
  change journal, §5.5, is the eventual answer if anyone needs deep
  history).
- The fee mechanism itself. This supplies identity, balance and ordering;
  what to charge is its own design.

## 10. Roadmap

1. **v0 — devnet proof of shape.** Add the drive to
   `build-snapshot.sh`; move the bank app's ledger from its Lua table
   onto the drive (its balances are already keyed the right way); serve
   `eth_getTransactionCount` and `eth_getBalance` from `read_memory`;
   delete the nonce workaround in `send-l2-tx.sh`. Test like the outputs
   root is tested: builder and verifier agree on drive bytes, and a
   record proof round-trips against the header's `stateRoot`.
2. **v1 — enforcement.** Sender recovery and nonce checking in the guest;
   mempool gating in the shim; `cartesi_getAccountProof`. This closes the
   replay-protection gap.
3. **v2 — the exit.** `OPAccountsDriveValidator` and the output builder,
   ported from the v3 contracts against `DisputeGameFactory` proposals.
4. **Later.** Token-balance table, NVRAM substrate when guest-tools
   ships it, fee market on top.

## References

- Rollups v3 accounts drive: `cartesi/rollups-contracts` branch
  `next/3.0` (`Application.sol`, `WithdrawalConfig.sol`,
  `LibUsdAccount.sol`, `IWithdrawalOutputBuilder.sol`);
  `cartesi/rollups-node` v2.0.0-alpha.12
  (`cmd/cartesi-rollups-machine-tool/accountdrive/`, whose tests pin the
  ewtools record layout).
- Emulator mechanics: `cartesi/machine-emulator` v0.21
  (`machine.read_memory`, `machine.get_proof`, address-range configs and
  labels, NVRAM/UIO ranges); `cartesi/machine-guest-tools`
  (`libcmt` outputs-root write; `feature/nvram` branch tooling);
  `cartesi/image-kernel` + `cartesi/linux` (pmem without DAX).
- Well-known-address precedent: `cartesi/dave`
  (`DaveConsensus._validateOutputTree` against the CMIO TX buffer).
- Flat-state prior art: Erigon `PlainState`; NOMT (flat B-tree + separate
  commitment pages).
