# The ABI drive — format specification, v1

Status: adopted on the devnet. Companion to
[ACCOUNTS-DRIVE-SPEC.md](ACCOUNTS-DRIVE-SPEC.md) and to the routing standard
in [EVM-COMPAT.md](EVM-COMPAT.md).

## 1. Purpose

A routed guest serves "contracts": native code dispatched by address
(EVM-COMPAT §5). Which addresses those are, and what ABI each speaks, is
knowledge the chain and outside tooling need — and it should not require
running guest code or knowing anything about the application's
implementation. The ABI drive makes a stored machine self-describing: a raw
flash drive holding an address → ABI registry, written by the guest at first
boot, readable by anyone holding the snapshot the same way balances are read
from the accounts drive.

Together the two drives answer the two discovery questions about a machine:

- **accounts drive** — which tokens does this chain hold and serve façades
  for? (the token registry, ACCOUNTS-DRIVE-SPEC §5)
- **abi drive** — which contract addresses does this chain route, and what
  ABI does each answer? (this spec)

## 2. Write-once discipline

The manifest of a routed guest is fixed when its snapshot is built — there
is no dynamic deploy (EVM-COMPAT §10). The drive is therefore written
exactly once, during first boot, **before the first yield**: the recorded
registry is genesis state, covered by the genesis state root like every
other consensus parameter. Nothing writes to the drive during an input, so
the drive needs no journaling and a reader needs no locking discipline. A
future standard revision that admits dynamic registration must bump the
version and define its journaling; v1 readers refuse unknown versions.

## 3. Layout

All integers are little-endian. Offsets are absolute drive offsets.

### Header (64 bytes, at offset 0)

| offset | size | field       | value                          |
|-------:|-----:|-------------|--------------------------------|
|      0 |    8 | magic       | `"ctsiabis"` (ASCII)           |
|      8 |    2 | version     | 1                              |
|     10 |    2 | flags       | 0                              |
|     12 |    4 | capacity    | index slots                    |
|     16 |    4 | count       | used slots                     |
|     20 |    4 | indexOffset | 64                             |
|     24 |    4 | heapOffset  | first heap byte                |
|     28 |    4 | heapLength  | heap size in bytes             |
|     32 |    4 | heapUsed    | bytes consumed from the heap   |
|     36 |   28 | reserved    | zero                           |

### Index (capacity × 32 bytes, at indexOffset)

Entries are dense, in registration order; entries `[0, count)` are valid.

| offset | size | field     | value                                   |
|-------:|-----:|-----------|-----------------------------------------|
|      0 |   20 | address   | the routed contract address              |
|     20 |    1 | kind      | 0 system built-in · 1 application        |
|     21 |    1 | reserved  | zero                                     |
|     22 |    4 | abiOffset | absolute offset of the ABI blob          |
|     26 |    4 | abiLength | blob length in bytes                     |
|     30 |    2 | reserved  | zero                                     |

### Heap (at heapOffset)

UTF-8 text blobs, tightly packed in registration order. Each blob is a
**standard Solidity JSON ABI** — the format every Ethereum tool consumes
(viem's `Abi`, Etherscan's ABI tab). Human-readable ABI strings are not
valid blob content; registration APIs may accept them but must store JSON.

## 4. What gets recorded

Every statically routed address: the system built-ins (registry, bridge,
config, `L1Block`) with kind 0, and every application contract with kind 1.
Two families of routed addresses are deliberately absent, because they are
already discoverable elsewhere:

- **Token façades** — data-driven from the accounts drive's token registry;
  their address derivation and shared ERC-20 ABI are fixed by EVM-COMPAT §6
  and §9.
- **The portal receiver** — resolved at the envelope's application-contract
  address and by registered portal sender (EVM-COMPAT §6); its calldata is
  InputEncoding's packed format, not ABI.

## 5. Devnet geometry

256 KiB (2^18 — a power of two, so the drive is one aligned subtree of the
machine's hash tree), 64 index slots, heap at 4096. Declared in
`demo/cartesi.toml` as the third drive; the guest formats it at boot
and reaches it at `$ABI_DRIVE` (`/dev/pmem2` on the devnet). Like the
accounts drive, the geometry is consensus once the snapshot is stored.

## 6. Readers

- `@op-cartesi/abis` (this repo, `abi-drive/js`) — reference reader/writer over
  a byte store; the guest runtime uses it to record registrations at boot.
  `abi-drive/js/golden.ts` writes the golden image the Go tests read, which
  is what pins the two implementations to the same bytes.
- `abi-drive/go` (this repo) — the shim's reader, over the same `Store`
  abstraction as `accounts-drive/go`. The shim discovers the drive by its
  label (`abi`), reads it through `machine.ReadMemory` — the `AccountAt`
  path — and serves `eth_getCode` markers from it: `0xEF 0xC7 0x51 <kind>`,
  with kind 0/1 straight from the index and 2 for token façades derived
  from the accounts drive's registry (`chain/code.go`). The discovery RPC,
  `cartesi_getContracts`, serves the full surface from the same reads: the
  recorded contracts with their ABIs embedded, and each façade with the L1
  token it serves (`devnet/contracts.ts` prints it).
