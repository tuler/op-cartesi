/*
 * accounts_drive.h — Cartesi Accounts Drive Format v1, pure C99.
 *
 * Implements docs/ACCOUNTS-DRIVE-SPEC.md: a fixed-layout accounts database
 * inside a Cartesi Machine flash drive, written by the guest as part of the
 * state transition and readable — and provable — from outside without
 * executing the machine.
 *
 * The same code serves both sides of the drive. A guest opens it over an
 * fd-backed store on the raw device (/dev/pmemN); a host opens it over a
 * memory store holding a copied image (or any store the caller implements,
 * e.g. one backed by machine.read_memory). All layout, hashing and probing
 * logic is shared, which is what keeps two independent parties
 * byte-for-byte agreed about what the drive means.
 *
 * Design constraints of this implementation:
 *
 *   - C99, libc only (the fd store additionally uses POSIX pread/pwrite/
 *     fsync, as any raw-device store must).
 *   - The library never allocates on the heap. The caller owns the
 *     acctdrv_drive struct; all internal slot/header buffers are bounded
 *     stack arrays. Peak stack use is a few slot buffers, i.e. a few times
 *     ACCTDRV_MAX_SLOT_SIZE.
 *   - IMPLEMENTATION LIMIT: the accounts-table slot size is capped at
 *     ACCTDRV_MAX_SLOT_SIZE (4096) bytes so stack buffers are bounded.
 *     The spec allows any power-of-two >= 128 for wide slots; drives with
 *     slots larger than 4096 bytes are refused with
 *     ACCTDRV_ERR_UNSUPPORTED. (The header page is 4096 bytes, and sparse
 *     slots are 32 or 64 bytes, so nothing else is affected.)
 *
 * Conventions (spec §2): balances are uint8_t[32], big-endian — a 32-byte
 * balance is byte-identical to an EVM ABI uint256 word. Nonces are uint64_t
 * (stored little-endian). Addresses are 20 raw bytes; the zero address is
 * invalid in every record. On a compact-record drive (profile 0 with
 * 32-byte slots, spec §7) the stored nonce is uint32 and the stored balance
 * uint64; values past those bounds are refused with ACCTDRV_ERR_OVERFLOW.
 */
#ifndef ACCOUNTS_DRIVE_H
#define ACCOUNTS_DRIVE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ------------------------------------------------------------------ limits */

/* The header page size fixed by the spec (§5). */
#define ACCTDRV_HEADER_SIZE 4096u

/* Implementation cap on a table's slot size (see file comment). */
#define ACCTDRV_MAX_SLOT_SIZE 4096u

/* Format version this library implements (spec §13). */
#define ACCTDRV_VERSION 1u

/* Profiles (spec §5). */
#define ACCTDRV_PROFILE_SINGLE_ASSET 0u
#define ACCTDRV_PROFILE_WIDE 1u
#define ACCTDRV_PROFILE_SPARSE 2u

/* ------------------------------------------------------------------ errors */

/*
 * Error codes. The first ten map 1:1 to the golden-vector `expect` kinds
 * ("ok", "tableFull", ... , "profile"); the rest are I/O and format errors
 * outside that taxonomy. In a guest, ACCTDRV_ERR_TABLE_FULL,
 * ACCTDRV_ERR_REGISTRY_FULL and ACCTDRV_ERR_OVERFLOW are exactly the
 * conditions the spec requires to be answered by rejecting the input
 * (spec §6.3, §8).
 */
typedef enum acctdrv_err {
    ACCTDRV_OK = 0,               /* "ok" */
    ACCTDRV_ERR_TABLE_FULL,       /* "tableFull": table at load limit, or a
                                     wide token registration whose column
                                     would not fit the slot (spec §7) */
    ACCTDRV_ERR_REGISTRY_FULL,    /* "registryFull": tokenCount == capacity */
    ACCTDRV_ERR_OVERFLOW,         /* "overflow": balance exceeds the token's
                                     declared width (spec §8), or a compact
                                     record's uint32 nonce / uint64 balance
                                     bound (spec §7) */
    ACCTDRV_ERR_NONCE_PROTECTED,  /* "nonceProtected": delete of a nonzero-
                                     nonce record without force (spec §7) */
    ACCTDRV_ERR_DUPLICATE_TOKEN,  /* "duplicateToken": address already in
                                     the registry (spec §8) */
    ACCTDRV_ERR_UNKNOWN_TOKEN,    /* "unknownToken": id >= token count */
    ACCTDRV_ERR_BAD_WIDTH,        /* "badWidth": width not 8/16/32, or not 8
                                     on a 32-byte-sparse-slot drive (§9) or
                                     a compact-record drive (§7) */
    ACCTDRV_ERR_ZERO_ADDRESS,     /* "zeroAddress": the zero address is
                                     invalid in every record (spec §6) */
    ACCTDRV_ERR_PROFILE,          /* "profile": operation not available in
                                     this drive's profile */
    /* -- not part of the golden taxonomy: environment / format failures -- */
    ACCTDRV_ERR_IO,               /* the store returned nonzero */
    ACCTDRV_ERR_NOT_FORMATTED,    /* bad magic: not an accounts drive */
    ACCTDRV_ERR_VERSION,          /* unsupported version, or nonzero flags */
    ACCTDRV_ERR_CONFIG,           /* invalid geometry passed to format */
    ACCTDRV_ERR_UNSUPPORTED,      /* geometry valid per spec but beyond this
                                     implementation's slot-size cap */
    ACCTDRV_ERR_CORRUPT           /* header/registry state no conforming
                                     guest can produce (e.g. zero capacity,
                                     token count past registry capacity) */
} acctdrv_err;

/* Human-readable name for an error code (static string, never NULL). */
const char *acctdrv_strerror(acctdrv_err e);

/* ------------------------------------------------------------------ stores */

/*
 * The byte-level access the library needs. Offsets are relative to the
 * drive's first byte. Callbacks return 0 on success, nonzero on failure
 * (mapped to ACCTDRV_ERR_IO). Implementations decide what backs it: a byte
 * buffer, a device file inside the guest, or a remote machine's memory on
 * the host (spec §11 — hosts must read only quiescent state).
 *
 * A host wires an existing Cartesi machine client in directly — this
 * library deliberately does not speak the machine's JSON-RPC itself. The
 * adapter is three lines: ctx holds the client handle and the drive's base
 * machine address; read_at calls the client's machine.read_memory with
 * (base + off, len); write_at returns nonzero (the machine is not writable
 * from the host).
 */
typedef struct acctdrv_store {
    void *ctx;
    int (*read_at)(void *ctx, uint64_t off, void *buf, size_t len);
    int (*write_at)(void *ctx, uint64_t off, const void *buf, size_t len);
} acctdrv_store;

/*
 * Memory store: the whole drive as one caller-owned byte buffer (an image
 * copied from a stored machine, or a test drive). The struct and the buffer
 * must outlive every drive opened over them.
 */
typedef struct acctdrv_memstore {
    uint8_t *data;
    uint64_t length;
} acctdrv_memstore;

/* Build the store interface over m (which the caller has filled in). */
acctdrv_store acctdrv_mem_store(acctdrv_memstore *m);

/*
 * File-descriptor store: the drive through an open fd — in the guest, the
 * raw device (/dev/pmemN). GUEST REQUIREMENT (spec §10): on a pmem-backed
 * flash drive, file I/O goes through the guest kernel's page cache, and
 * un-synced bytes are not in machine state at the yield — the guest MUST
 * call acctdrv_fdstore_sync before finishing each input.
 */
typedef struct acctdrv_fdstore {
    int fd;
} acctdrv_fdstore;

/* Build the store interface over f (which the caller has filled in). */
acctdrv_store acctdrv_fd_store(acctdrv_fdstore *f);

/* fsync the device; returns 0 on success, -1 with errno set (spec §10). */
int acctdrv_fdstore_sync(const acctdrv_fdstore *f);

/* ---------------------------------------------------------------- geometry */

/*
 * Drive geometry (the spec §5 header fields that are deployment constants).
 * Fields marked "0 →" are defaulted by acctdrv_format; acctdrv_open fills
 * the struct from the header (drive_length is not stored in the header and
 * reads back as 0 on a drive opened rather than formatted).
 */
typedef struct acctdrv_config {
    uint64_t drive_length;      /* power of two; used by format validation */
    uint64_t capacity;          /* accounts-table slot count, >= 2 */
    uint64_t load_limit;        /* 0 → 7/8 of capacity */
    uint8_t seed[32];           /* hash key (spec §6.1) */
    uint32_t profile;           /* ACCTDRV_PROFILE_* */
    uint32_t slot_size;         /* 0 → 64; 64 or 32 in profile 0 (32 =
                                   compact record, §7); power of two
                                   >= 128 in wide */
    uint64_t table_offset;      /* 0 → 4096 */
    uint64_t registry_offset;   /* 0 → no registry (profile 0 only) */
    uint64_t registry_capacity; /* <= 65536; 0 iff no registry */
    uint64_t sparse_offset;     /* profile 2 */
    uint64_t sparse_capacity;   /* profile 2; >= 2 */
    uint64_t sparse_load_limit; /* 0 → 7/8 of sparse_capacity */
    uint32_t sparse_slot_size;  /* 0 → 64; 32 iff every width is 8 (§9) */
} acctdrv_config;

/* ----------------------------------------------------------------- records */

/* One decoded accounts-table record (spec §7), standard or compact; a
 * compact record's uint32 nonce and uint64 balance are zero-extended. */
typedef struct acctdrv_account {
    uint8_t address[20];
    uint64_t nonce;
    uint8_t balance[32]; /* big-endian uint256 */
} acctdrv_account;

/* One decoded registry entry (spec §8). */
typedef struct acctdrv_token {
    uint8_t address[20];
    int width; /* balance width in bytes: 8, 16 or 32 */
} acctdrv_token;

/* ------------------------------------------------------------------- drive */

/*
 * An open drive. The caller owns the struct (no heap anywhere in this
 * library); treat all fields as private. Geometry is cached at open (it is
 * immutable by spec, §5). The registry LENGTH is cached too — entries are
 * read through the store on demand — and refreshed by writes through this
 * handle; a host watching a drive someone else appends to should call
 * acctdrv_reload_registry when the token count moves.
 */
typedef struct acctdrv_drive {
    acctdrv_store store;  /* private */
    acctdrv_config geo;   /* private: cached geometry */
    uint64_t token_count; /* private: cached registry length */
} acctdrv_drive;

/*
 * Format a v1 drive: validate cfg (defaults applied first), write the 4096-
 * byte header — and nothing else — onto a store that must be all-zero, then
 * open it into *d. This is how a snapshot build formats the drive image and
 * how tests build drives from nothing.
 * Errors: ACCTDRV_ERR_CONFIG, ACCTDRV_ERR_UNSUPPORTED, ACCTDRV_ERR_IO.
 */
acctdrv_err acctdrv_format(acctdrv_drive *d, const acctdrv_store *store,
                           const acctdrv_config *cfg);

/*
 * Open an existing drive: read and validate the header (magic "ctsiacct",
 * version 1, flags 0), cache the geometry, and load + validate the registry
 * length. Errors: ACCTDRV_ERR_NOT_FORMATTED, ACCTDRV_ERR_VERSION,
 * ACCTDRV_ERR_UNSUPPORTED (slot size beyond ACCTDRV_MAX_SLOT_SIZE),
 * ACCTDRV_ERR_CORRUPT, ACCTDRV_ERR_BAD_WIDTH (bad registry entry),
 * ACCTDRV_ERR_IO.
 */
acctdrv_err acctdrv_open(acctdrv_drive *d, const acctdrv_store *store);

/* The drive's cached geometry (valid as long as d is). */
const acctdrv_config *acctdrv_geometry(const acctdrv_drive *d);

/*
 * Re-read the registry length from the header and re-validate the entries.
 * For hosts watching a drive another party writes (the cache is otherwise
 * only refreshed by writes through this handle).
 */
acctdrv_err acctdrv_reload_registry(acctdrv_drive *d);

/* ---------------------------------------------------------------- accounts */

/*
 * Look up an account. *found is 0 when the address has no record — which
 * is a proven statement, not a failure — and *out is left zeroed; when
 * *found is 1, *out holds the record. out or found may not be NULL.
 */
acctdrv_err acctdrv_get_account(const acctdrv_drive *d,
                                const uint8_t addr[20],
                                acctdrv_account *out, int *found);

/*
 * Write an account's nonce and 32-byte big-endian balance, inserting the
 * record if the address has none. In the wide profile an update preserves
 * the record's token columns. On a compact-record drive a nonce past uint32
 * or a balance past uint64 is ACCTDRV_ERR_OVERFLOW (spec §7).
 * ACCTDRV_ERR_TABLE_FULL and ACCTDRV_ERR_OVERFLOW mean the input carrying
 * this change must be rejected (spec §6.3, §7).
 */
acctdrv_err acctdrv_set_account(acctdrv_drive *d, const uint8_t addr[20],
                                uint64_t nonce, const uint8_t balance[32]);

/*
 * Delete an account's record (backward-shift, spec §6.4). A record with a
 * nonzero nonce is refused with ACCTDRV_ERR_NONCE_PROTECTED unless force is
 * nonzero: deleting it would re-enable replay of that account's past
 * transactions (spec §7). In the wide profile the token columns go with the
 * record. *deleted (may be NULL) is set to 0 when there was no record —
 * which is not an error.
 */
acctdrv_err acctdrv_delete_account(acctdrv_drive *d, const uint8_t addr[20],
                                   int force, int *deleted);

/* ---------------------------------------------------------------- registry */

/*
 * Append a token to the registry (spec §8); *id (may be NULL) receives its
 * id. width must be 8, 16 or 32. In the wide profile the new column must
 * fit slot_size - 64 (ACCTDRV_ERR_TABLE_FULL otherwise, spec §7); on a
 * 32-byte-sparse-slot drive (spec §9) and on a compact-record drive
 * (spec §7) only width 8 is allowed (ACCTDRV_ERR_BAD_WIDTH).
 * ACCTDRV_ERR_REGISTRY_FULL and ACCTDRV_ERR_TABLE_FULL mean rejecting the
 * input.
 */
acctdrv_err acctdrv_register_token(acctdrv_drive *d, const uint8_t token[20],
                                   int width, uint16_t *id);

/* Read registry entry id (id must be below the cached token count). */
acctdrv_err acctdrv_token_at(const acctdrv_drive *d, uint16_t id,
                             acctdrv_token *out);

/*
 * Resolve a token address to its id by scanning the registry; *found is 0
 * when the address is not registered. id/out may be NULL.
 */
acctdrv_err acctdrv_token_by_address(const acctdrv_drive *d,
                                     const uint8_t addr[20], uint16_t *id,
                                     acctdrv_token *out, int *found);

/* ------------------------------------------------------------ token balances */

/*
 * Read an account's balance of a registered token into out (32 bytes,
 * big-endian, zero-extended from the token's width). An absent record (or
 * a column of an absent account) reads as zero, not an error.
 */
acctdrv_err acctdrv_get_token_balance(const acctdrv_drive *d,
                                      const uint8_t addr[20], uint16_t id,
                                      uint8_t out[32]);

/*
 * Write an account's balance of a registered token (value: 32 bytes,
 * big-endian). ACCTDRV_ERR_OVERFLOW (value exceeds the token's declared
 * width) and ACCTDRV_ERR_TABLE_FULL both mean rejecting the input. In the
 * wide profile a missing account record is auto-created (zero nonce and
 * native balance) on a nonzero set, and a zero set keeps the record; in the
 * sparse profile a balance reaching zero deletes its record (spec §9).
 */
acctdrv_err acctdrv_set_token_balance(acctdrv_drive *d,
                                      const uint8_t addr[20], uint16_t id,
                                      const uint8_t value[32]);

/* ---------------------------------------------------------------- counters */

/* Occupied-slot count of the accounts table (read from the header). */
acctdrv_err acctdrv_live_count(const acctdrv_drive *d, uint64_t *out);

/* Occupied-slot count of the sparse table (read from the header). */
acctdrv_err acctdrv_sparse_live_count(const acctdrv_drive *d, uint64_t *out);

/*
 * Number of registered tokens, read from the header (the id-validation
 * cache is d->token_count; see acctdrv_reload_registry).
 */
acctdrv_err acctdrv_token_count(const acctdrv_drive *d, uint64_t *out);

/* ------------------------------------------------------------------ keccak */

/*
 * Ethereum's Keccak-256 (rate 136, pre-FIPS 0x01 domain padding) — the
 * function the machine's Merkle tree and the spec's home-slot hash (§6.1)
 * use. Exposed for tests and for callers computing home slots themselves.
 */
void acctdrv_keccak256(const void *data, size_t len, uint8_t out[32]);

#ifdef __cplusplus
}
#endif

#endif /* ACCOUNTS_DRIVE_H */
