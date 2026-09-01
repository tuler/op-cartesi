"""Pure-Python implementation of the Cartesi Accounts Drive Format, version 1.

This module implements docs/ACCOUNTS-DRIVE-SPEC.md: a fixed-layout accounts
database inside a Cartesi Machine flash drive, written by the guest as part of
the state transition and readable -- and provable -- from outside without
executing the machine.

The same code serves both sides of the drive:

- **Guest**: open the drive over a :class:`FileStore` on the raw device
  (``/dev/pmemN``) and maintain it as part of the state transition. On a
  pmem-backed drive the guest MUST call :meth:`FileStore.sync` before
  finishing each input (spec section 10): guest file I/O goes through the
  kernel page cache, and un-synced bytes are not in machine state at the
  yield.
- **Host**: open the same bytes over a :class:`MachineStore` against a
  parked machine, or a :class:`MemStore` holding a copied image (spec
  section 11). All layout, hashing and probing logic is shared, which is what
  keeps two independent parties byte-for-byte agreed about what the drive
  means.

Requires only the standard library (Python >= 3.8); Keccak-256 is implemented
here (Ethereum's pre-FIPS 0x01 padding -- ``hashlib.sha3_256`` is FIPS SHA-3
with different padding and MUST NOT be substituted).

API summary (snake_case per the accounts-drive README convention):

- ``format(store, config)`` / ``open(store)`` -- create a v1 drive on an
  all-zero store / open and validate an existing one.
- Accounts: ``get_account``, ``set_account``, ``delete_account``.
- Registry: ``register_token``, ``tokens``, ``token_by_address``,
  ``reload_registry``.
- Token balances: ``get_token_balance``, ``set_token_balance``.
- Counters: ``live_count``, ``sparse_live_count``, ``token_count``.

Failures that the spec requires a guest to answer by rejecting the input
(``tableFull``, ``registryFull``, ``overflow``) -- and the rest of the shared
error taxonomy -- are raised as :class:`AccountsDriveError` with a ``.kind``
attribute holding the golden-vector kind string.

Addresses are 20 raw bytes; every address argument also accepts a 40-hex-char
string (with or without an ``0x`` prefix). Balances are Python ``int``
(arbitrary precision), encoded big-endian at 32 bytes or the token's declared
column width; nonces are ``int`` in the ``uint64`` range, encoded
little-endian.
"""

from __future__ import annotations

import dataclasses
import io
import os
import threading

__all__ = [
    "MAGIC",
    "VERSION",
    "HEADER_SIZE",
    "PROFILE_SINGLE_ASSET",
    "PROFILE_WIDE",
    "PROFILE_SPARSE",
    "ERROR_KINDS",
    "AccountsDriveError",
    "ReadOnlyStoreError",
    "keccak256",
    "home_slot",
    "sparse_key",
    "Config",
    "Token",
    "Account",
    "Store",
    "MemStore",
    "FileStore",
    "MachineStore",
    "Drive",
    "format",
    "open",
]

# ---------------------------------------------------------------------------
# Keccak-256 (Ethereum's, pre-FIPS 0x01 domain padding, rate 136)
# ---------------------------------------------------------------------------
# Implemented here so the library has no dependencies. The permutation follows
# the public-domain tiny_sha3 structure (same tables as the Go reference);
# correctness is pinned by the spec's Appendix B test vectors.

_KECCAK_RNDC = (
    0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
    0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
    0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
    0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
    0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
    0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
)
_KECCAK_ROTC = (1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8,
                25, 43, 62, 18, 39, 61, 20, 44)
_KECCAK_PILN = (10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13,
                12, 2, 20, 14, 22, 9, 6, 1)
_MASK64 = (1 << 64) - 1
_KECCAK_RATE = 136  # 1088-bit rate for 256-bit output


def _keccak_f(st):
    """The keccak-f[1600] permutation over a 25-lane state (list of ints)."""
    for rnd in range(24):
        # Theta
        bc = [st[i] ^ st[i + 5] ^ st[i + 10] ^ st[i + 15] ^ st[i + 20]
              for i in range(5)]
        for i in range(5):
            u = bc[(i + 1) % 5]
            t = bc[(i + 4) % 5] ^ (((u << 1) | (u >> 63)) & _MASK64)
            for j in range(0, 25, 5):
                st[j + i] ^= t
        # Rho and Pi
        t = st[1]
        for i in range(24):
            j = _KECCAK_PILN[i]
            tmp = st[j]
            n = _KECCAK_ROTC[i]
            st[j] = ((t << n) | (t >> (64 - n))) & _MASK64
            t = tmp
        # Chi
        for j in range(0, 25, 5):
            row = st[j:j + 5]
            for i in range(5):
                st[j + i] = row[i] ^ (~row[(i + 1) % 5] & row[(i + 2) % 5])
        # Iota
        st[0] ^= _KECCAK_RNDC[rnd]


def _keccak256_pure(*chunks: bytes) -> bytes:
    """Pure-Python Keccak-256 fallback; see keccak256 for backend selection."""
    st = [0] * 25
    buf = b"".join(bytes(c) for c in chunks)
    n = len(buf)
    pos = 0
    while n - pos >= _KECCAK_RATE:
        for w in range(_KECCAK_RATE // 8):
            st[w] ^= int.from_bytes(buf[pos + w * 8:pos + w * 8 + 8], "little")
        _keccak_f(st)
        pos += _KECCAK_RATE
    block = bytearray(_KECCAK_RATE)
    block[:n - pos] = buf[pos:]
    block[n - pos] ^= 0x01
    block[_KECCAK_RATE - 1] ^= 0x80
    for w in range(_KECCAK_RATE // 8):
        st[w] ^= int.from_bytes(block[w * 8:w * 8 + 8], "little")
    _keccak_f(st)
    return b"".join((st[w] & _MASK64).to_bytes(8, "little") for w in range(4))


# pycryptodome is Python's de-facto standard Keccak; use it when present and
# fall back to the pure implementation above otherwise (the same pattern
# eth-hash uses). The fallback is what keeps this module a single file a
# guest can drop into a rootfs with no packages installed.
try:
    from Crypto.Hash import keccak as _pycryptodome_keccak  # type: ignore
except ImportError:  # pragma: no cover - depends on the environment
    _pycryptodome_keccak = None


def keccak256(*chunks: bytes) -> bytes:
    """Keccak-256 of the concatenation of the given byte strings.

    This is Ethereum's Keccak-256 -- 0x01 domain padding, not the FIPS SHA-3
    0x06 padding (hashlib.sha3_256 is the wrong function) -- the same hash
    the machine's Merkle tree uses. Backed by pycryptodome when importable,
    by the pure-Python implementation otherwise.
    """
    if _pycryptodome_keccak is not None:
        h = _pycryptodome_keccak.new(digest_bits=256)
        for c in chunks:
            h.update(bytes(c))
        return h.digest()
    return _keccak256_pure(*chunks)


# ---------------------------------------------------------------------------
# Constants (spec section 5)
# ---------------------------------------------------------------------------

MAGIC = b"ctsiacct"
VERSION = 1
HEADER_SIZE = 4096

PROFILE_SINGLE_ASSET = 0
PROFILE_WIDE = 1
PROFILE_SPARSE = 2

# Header field offsets.
_OFF_MAGIC = 0x00
_OFF_VERSION = 0x08
_OFF_FLAGS = 0x0C
_OFF_CAPACITY = 0x10
_OFF_TABLE_OFFSET = 0x18
_OFF_LOAD_LIMIT = 0x20
_OFF_LIVE_COUNT = 0x28
_OFF_SEED = 0x30
_OFF_PROFILE = 0x50
_OFF_SLOT_SIZE = 0x54
_OFF_REGISTRY_OFFSET = 0x58
_OFF_REGISTRY_CAP = 0x60
_OFF_TOKEN_COUNT = 0x68
_OFF_SPARSE_OFFSET = 0x70
_OFF_SPARSE_CAPACITY = 0x78
_OFF_SPARSE_LOAD_LIMIT = 0x80
_OFF_SPARSE_LIVE_COUNT = 0x88
_OFF_SPARSE_SLOT_SIZE = 0x90
_MIN_REGISTRY_OFFSET = 0x100

_ZERO_ADDR = bytes(20)


# ---------------------------------------------------------------------------
# Errors
# ---------------------------------------------------------------------------

#: The shared cross-language error taxonomy (the golden-vector `expect`
#: kinds, minus "ok"). In a guest, ``tableFull``, ``registryFull`` and
#: ``overflow`` are exactly the conditions the spec requires to be answered
#: by rejecting the input.
ERROR_KINDS = frozenset({
    "tableFull", "registryFull", "overflow", "nonceProtected",
    "duplicateToken", "unknownToken", "badWidth", "zeroAddress", "profile",
})

_KIND_MESSAGES = {
    "tableFull": "table at load limit",
    "registryFull": "token registry full",
    "overflow": "balance exceeds declared width",
    "nonceProtected": "record has nonzero nonce (replay protection); "
                      "pass force=True to delete",
    "duplicateToken": "token already registered",
    "unknownToken": "token not registered",
    "badWidth": "balance width must be 8, 16 or 32",
    "zeroAddress": "zero address is invalid",
    "profile": "operation not available in this profile",
}


class AccountsDriveError(Exception):
    """A drive operation failure from the shared error taxonomy.

    ``kind`` is exactly one of :data:`ERROR_KINDS` -- the same strings the
    golden conformance vectors use in their ``expect`` fields, so a guest can
    branch on it (and reject the input for ``tableFull``, ``registryFull``
    and ``overflow``, as the spec mandates).

    Malformed drives, bad geometry and I/O problems are *not* part of the
    taxonomy; those raise ``ValueError`` / ``OSError`` instead.
    """

    def __init__(self, kind: str, message: str = None):
        if kind not in ERROR_KINDS:
            raise ValueError(f"unknown error kind {kind!r}")
        self.kind = kind
        super().__init__(message if message is not None else _KIND_MESSAGES[kind])


class ReadOnlyStoreError(OSError):
    """Raised by write_at on stores that cannot write (host-side reads)."""


# ---------------------------------------------------------------------------
# Stores
# ---------------------------------------------------------------------------

class Store:
    """The byte-level access the library needs (an informal interface).

    Offsets are relative to the drive's first byte. Implementations decide
    what backs it: a bytearray, a device file inside the guest, or a remote
    machine's memory on the host.
    """

    def read_at(self, off: int, length: int) -> bytes:
        """Return exactly ``length`` bytes starting at drive offset ``off``."""
        raise NotImplementedError

    def write_at(self, off: int, data: bytes) -> None:
        """Write ``data`` at drive offset ``off``."""
        raise NotImplementedError


class MemStore(Store):
    """An in-memory drive image: the whole drive as one bytearray.

    ``MemStore(length)`` allocates a zeroed image; ``MemStore(image)`` wraps
    an existing ``bytearray`` (shared, not copied) or copies a ``bytes``
    object. The live image is exposed as :attr:`image` -- hash it with
    ``hashlib.sha256(store.image)`` for whole-drive conformance checks.
    """

    def __init__(self, length_or_image):
        if isinstance(length_or_image, int):
            self._image = bytearray(length_or_image)
        elif isinstance(length_or_image, bytearray):
            self._image = length_or_image
        elif isinstance(length_or_image, (bytes, memoryview)):
            self._image = bytearray(length_or_image)
        else:
            raise TypeError("MemStore wants a length or a bytes-like image")

    @property
    def image(self) -> bytearray:
        """The live drive image (a bytearray; mutations show through)."""
        return self._image

    def _bounds(self, off: int, n: int) -> None:
        if off < 0 or n < 0 or off + n > len(self._image):
            raise ValueError(
                f"access [{off},{off + n}) outside drive of "
                f"{len(self._image)} bytes")

    def read_at(self, off: int, length: int) -> bytes:
        self._bounds(off, length)
        return bytes(self._image[off:off + length])

    def write_at(self, off: int, data: bytes) -> None:
        self._bounds(off, len(data))
        self._image[off:off + len(data)] = data


class FileStore(Store):
    """A drive accessed through a file -- in the guest, the raw device
    (``/dev/pmemN``); on the host, a stored machine's drive image file.

    Accepts a path (opened read-write, unbuffered) or an already-open binary
    file object positioned at the drive's first byte. I/O uses
    ``os.pread``/``os.pwrite`` where available (falling back to locked
    seek+read/write elsewhere), so concurrent readers do not fight over the
    file position.

    **Spec section 10 (guest write discipline):** on a pmem-backed flash
    drive, guest file I/O goes through the kernel page cache, so the drive's
    bytes are NOT current in machine state until flushed. A guest MUST call
    :meth:`sync` before finishing each input; un-synced bytes are not in
    machine state at the yield, and the drive a host or prover reads would be
    stale.
    """

    def __init__(self, file):
        if isinstance(file, (str, bytes, os.PathLike)):
            self._file = io.open(file, "r+b", buffering=0)
            self._owns = True
        else:
            self._file = file
            self._owns = False
        self._fd = self._file.fileno()
        self._lock = threading.Lock()  # for the seek-based fallback only

    def read_at(self, off: int, length: int) -> bytes:
        if length < 0 or off < 0:
            raise ValueError(f"bad read [{off},{off + length})")
        if hasattr(os, "pread"):
            chunks = []
            pos, remaining = off, length
            while remaining > 0:
                b = os.pread(self._fd, remaining, pos)
                if not b:
                    raise ValueError(
                        f"short read at {pos}: wanted {remaining} more bytes")
                chunks.append(b)
                pos += len(b)
                remaining -= len(b)
            return b"".join(chunks)
        with self._lock:
            self._file.seek(off)
            data = self._file.read(length)
        if len(data) != length:
            raise ValueError(
                f"short read at {off}: got {len(data)}, want {length}")
        return data

    def write_at(self, off: int, data: bytes) -> None:
        if off < 0:
            raise ValueError(f"bad write at {off}")
        data = bytes(data)
        if hasattr(os, "pwrite"):
            pos = 0
            while pos < len(data):
                pos += os.pwrite(self._fd, data[pos:], off + pos)
            return
        with self._lock:
            self._file.seek(off)
            self._file.write(data)

    def sync(self) -> None:
        """Flush written bytes through to the device (flush + ``os.fsync``).

        Per spec section 10, a guest on a pmem-backed drive MUST call this
        before finishing each input, so the drive's bytes are current in
        machine state at the quiescent point.
        """
        self._file.flush()
        os.fsync(self._fd)

    def close(self) -> None:
        if self._owns:
            self._file.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


class MachineStore(Store):
    """Reads the drive out of a Cartesi Machine through whatever machine
    client the host already uses -- this module deliberately does not speak
    the machine's JSON-RPC itself. The store only translates drive-relative
    offsets into the ``(address, length)`` arguments of the client's
    ``machine.read_memory``.

    ``read_memory`` is a callable ``(address: int, length: int) -> bytes``
    returning that many bytes at an absolute machine address;
    ``base_address`` is the drive's start address in the machine's address
    space. The store is read-only (:meth:`write_at` raises
    :class:`ReadOnlyStoreError`), and per spec section 11 it must only be
    used against a quiescent (parked or stored) machine.
    """

    def __init__(self, read_memory, base_address: int):
        self.read_memory = read_memory
        self.base_address = base_address

    def read_at(self, off: int, length: int) -> bytes:
        data = self.read_memory(self.base_address + off, length)
        if len(data) != length:
            raise OSError(
                f"read_memory returned {len(data)} bytes, want {length}")
        return bytes(data)

    def write_at(self, off: int, data: bytes) -> None:
        raise ReadOnlyStoreError("machine store is read-only")


# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

@dataclasses.dataclass
class Config:
    """The geometry of a drive. Every field is a deployment constant (spec
    section 5); :func:`format` validates the lot, and :func:`open` reads it
    back from the header.

    Zero-valued fields take spec defaults at format time: ``slot_size`` 64,
    ``table_offset`` 4096, ``load_limit`` (and ``sparse_load_limit``) 7/8 of
    the capacity, ``sparse_slot_size`` 64. ``seed`` accepts 32 raw bytes or
    64 hex chars.
    """

    drive_length: int = 0
    capacity: int = 0
    load_limit: int = 0            # 0 -> 7/8 of capacity
    seed: bytes = _ZERO_ADDR + b"\x00" * 12  # 32 zero bytes
    profile: int = PROFILE_SINGLE_ASSET
    slot_size: int = 0             # 0 -> 64; 64 or 32 (compact) in profile 0,
                                   # 64 in profile 2, pow2 >= 128 in profile 1
    table_offset: int = 0          # 0 -> 4096

    registry_offset: int = 0       # 0 -> none (allowed only in profile 0)
    registry_capacity: int = 0

    sparse_offset: int = 0
    sparse_capacity: int = 0
    sparse_load_limit: int = 0     # 0 -> 7/8 of sparse_capacity
    sparse_slot_size: int = 0      # 0 -> 64

    def _apply_defaults(self) -> None:
        if isinstance(self.seed, str):
            self.seed = bytes.fromhex(
                self.seed[2:] if self.seed.lower().startswith("0x") else self.seed)
        else:
            self.seed = bytes(self.seed)
        if self.slot_size == 0:
            self.slot_size = 64
        if self.table_offset == 0:
            self.table_offset = HEADER_SIZE
        if self.load_limit == 0 and self.capacity > 0:
            self.load_limit = self.capacity - self.capacity // 8
        if self.profile == PROFILE_SPARSE:
            if self.sparse_slot_size == 0:
                self.sparse_slot_size = 64
            if self.sparse_load_limit == 0 and self.sparse_capacity > 0:
                self.sparse_load_limit = (
                    self.sparse_capacity - self.sparse_capacity // 8)

    def _validate(self) -> None:
        dl = self.drive_length
        if dl <= 0 or dl & (dl - 1) != 0:
            raise ValueError(f"drive length {dl} is not a power of two")
        if len(self.seed) != 32:
            raise ValueError(f"seed is {len(self.seed)} bytes, want 32")
        if not (PROFILE_SINGLE_ASSET <= self.profile <= PROFILE_SPARSE):
            raise ValueError(f"unknown profile {self.profile}")
        ss = self.slot_size
        if self.profile == PROFILE_WIDE:
            if ss < 128 or ss & (ss - 1) != 0:
                raise ValueError(
                    f"wide slot size {ss}: want a power of two >= 128")
        elif self.profile == PROFILE_SINGLE_ASSET:
            if ss not in (64, 32):
                raise ValueError(
                    f"slot size {ss}: profile 0 takes 64 (standard record) "
                    f"or 32 (compact record)")
        elif ss != 64:
            raise ValueError(
                f"slot size {ss}: must be 64 in the sparse profile")
        if self.capacity < 2:
            raise ValueError(f"capacity {self.capacity} < 2")
        if not (1 <= self.load_limit < self.capacity):
            raise ValueError(
                f"load limit {self.load_limit} outside [1, capacity-1]")
        if self.table_offset < HEADER_SIZE or self.table_offset % ss != 0:
            raise ValueError(
                f"table offset {self.table_offset}: want >= {HEADER_SIZE} "
                f"and a multiple of the slot size")
        regions = [
            ("header", 0, HEADER_SIZE),
            ("accounts table", self.table_offset, self.capacity * ss),
        ]
        if self.profile != PROFILE_SINGLE_ASSET and self.registry_offset == 0:
            raise ValueError(f"profile {self.profile} requires a token registry")
        if self.registry_offset != 0:
            if not (1 <= self.registry_capacity <= 65536):
                raise ValueError(
                    f"registry capacity {self.registry_capacity} "
                    f"outside [1, 65536]")
            if self.registry_offset % 32 != 0:
                raise ValueError(
                    f"registry offset {self.registry_offset} "
                    f"not a multiple of 32")
            end = self.registry_offset + 32 * self.registry_capacity
            if self.registry_offset < HEADER_SIZE:
                # In-header placement (spec section 5 geometry rules).
                if self.registry_offset < _MIN_REGISTRY_OFFSET or end > HEADER_SIZE:
                    raise ValueError(
                        f"in-header registry [{self.registry_offset:#x},"
                        f"{end:#x}) outside [{_MIN_REGISTRY_OFFSET:#x},"
                        f"{HEADER_SIZE:#x})")
            else:
                regions.append(("registry", self.registry_offset,
                                32 * self.registry_capacity))
        if self.profile == PROFILE_SPARSE:
            sss = self.sparse_slot_size
            if sss not in (64, 32):
                raise ValueError(f"sparse slot size {sss}: want 64 or 32")
            if self.sparse_capacity < 2:
                raise ValueError(f"sparse capacity {self.sparse_capacity} < 2")
            if not (1 <= self.sparse_load_limit < self.sparse_capacity):
                raise ValueError(
                    f"sparse load limit {self.sparse_load_limit} "
                    f"outside [1, capacity-1]")
            if self.sparse_offset < HEADER_SIZE or self.sparse_offset % sss != 0:
                raise ValueError(
                    f"sparse offset {self.sparse_offset}: want >= "
                    f"{HEADER_SIZE} and a multiple of the slot size")
            regions.append(("sparse table", self.sparse_offset,
                            self.sparse_capacity * sss))
        for name, off, ln in regions:
            if off + ln > dl:
                raise ValueError(
                    f"{name} [{off},{off + ln}) outside drive of {dl} bytes")
        for i, (an, ao, al) in enumerate(regions):
            for bn, bo, bl in regions[i + 1:]:
                if ao < bo + bl and bo < ao + al:
                    raise ValueError(f"{an} overlaps {bn}")


# ---------------------------------------------------------------------------
# Records
# ---------------------------------------------------------------------------

class Token:
    """One registry entry (spec section 8): a token address and its declared
    big-endian balance width in bytes (8, 16 or 32)."""

    __slots__ = ("address", "width")

    def __init__(self, address: bytes, width: int):
        self.address = bytes(address)
        self.width = width

    def __repr__(self):
        return f"Token(address={self.address.hex()}, width={self.width})"

    def __eq__(self, other):
        return (isinstance(other, Token) and self.address == other.address
                and self.width == other.width)


class Account:
    """The account record of spec section 7 -- standard or compact --
    decoded: 20-byte address, nonce and balance as Python ints."""

    __slots__ = ("address", "nonce", "balance")

    def __init__(self, address: bytes, nonce: int, balance: int):
        self.address = bytes(address)
        self.nonce = nonce
        self.balance = balance

    def __repr__(self):
        return (f"Account(address={self.address.hex()}, "
                f"nonce={self.nonce}, balance={self.balance})")

    def __eq__(self, other):
        return (isinstance(other, Account) and self.address == other.address
                and self.nonce == other.nonce and self.balance == other.balance)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _to_addr(addr) -> bytes:
    """Normalize an address argument to 20 raw bytes (accepts hex str)."""
    if isinstance(addr, str):
        s = addr[2:] if addr.lower().startswith("0x") else addr
        b = bytes.fromhex(s)
    elif isinstance(addr, (bytes, bytearray, memoryview)):
        b = bytes(addr)
    else:
        raise TypeError(f"address must be bytes or hex str, not {type(addr)}")
    if len(b) != 20:
        raise ValueError(f"address is {len(b)} bytes, want 20")
    return b


def sparse_key(token_id: int, addr) -> bytes:
    """The sparse table's 22-byte key: ``uint16`` little-endian token id
    followed by the 20-byte address (spec section 6.1)."""
    if not (0 <= token_id < 1 << 16):
        raise ValueError(f"token id {token_id} outside uint16")
    return token_id.to_bytes(2, "little") + _to_addr(addr)


def home_slot(seed: bytes, key: bytes, capacity: int) -> int:
    """``home(key) = LE64(keccak256(seed || key)[0..8]) mod capacity``
    (spec section 6.1). Exposed for hosts and verifiers walking proofs."""
    h = keccak256(bytes(seed), bytes(key))
    return int.from_bytes(h[:8], "little") % capacity


def _le(v: int, size: int) -> bytes:
    return v.to_bytes(size, "little")


def _put_be(buf: bytearray, off: int, width: int, value: int) -> None:
    """Write a non-negative int big-endian at the given width, or raise
    the ``overflow`` kind (value exceeds the declared width)."""
    if value < 0 or value.bit_length() > 8 * width:
        raise AccountsDriveError("overflow")
    buf[off:off + width] = value.to_bytes(width, "big")


class _Table:
    """One of the two hash tables, so the probing code (spec section 6) is
    written once. ``key(slot)`` extracts the record's key; a slot is live iff
    its address part (20 bytes at ``live_off``) is nonzero."""

    __slots__ = ("base", "capacity", "slot_size", "count_off", "limit",
                 "key", "live_off")

    def __init__(self, base, capacity, slot_size, count_off, limit, key,
                 live_off):
        self.base = base
        self.capacity = capacity
        self.slot_size = slot_size
        self.count_off = count_off
        self.limit = limit
        self.key = key
        self.live_off = live_off


# ---------------------------------------------------------------------------
# Drive
# ---------------------------------------------------------------------------

class Drive:
    """An open accounts drive. Geometry is cached at open (it is immutable by
    spec); the registry is cached too and refreshed on writes through this
    instance -- a host watching a drive someone else appends to should call
    :meth:`reload_registry` when the token count moves.

    Construct with :func:`format` (new drive on an all-zero store) or
    :func:`open` (existing drive)."""

    def __init__(self, store: Store, cfg: Config):
        # Internal: use format()/open(), which validate.
        self._store = store
        self._cfg = cfg
        self._tokens = []   # list[Token], id order
        self._col_off = []  # wide profile: column byte offset per token id

    # ------------------------------------------------------------- geometry

    @property
    def config(self) -> Config:
        """A copy of the drive's geometry."""
        return dataclasses.replace(self._cfg)

    @property
    def store(self) -> Store:
        """The store this drive reads and writes."""
        return self._store

    def tokens(self):
        """The cached registry, in id order (a copy)."""
        return list(self._tokens)

    def reload_registry(self) -> None:
        """Re-read the registry (and the wide column offsets) from the store."""
        self._tokens = []
        self._col_off = []
        if self._cfg.registry_offset == 0:
            return
        n = self._counter(_OFF_TOKEN_COUNT)
        if n > self._cfg.registry_capacity:
            raise ValueError(
                f"token count {n} exceeds registry capacity "
                f"{self._cfg.registry_capacity}")
        col = 64
        for i in range(n):
            e = self._store.read_at(self._cfg.registry_offset + 32 * i, 32)
            width = e[20]
            if width not in (8, 16, 32):
                raise AccountsDriveError(
                    "badWidth", f"registry entry {i}: bad width {width}")
            self._tokens.append(Token(e[:20], width))
            self._col_off.append(col)
            col += width

    # ------------------------------------------------------------- counters

    def _counter(self, off: int) -> int:
        return int.from_bytes(self._store.read_at(off, 8), "little")

    def _set_counter(self, off: int, v: int) -> None:
        self._store.write_at(off, _le(v, 8))

    def live_count(self) -> int:
        """The accounts table's occupied-slot count."""
        return self._counter(_OFF_LIVE_COUNT)

    def sparse_live_count(self) -> int:
        """The sparse table's occupied-slot count."""
        return self._counter(_OFF_SPARSE_LIVE_COUNT)

    def token_count(self) -> int:
        """The number of registered tokens."""
        return self._counter(_OFF_TOKEN_COUNT)

    # -------------------------------------------------------------- tables

    def _accounts_table(self) -> _Table:
        return _Table(
            base=self._cfg.table_offset, capacity=self._cfg.capacity,
            slot_size=self._cfg.slot_size, count_off=_OFF_LIVE_COUNT,
            limit=self._cfg.load_limit,
            key=lambda s: s[0:20], live_off=0)

    def _sparse_table(self) -> _Table:
        return _Table(
            base=self._cfg.sparse_offset, capacity=self._cfg.sparse_capacity,
            slot_size=self._cfg.sparse_slot_size,
            count_off=_OFF_SPARSE_LIVE_COUNT,
            limit=self._cfg.sparse_load_limit,
            key=lambda s: bytes(s[0:2]) + bytes(s[4:24]), live_off=4)

    def _home(self, key: bytes, capacity: int) -> int:
        return home_slot(self._cfg.seed, key, capacity)

    def _read_slot(self, t: _Table, i: int) -> bytes:
        return self._store.read_at(t.base + t.slot_size * i, t.slot_size)

    def _write_slot(self, t: _Table, i: int, slot: bytes) -> None:
        self._store.write_at(t.base + t.slot_size * i, slot)

    def _live(self, t: _Table, slot: bytes) -> bool:
        return slot[t.live_off:t.live_off + 20] != _ZERO_ADDR

    def _displacement(self, t: _Table, slot: bytes, at: int) -> int:
        return (at - self._home(t.key(slot), t.capacity)) % t.capacity

    def _lookup(self, t: _Table, key: bytes):
        """Spec section 6.2. Returns ``(index, slot_bytes)`` or ``None``."""
        h = self._home(key, t.capacity)
        for dist in range(t.capacity):
            i = (h + dist) % t.capacity
            slot = self._read_slot(t, i)
            if not self._live(t, slot):
                return None
            if t.key(slot) == key:
                return i, slot
            if self._displacement(t, slot, i) < dist:
                return None
        return None  # unreachable while load limit < capacity

    def _insert(self, t: _Table, rec: bytes) -> None:
        """Spec section 6.3 (robin-hood, strict ``<`` swap). The caller has
        checked the key is absent and the load limit has room; the live
        counter is bumped here."""
        carry = bytes(rec)
        h = self._home(t.key(carry), t.capacity)
        dist = 0
        while True:
            i = (h + dist) % t.capacity
            slot = self._read_slot(t, i)
            if not self._live(t, slot):
                self._write_slot(t, i, carry)
                self._set_counter(t.count_off, self._counter(t.count_off) + 1)
                return
            disp = self._displacement(t, slot, i)
            if disp < dist:
                self._write_slot(t, i, carry)
                carry = slot
                h = self._home(t.key(carry), t.capacity)
                dist = disp
            dist += 1

    def _remove(self, t: _Table, i: int) -> None:
        """Spec section 6.4 (backward shift) from a known slot index."""
        while True:
            j = (i + 1) % t.capacity
            nxt = self._read_slot(t, j)
            if not self._live(t, nxt) or self._displacement(t, nxt, j) == 0:
                self._write_slot(t, i, bytes(t.slot_size))
                self._set_counter(t.count_off, self._counter(t.count_off) - 1)
                return
            self._write_slot(t, i, nxt)
            i = j

    # ------------------------------------------------------------- accounts

    def _compact(self) -> bool:
        """Whether the drive uses the 32-byte profile-0 record (spec 7)."""
        return (self._cfg.profile == PROFILE_SINGLE_ASSET
                and self._cfg.slot_size == 32)

    def _decode_account(self, slot):
        """Read ``(nonce, balance)`` out of a live account slot."""
        if self._compact():
            return (int.from_bytes(slot[20:24], "little"),
                    int.from_bytes(slot[24:32], "big"))
        return (int.from_bytes(slot[24:32], "little"),
                int.from_bytes(slot[32:64], "big"))

    def _encode_account(self, a: bytes, nonce: int, balance: int) -> bytes:
        """Build a record. In the compact record a nonce past ``uint32`` or
        a balance past ``uint64`` is kind ``overflow`` -- the input carrying
        the change must be rejected."""
        if self._compact():
            if nonce > 0xFFFFFFFF:
                raise AccountsDriveError(
                    "overflow", "compact record nonce exceeds uint32")
            rec = bytearray(32)
            rec[0:20] = a
            rec[20:24] = _le(nonce, 4)
            _put_be(rec, 24, 8, balance)
            return bytes(rec)
        rec = bytearray(64)
        rec[0:20] = a
        rec[24:32] = _le(nonce, 8)
        _put_be(rec, 32, 32, balance)
        return bytes(rec)

    def get_account(self, addr):
        """Look an account up. Returns an :class:`Account`, or ``None`` when
        the address has no record (a proven statement, not a failure)."""
        a = _to_addr(addr)
        if a == _ZERO_ADDR:
            raise AccountsDriveError("zeroAddress")
        found = self._lookup(self._accounts_table(), a)
        if found is None:
            return None
        _, slot = found
        nonce, balance = self._decode_account(slot)
        return Account(a, nonce, balance)

    def set_account(self, addr, nonce: int, balance: int) -> None:
        """Write an account's nonce and balance, inserting the record if the
        address has none. Raises kind ``tableFull`` when the insert would
        pass the load limit, and kind ``overflow`` when a compact record's
        nonce would pass ``uint32`` or its balance ``uint64`` -- in a guest,
        reject the input (spec 6.3, 7)."""
        a = _to_addr(addr)
        if a == _ZERO_ADDR:
            raise AccountsDriveError("zeroAddress")
        if not (0 <= nonce < 1 << 64):
            raise ValueError(f"nonce {nonce} outside uint64")
        rec = self._encode_account(a, nonce, balance)
        t = self._accounts_table()
        found = self._lookup(t, a)
        if found is not None:
            # Update in place, touching only the record bytes: in the wide
            # profile the rest of the slot is token columns.
            i, slot = found
            full = bytearray(slot)
            full[:len(rec)] = rec
            self._write_slot(t, i, bytes(full))
            return
        if self._counter(t.count_off) >= t.limit:
            raise AccountsDriveError("tableFull")
        full = bytearray(t.slot_size)
        full[:len(rec)] = rec
        self._insert(t, bytes(full))

    def delete_account(self, addr, force: bool = False) -> bool:
        """Remove an account's record. A record with a nonzero nonce is
        refused (kind ``nonceProtected``) unless ``force`` is set: deleting
        it re-enables replay of that account's past transactions (spec
        section 7). In the wide profile the record's token columns go with
        it. Returns ``False`` when there was no record."""
        a = _to_addr(addr)
        if a == _ZERO_ADDR:
            raise AccountsDriveError("zeroAddress")
        t = self._accounts_table()
        found = self._lookup(t, a)
        if found is None:
            return False
        i, slot = found
        nonce, _ = self._decode_account(slot)
        if not force and nonce != 0:
            raise AccountsDriveError("nonceProtected")
        self._remove(t, i)
        return True

    # ------------------------------------------------------------- registry

    def token_by_address(self, addr):
        """Resolve a token address to ``(id, Token)``, or ``None``."""
        a = _to_addr(addr)
        for i, tok in enumerate(self._tokens):
            if tok.address == a:
                return i, tok
        return None

    def register_token(self, addr, width: int) -> int:
        """Append a token to the registry; returns its id. In the wide
        profile the new column must fit the slot's ``slotSize - 64`` budget
        (spec section 7, kind ``tableFull``); everywhere the registry must
        have room (spec section 8, kind ``registryFull``). Both mean
        rejecting the input. The zero address is permitted (it stands for
        the native asset in a profile-0 declaration)."""
        if self._cfg.registry_offset == 0:
            raise AccountsDriveError("profile")
        if width not in (8, 16, 32):
            raise AccountsDriveError("badWidth")
        a = _to_addr(addr)
        if self.token_by_address(a) is not None:
            raise AccountsDriveError("duplicateToken")
        n = len(self._tokens)
        if n >= self._cfg.registry_capacity:
            raise AccountsDriveError("registryFull")
        if self._cfg.profile == PROFILE_WIDE:
            total = 64 + sum(tok.width for tok in self._tokens) + width
            if total > self._cfg.slot_size:
                raise AccountsDriveError(
                    "tableFull",
                    f"columns would need {total} bytes in a "
                    f"{self._cfg.slot_size}-byte slot")
        if (self._cfg.profile == PROFILE_SPARSE
                and self._cfg.sparse_slot_size == 32 and width != 8):
            raise AccountsDriveError(
                "badWidth", "32-byte sparse slots require width 8")
        if self._compact() and width != 8:
            raise AccountsDriveError(
                "badWidth", "the compact account record requires width 8")
        e = bytearray(32)
        e[:20] = a
        e[20] = width
        self._store.write_at(self._cfg.registry_offset + 32 * n, bytes(e))
        self._set_counter(_OFF_TOKEN_COUNT, n + 1)
        col = 64 if not self._col_off else (
            self._col_off[-1] + self._tokens[-1].width)
        self._tokens.append(Token(a, width))
        self._col_off.append(col)
        return n

    # ------------------------------------------------------- token balances

    def _check_token(self, addr, token_id: int) -> bytes:
        a = _to_addr(addr)
        if a == _ZERO_ADDR:
            raise AccountsDriveError("zeroAddress")
        if not (0 <= token_id < len(self._tokens)):
            raise AccountsDriveError("unknownToken")
        return a

    def get_token_balance(self, addr, token_id: int) -> int:
        """An account's balance of a registered token. An absent record (or
        column of an absent account) is zero, not an error."""
        a = self._check_token(addr, token_id)
        if self._cfg.profile == PROFILE_WIDE:
            found = self._lookup(self._accounts_table(), a)
            if found is None:
                return 0
            _, slot = found
            off = self._col_off[token_id]
            return int.from_bytes(
                slot[off:off + self._tokens[token_id].width], "big")
        if self._cfg.profile == PROFILE_SPARSE:
            found = self._lookup(self._sparse_table(), sparse_key(token_id, a))
            if found is None:
                return 0
            _, slot = found
            if self._cfg.sparse_slot_size == 32:
                return int.from_bytes(slot[24:32], "big")
            return int.from_bytes(slot[32:64], "big")
        raise AccountsDriveError("profile")

    def set_token_balance(self, addr, token_id: int, value: int) -> None:
        """Write an account's balance of a registered token. Kind
        ``overflow`` (value exceeds the token's declared width) and
        ``tableFull`` both mean rejecting the input. In the wide profile a
        missing account record is auto-created with a zero nonce and native
        balance (and setting zero on a present record keeps it); in the
        sparse profile a balance reaching zero deletes its record (spec
        section 9)."""
        a = self._check_token(addr, token_id)
        width = self._tokens[token_id].width
        if value < 0 or value.bit_length() > 8 * width:
            raise AccountsDriveError("overflow")
        if self._cfg.profile == PROFILE_WIDE:
            t = self._accounts_table()
            off = self._col_off[token_id]
            found = self._lookup(t, a)
            if found is None:
                if value == 0:
                    return
                if self._counter(t.count_off) >= t.limit:
                    raise AccountsDriveError("tableFull")
                full = bytearray(t.slot_size)
                full[0:20] = a
                _put_be(full, off, width, value)
                self._insert(t, bytes(full))
                return
            i, slot = found
            full = bytearray(slot)
            _put_be(full, off, width, value)
            self._write_slot(t, i, bytes(full))
            return
        if self._cfg.profile == PROFILE_SPARSE:
            t = self._sparse_table()
            key = sparse_key(token_id, a)
            found = self._lookup(t, key)
            if value == 0:
                if found is None:
                    return
                self._remove(t, found[0])
                return
            if found is not None:
                i, slot = found
                full = bytearray(slot)
                self._put_sparse_balance(full, value)
                self._write_slot(t, i, bytes(full))
                return
            if self._counter(t.count_off) >= t.limit:
                raise AccountsDriveError("tableFull")
            rec = bytearray(t.slot_size)
            rec[0:2] = _le(token_id, 2)
            rec[4:24] = a
            self._put_sparse_balance(rec, value)
            self._insert(t, bytes(rec))
            return
        raise AccountsDriveError("profile")

    def _put_sparse_balance(self, slot: bytearray, value: int) -> None:
        if self._cfg.sparse_slot_size == 32:
            _put_be(slot, 24, 8, value)
        else:
            _put_be(slot, 32, 32, value)


# ---------------------------------------------------------------------------
# format / open
# ---------------------------------------------------------------------------

def format(store: Store, config: Config) -> Drive:  # noqa: A001 - API name
    """Write a v1 header (and nothing else) describing ``config`` onto a
    store that must be all-zero, then open it. This is how a snapshot build
    formats the drive image, and how tests build drives from nothing.

    (Intentionally shadows the ``format`` builtin inside this module; call
    it as ``accounts_drive.format``.)"""
    cfg = dataclasses.replace(config)
    cfg._apply_defaults()
    cfg._validate()
    h = bytearray(HEADER_SIZE)
    h[_OFF_MAGIC:_OFF_MAGIC + 8] = MAGIC
    h[_OFF_VERSION:_OFF_VERSION + 4] = _le(VERSION, 4)
    # flags: zero in version 1
    h[_OFF_CAPACITY:_OFF_CAPACITY + 8] = _le(cfg.capacity, 8)
    h[_OFF_TABLE_OFFSET:_OFF_TABLE_OFFSET + 8] = _le(cfg.table_offset, 8)
    h[_OFF_LOAD_LIMIT:_OFF_LOAD_LIMIT + 8] = _le(cfg.load_limit, 8)
    h[_OFF_SEED:_OFF_SEED + 32] = cfg.seed
    h[_OFF_PROFILE:_OFF_PROFILE + 4] = _le(cfg.profile, 4)
    h[_OFF_SLOT_SIZE:_OFF_SLOT_SIZE + 4] = _le(cfg.slot_size, 4)
    h[_OFF_REGISTRY_OFFSET:_OFF_REGISTRY_OFFSET + 8] = _le(cfg.registry_offset, 8)
    h[_OFF_REGISTRY_CAP:_OFF_REGISTRY_CAP + 8] = _le(cfg.registry_capacity, 8)
    if cfg.profile == PROFILE_SPARSE:
        h[_OFF_SPARSE_OFFSET:_OFF_SPARSE_OFFSET + 8] = _le(cfg.sparse_offset, 8)
        h[_OFF_SPARSE_CAPACITY:_OFF_SPARSE_CAPACITY + 8] = _le(cfg.sparse_capacity, 8)
        h[_OFF_SPARSE_LOAD_LIMIT:_OFF_SPARSE_LOAD_LIMIT + 8] = _le(cfg.sparse_load_limit, 8)
        h[_OFF_SPARSE_SLOT_SIZE:_OFF_SPARSE_SLOT_SIZE + 4] = _le(cfg.sparse_slot_size, 4)
    store.write_at(0, bytes(h))
    return open(store)


def open(store: Store) -> Drive:  # noqa: A001 - API name
    """Read and validate the header, cache the geometry and the registry,
    and return a :class:`Drive`.

    Raises ``ValueError`` for a store that is not an accounts drive (bad
    magic), has an unknown version, or nonzero flags (a later, unknown
    revision -- spec section 13).

    (Intentionally shadows the ``open`` builtin inside this module; call it
    as ``accounts_drive.open``.)"""
    h = store.read_at(0, HEADER_SIZE)
    if h[_OFF_MAGIC:_OFF_MAGIC + 8] != MAGIC:
        raise ValueError("not an accounts drive (bad magic)")
    version = int.from_bytes(h[_OFF_VERSION:_OFF_VERSION + 4], "little")
    if version != VERSION:
        raise ValueError(f"unsupported accounts drive version {version}")
    flags = int.from_bytes(h[_OFF_FLAGS:_OFF_FLAGS + 4], "little")
    if flags != 0:
        raise ValueError(
            f"unsupported accounts drive version: nonzero flags {flags:#x}")
    cfg = Config(
        capacity=int.from_bytes(h[_OFF_CAPACITY:_OFF_CAPACITY + 8], "little"),
        table_offset=int.from_bytes(h[_OFF_TABLE_OFFSET:_OFF_TABLE_OFFSET + 8], "little"),
        load_limit=int.from_bytes(h[_OFF_LOAD_LIMIT:_OFF_LOAD_LIMIT + 8], "little"),
        seed=bytes(h[_OFF_SEED:_OFF_SEED + 32]),
        profile=int.from_bytes(h[_OFF_PROFILE:_OFF_PROFILE + 4], "little"),
        slot_size=int.from_bytes(h[_OFF_SLOT_SIZE:_OFF_SLOT_SIZE + 4], "little"),
        registry_offset=int.from_bytes(h[_OFF_REGISTRY_OFFSET:_OFF_REGISTRY_OFFSET + 8], "little"),
        registry_capacity=int.from_bytes(h[_OFF_REGISTRY_CAP:_OFF_REGISTRY_CAP + 8], "little"),
        sparse_offset=int.from_bytes(h[_OFF_SPARSE_OFFSET:_OFF_SPARSE_OFFSET + 8], "little"),
        sparse_capacity=int.from_bytes(h[_OFF_SPARSE_CAPACITY:_OFF_SPARSE_CAPACITY + 8], "little"),
        sparse_load_limit=int.from_bytes(h[_OFF_SPARSE_LOAD_LIMIT:_OFF_SPARSE_LOAD_LIMIT + 8], "little"),
        sparse_slot_size=int.from_bytes(h[_OFF_SPARSE_SLOT_SIZE:_OFF_SPARSE_SLOT_SIZE + 4], "little"),
    )
    if cfg.slot_size == 0:
        cfg.slot_size = 64
    d = Drive(store, cfg)
    d.reload_registry()
    return d
