/*
 * accounts_drive.c — Cartesi Accounts Drive Format v1, pure C99.
 *
 * Behavior mirrors the Go reference implementation (accounts-drive/go)
 * exactly; the shared golden vectors (accounts-drive/testdata/golden.json)
 * pin the two to byte-identical drives.
 *
 * No heap allocation anywhere: slot and header buffers are bounded stack
 * arrays (hence the ACCTDRV_MAX_SLOT_SIZE cap documented in the header).
 */
#define _POSIX_C_SOURCE 200809L /* pread, pwrite, fsync for the fd store */

#include "accounts_drive.h"

#include <errno.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

/* Header field offsets (spec §5). */
#define OFF_MAGIC 0x00u
#define OFF_VERSION 0x08u
#define OFF_FLAGS 0x0cu
#define OFF_CAPACITY 0x10u
#define OFF_TABLE_OFFSET 0x18u
#define OFF_LOAD_LIMIT 0x20u
#define OFF_LIVE_COUNT 0x28u
#define OFF_SEED 0x30u
#define OFF_PROFILE 0x50u
#define OFF_SLOT_SIZE 0x54u
#define OFF_REGISTRY_OFFSET 0x58u
#define OFF_REGISTRY_CAP 0x60u
#define OFF_TOKEN_COUNT 0x68u
#define OFF_SPARSE_OFFSET 0x70u
#define OFF_SPARSE_CAPACITY 0x78u
#define OFF_SPARSE_LOAD_LIMIT 0x80u
#define OFF_SPARSE_LIVE_COUNT 0x88u
#define OFF_SPARSE_SLOT_SIZE 0x90u
#define MIN_REGISTRY_OFFSET 0x100u

static const uint8_t acctdrv_magic[8] = {'c', 't', 's', 'i', 'a', 'c', 'c', 't'};

/* ------------------------------------------------------------- Keccak-256 */

static const uint64_t keccak_rndc[24] = {
    0x0000000000000001ull, 0x0000000000008082ull, 0x800000000000808aull,
    0x8000000080008000ull, 0x000000000000808bull, 0x0000000080000001ull,
    0x8000000080008081ull, 0x8000000000008009ull, 0x000000000000008aull,
    0x0000000000000088ull, 0x0000000080008009ull, 0x000000008000000aull,
    0x000000008000808bull, 0x800000000000008bull, 0x8000000000008089ull,
    0x8000000000008003ull, 0x8000000000008002ull, 0x8000000000000080ull,
    0x000000000000800aull, 0x800000008000000aull, 0x8000000080008081ull,
    0x8000000000008080ull, 0x0000000080000001ull, 0x8000000080008008ull};

static const unsigned keccak_rotc[24] = {1,  3,  6,  10, 15, 21, 28, 36,
                                         45, 55, 2,  14, 27, 41, 56, 8,
                                         25, 43, 62, 18, 39, 61, 20, 44};

static const int keccak_piln[24] = {10, 7,  11, 17, 18, 3, 5,  16,
                                    8,  21, 24, 4,  15, 23, 19, 13,
                                    12, 2,  20, 14, 22, 9, 6,  1};

static uint64_t rotl64(uint64_t x, unsigned n)
{
    return (x << n) | (x >> (64u - n));
}

static void keccak_f(uint64_t st[25])
{
    uint64_t bc[5];
    int round, i, j;
    for (round = 0; round < 24; round++) {
        for (i = 0; i < 5; i++)
            bc[i] = st[i] ^ st[i + 5] ^ st[i + 10] ^ st[i + 15] ^ st[i + 20];
        for (i = 0; i < 5; i++) {
            uint64_t t = bc[(i + 4) % 5] ^ rotl64(bc[(i + 1) % 5], 1);
            for (j = 0; j < 25; j += 5)
                st[j + i] ^= t;
        }
        {
            uint64_t t = st[1];
            for (i = 0; i < 24; i++) {
                j = keccak_piln[i];
                bc[0] = st[j];
                st[j] = rotl64(t, keccak_rotc[i]);
                t = bc[0];
            }
        }
        for (j = 0; j < 25; j += 5) {
            for (i = 0; i < 5; i++)
                bc[i] = st[j + i];
            for (i = 0; i < 5; i++)
                st[j + i] ^= ~bc[(i + 1) % 5] & bc[(i + 2) % 5];
        }
        st[0] ^= keccak_rndc[round];
    }
}

#define KECCAK_RATE 136 /* 1088-bit rate for 256-bit output */

static void keccak_absorb(uint64_t st[25], const uint8_t block[KECCAK_RATE])
{
    int i, b;
    for (i = 0; i < KECCAK_RATE / 8; i++) {
        uint64_t lane = 0;
        for (b = 0; b < 8; b++)
            lane |= (uint64_t)block[i * 8 + b] << (8 * b);
        st[i] ^= lane;
    }
    keccak_f(st);
}

void acctdrv_keccak256(const void *data, size_t len, uint8_t out[32])
{
    uint64_t st[25] = {0};
    uint8_t block[KECCAK_RATE];
    const uint8_t *p = (const uint8_t *)data;
    size_t n = 0;
    int i, b;
    memset(block, 0, sizeof block);
    while (len > 0) {
        size_t c = KECCAK_RATE - n;
        if (c > len)
            c = len;
        memcpy(block + n, p, c);
        n += c;
        p += c;
        len -= c;
        if (n == KECCAK_RATE) {
            keccak_absorb(st, block);
            memset(block, 0, sizeof block);
            n = 0;
        }
    }
    block[n] ^= 0x01; /* Ethereum's pre-FIPS domain padding */
    block[KECCAK_RATE - 1] ^= 0x80;
    keccak_absorb(st, block);
    for (i = 0; i < 4; i++)
        for (b = 0; b < 8; b++)
            out[i * 8 + b] = (uint8_t)(st[i] >> (8 * b));
}

/* -------------------------------------------------------------- utilities */

static uint64_t get_le64(const uint8_t *p)
{
    uint64_t v = 0;
    int i;
    for (i = 7; i >= 0; i--)
        v = (v << 8) | p[i];
    return v;
}

static void put_le64(uint8_t *p, uint64_t v)
{
    int i;
    for (i = 0; i < 8; i++)
        p[i] = (uint8_t)(v >> (8 * i));
}

static uint32_t get_le32(const uint8_t *p)
{
    return (uint32_t)p[0] | (uint32_t)p[1] << 8 | (uint32_t)p[2] << 16 |
           (uint32_t)p[3] << 24;
}

static void put_le32(uint8_t *p, uint32_t v)
{
    int i;
    for (i = 0; i < 4; i++)
        p[i] = (uint8_t)(v >> (8 * i));
}

static void put_le16(uint8_t *p, uint16_t v)
{
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
}

static int is_zero(const uint8_t *p, size_t n)
{
    size_t i;
    for (i = 0; i < n; i++)
        if (p[i] != 0)
            return 0;
    return 1;
}

const char *acctdrv_strerror(acctdrv_err e)
{
    switch (e) {
    case ACCTDRV_OK:
        return "ok";
    case ACCTDRV_ERR_TABLE_FULL:
        return "table at load limit";
    case ACCTDRV_ERR_REGISTRY_FULL:
        return "token registry full";
    case ACCTDRV_ERR_OVERFLOW:
        return "balance exceeds declared width";
    case ACCTDRV_ERR_NONCE_PROTECTED:
        return "record has nonzero nonce (replay protection); force to delete";
    case ACCTDRV_ERR_DUPLICATE_TOKEN:
        return "token already registered";
    case ACCTDRV_ERR_UNKNOWN_TOKEN:
        return "token not registered";
    case ACCTDRV_ERR_BAD_WIDTH:
        return "balance width must be 8, 16 or 32";
    case ACCTDRV_ERR_ZERO_ADDRESS:
        return "zero address is invalid";
    case ACCTDRV_ERR_PROFILE:
        return "operation not available in this profile";
    case ACCTDRV_ERR_IO:
        return "store I/O error";
    case ACCTDRV_ERR_NOT_FORMATTED:
        return "not an accounts drive (bad magic)";
    case ACCTDRV_ERR_VERSION:
        return "unsupported accounts drive version or flags";
    case ACCTDRV_ERR_CONFIG:
        return "invalid drive geometry";
    case ACCTDRV_ERR_UNSUPPORTED:
        return "geometry beyond this implementation's slot-size cap";
    case ACCTDRV_ERR_CORRUPT:
        return "malformed drive state";
    }
    return "unknown error";
}

/* ------------------------------------------------------------------ stores */

static int mem_read_at(void *ctx, uint64_t off, void *buf, size_t len)
{
    const acctdrv_memstore *m = (const acctdrv_memstore *)ctx;
    if (len > m->length || off > m->length - len)
        return -1;
    memcpy(buf, m->data + off, len);
    return 0;
}

static int mem_write_at(void *ctx, uint64_t off, const void *buf, size_t len)
{
    acctdrv_memstore *m = (acctdrv_memstore *)ctx;
    if (len > m->length || off > m->length - len)
        return -1;
    memcpy(m->data + off, buf, len);
    return 0;
}

acctdrv_store acctdrv_mem_store(acctdrv_memstore *m)
{
    acctdrv_store s;
    s.ctx = m;
    s.read_at = mem_read_at;
    s.write_at = mem_write_at;
    return s;
}

static int fd_read_at(void *ctx, uint64_t off, void *buf, size_t len)
{
    const acctdrv_fdstore *f = (const acctdrv_fdstore *)ctx;
    uint8_t *p = (uint8_t *)buf;
    while (len > 0) {
        ssize_t r = pread(f->fd, p, len, (off_t)off);
        if (r < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }
        if (r == 0)
            return -1; /* EOF before len bytes */
        p += r;
        off += (uint64_t)r;
        len -= (size_t)r;
    }
    return 0;
}

static int fd_write_at(void *ctx, uint64_t off, const void *buf, size_t len)
{
    const acctdrv_fdstore *f = (const acctdrv_fdstore *)ctx;
    const uint8_t *p = (const uint8_t *)buf;
    while (len > 0) {
        ssize_t r = pwrite(f->fd, p, len, (off_t)off);
        if (r < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }
        if (r == 0)
            return -1;
        p += r;
        off += (uint64_t)r;
        len -= (size_t)r;
    }
    return 0;
}

acctdrv_store acctdrv_fd_store(acctdrv_fdstore *f)
{
    acctdrv_store s;
    s.ctx = f;
    s.read_at = fd_read_at;
    s.write_at = fd_write_at;
    return s;
}

int acctdrv_fdstore_sync(const acctdrv_fdstore *f)
{
    return fsync(f->fd);
}

/* ---------------------------------------------------------- store helpers */

static acctdrv_err store_read(const acctdrv_drive *d, uint64_t off, void *buf,
                              size_t len)
{
    return d->store.read_at(d->store.ctx, off, buf, len) == 0 ? ACCTDRV_OK
                                                              : ACCTDRV_ERR_IO;
}

static acctdrv_err store_write(const acctdrv_drive *d, uint64_t off,
                               const void *buf, size_t len)
{
    return d->store.write_at(d->store.ctx, off, buf, len) == 0
               ? ACCTDRV_OK
               : ACCTDRV_ERR_IO;
}

static acctdrv_err read_counter(const acctdrv_drive *d, uint64_t off,
                                uint64_t *out)
{
    uint8_t b[8];
    acctdrv_err err = store_read(d, off, b, 8);
    if (err)
        return err;
    *out = get_le64(b);
    return ACCTDRV_OK;
}

static acctdrv_err write_counter(const acctdrv_drive *d, uint64_t off,
                                 uint64_t v)
{
    uint8_t b[8];
    put_le64(b, v);
    return store_write(d, off, b, 8);
}

/* ------------------------------------------------- tables (spec §6, both) */

/*
 * Both hash tables (accounts §7, sparse §9) share the open-addressing
 * robin-hood scheme of spec §6; this descriptor is what makes the probing
 * code below single-sourced, exactly like the Go reference.
 */
typedef struct {
    uint64_t base;      /* byte offset of slot 0 from drive start */
    uint64_t capacity;  /* C */
    uint64_t slot_size; /* S */
    uint64_t limit;     /* load limit */
    uint64_t count_off; /* header offset of the live counter */
    int sparse;         /* key/liveness encoding selector */
} acct_table;

static void accounts_table(const acctdrv_drive *d, acct_table *t)
{
    t->base = d->geo.table_offset;
    t->capacity = d->geo.capacity;
    t->slot_size = d->geo.slot_size;
    t->limit = d->geo.load_limit;
    t->count_off = OFF_LIVE_COUNT;
    t->sparse = 0;
}

static void sparse_table(const acctdrv_drive *d, acct_table *t)
{
    t->base = d->geo.sparse_offset;
    t->capacity = d->geo.sparse_capacity;
    t->slot_size = d->geo.sparse_slot_size;
    t->limit = d->geo.sparse_load_limit;
    t->count_off = OFF_SPARSE_LIVE_COUNT;
    t->sparse = 1;
}

/*
 * Key extraction (spec §6.1): the 20-byte address (accounts table), or the
 * 2-byte little-endian token id followed by the 20-byte address (sparse).
 */
static size_t slot_key(const acct_table *t, const uint8_t *slot,
                       uint8_t out[22])
{
    if (t->sparse) {
        out[0] = slot[0];
        out[1] = slot[1];
        memcpy(out + 2, slot + 4, 20);
        return 22;
    }
    memcpy(out, slot, 20);
    return 20;
}

/* A slot is live iff its address field is nonzero (spec §6). */
static int slot_live(const acct_table *t, const uint8_t *slot)
{
    return !is_zero(slot + (t->sparse ? 4 : 0), 20);
}

/* home(key) = LE64(keccak256(seed ‖ key)[0..8]) mod C (spec §6.1). */
static uint64_t home_of(const acctdrv_drive *d, const uint8_t *key,
                        size_t key_len, uint64_t capacity)
{
    uint8_t buf[32 + 22];
    uint8_t h[32];
    memcpy(buf, d->geo.seed, 32);
    memcpy(buf + 32, key, key_len);
    acctdrv_keccak256(buf, 32 + key_len, h);
    return get_le64(h) % capacity;
}

/* displacement(r@i) = (i − home(key(r))) mod C (spec §6.1). */
static uint64_t displacement(const acctdrv_drive *d, const acct_table *t,
                             const uint8_t *slot, uint64_t at)
{
    uint8_t k[22];
    size_t kl = slot_key(t, slot, k);
    uint64_t h = home_of(d, k, kl, t->capacity);
    return (at + t->capacity - h) % t->capacity;
}

static acctdrv_err read_slot(const acctdrv_drive *d, const acct_table *t,
                             uint64_t i, uint8_t *slot)
{
    return store_read(d, t->base + t->slot_size * i, slot,
                      (size_t)t->slot_size);
}

static acctdrv_err write_slot(const acctdrv_drive *d, const acct_table *t,
                              uint64_t i, const uint8_t *slot)
{
    return store_write(d, t->base + t->slot_size * i, slot,
                       (size_t)t->slot_size);
}

/*
 * Lookup (spec §6.2). On return with *found nonzero, *idx is the slot index
 * and slot holds its bytes; with *found zero the key is proven absent.
 */
static acctdrv_err tbl_lookup(const acctdrv_drive *d, const acct_table *t,
                              const uint8_t *key, size_t key_len,
                              uint64_t *idx, uint8_t *slot, int *found)
{
    uint64_t h = home_of(d, key, key_len, t->capacity);
    uint64_t dist;
    *found = 0;
    for (dist = 0; dist < t->capacity; dist++) {
        uint64_t i = (h + dist) % t->capacity;
        uint8_t k[22];
        size_t kl;
        acctdrv_err err = read_slot(d, t, i, slot);
        if (err)
            return err;
        if (!slot_live(t, slot))
            return ACCTDRV_OK; /* empty slot: absent */
        kl = slot_key(t, slot, k);
        if (kl == key_len && memcmp(k, key, key_len) == 0) {
            *idx = i;
            *found = 1;
            return ACCTDRV_OK;
        }
        if (displacement(d, t, slot, i) < dist)
            return ACCTDRV_OK; /* richer than us: absent */
    }
    return ACCTDRV_OK; /* unreachable while load limit < capacity */
}

/*
 * Insert (spec §6.3). The caller has checked the key is absent and the load
 * limit has room; insert bumps the live counter itself. carry is clobbered.
 * The strict `<` swap rule makes chains first-come-first-placed among equal
 * displacements; the spec forbids varying it.
 */
static acctdrv_err tbl_insert(const acctdrv_drive *d, const acct_table *t,
                              uint8_t *carry)
{
    uint8_t cur[ACCTDRV_MAX_SLOT_SIZE];
    uint8_t k[22];
    size_t kl = slot_key(t, carry, k);
    uint64_t h = home_of(d, k, kl, t->capacity);
    uint64_t dist = 0;
    for (;;) {
        uint64_t i = (h + dist) % t->capacity;
        acctdrv_err err = read_slot(d, t, i, cur);
        if (err)
            return err;
        if (!slot_live(t, cur)) {
            uint64_t n;
            err = write_slot(d, t, i, carry);
            if (err)
                return err;
            err = read_counter(d, t->count_off, &n);
            if (err)
                return err;
            return write_counter(d, t->count_off, n + 1);
        }
        {
            uint64_t disp = displacement(d, t, cur, i);
            if (disp < dist) {
                err = write_slot(d, t, i, carry);
                if (err)
                    return err;
                memcpy(carry, cur, (size_t)t->slot_size);
                kl = slot_key(t, carry, k);
                h = home_of(d, k, kl, t->capacity);
                dist = disp;
            }
        }
        dist++;
    }
}

/*
 * Delete by backward shift from a known slot index (spec §6.4). No
 * tombstones: after deletion, empty slots are all-zero.
 */
static acctdrv_err tbl_remove(const acctdrv_drive *d, const acct_table *t,
                              uint64_t i)
{
    uint8_t next[ACCTDRV_MAX_SLOT_SIZE];
    for (;;) {
        uint64_t j = (i + 1) % t->capacity;
        acctdrv_err err = read_slot(d, t, j, next);
        if (err)
            return err;
        if (!slot_live(t, next) || displacement(d, t, next, j) == 0) {
            uint64_t n;
            memset(next, 0, (size_t)t->slot_size);
            err = write_slot(d, t, i, next);
            if (err)
                return err;
            err = read_counter(d, t->count_off, &n);
            if (err)
                return err;
            return write_counter(d, t->count_off, n - 1);
        }
        err = write_slot(d, t, i, next);
        if (err)
            return err;
        i = j;
    }
}

/* -------------------------------------------------------- format and open */

static int is_pow2(uint64_t x)
{
    return x != 0 && (x & (x - 1)) == 0;
}

typedef struct {
    uint64_t off, len;
} region_t;

static void cfg_defaults(acctdrv_config *c)
{
    if (c->slot_size == 0)
        c->slot_size = 64;
    if (c->table_offset == 0)
        c->table_offset = ACCTDRV_HEADER_SIZE;
    if (c->load_limit == 0 && c->capacity > 0)
        c->load_limit = c->capacity - c->capacity / 8;
    if (c->profile == ACCTDRV_PROFILE_SPARSE) {
        if (c->sparse_slot_size == 0)
            c->sparse_slot_size = 64;
        if (c->sparse_load_limit == 0 && c->sparse_capacity > 0)
            c->sparse_load_limit = c->sparse_capacity - c->sparse_capacity / 8;
    }
}

/* Geometry validation, mirroring the Go reference's Config.validate. */
static acctdrv_err cfg_validate(const acctdrv_config *c)
{
    region_t regions[4];
    size_t nregions = 0;
    uint64_t ss = c->slot_size;
    size_t i, j;

    if (!is_pow2(c->drive_length))
        return ACCTDRV_ERR_CONFIG;
    if (c->profile > ACCTDRV_PROFILE_SPARSE)
        return ACCTDRV_ERR_CONFIG;
    if (c->profile == ACCTDRV_PROFILE_WIDE) {
        if (ss < 128 || !is_pow2(ss))
            return ACCTDRV_ERR_CONFIG;
    } else if (c->profile == ACCTDRV_PROFILE_SINGLE_ASSET) {
        /* 64 = standard record, 32 = compact record (spec §5, §7). */
        if (ss != 64 && ss != 32)
            return ACCTDRV_ERR_CONFIG;
    } else if (ss != 64) {
        return ACCTDRV_ERR_CONFIG;
    }
    if (ss > ACCTDRV_MAX_SLOT_SIZE)
        return ACCTDRV_ERR_UNSUPPORTED; /* implementation limit */
    if (c->capacity < 2)
        return ACCTDRV_ERR_CONFIG;
    if (c->load_limit < 1 || c->load_limit >= c->capacity)
        return ACCTDRV_ERR_CONFIG;
    if (c->table_offset < ACCTDRV_HEADER_SIZE || c->table_offset % ss != 0)
        return ACCTDRV_ERR_CONFIG;

    regions[nregions].off = 0;
    regions[nregions].len = ACCTDRV_HEADER_SIZE;
    nregions++;
    regions[nregions].off = c->table_offset;
    regions[nregions].len = c->capacity * ss;
    nregions++;

    if (c->profile != ACCTDRV_PROFILE_SINGLE_ASSET && c->registry_offset == 0)
        return ACCTDRV_ERR_CONFIG;
    if (c->registry_offset != 0) {
        uint64_t end;
        if (c->registry_capacity == 0 || c->registry_capacity > 65536)
            return ACCTDRV_ERR_CONFIG;
        if (c->registry_offset % 32 != 0)
            return ACCTDRV_ERR_CONFIG;
        end = c->registry_offset + 32 * c->registry_capacity;
        if (c->registry_offset < ACCTDRV_HEADER_SIZE) {
            /* in-header placement (spec §5 geometry rules) */
            if (c->registry_offset < MIN_REGISTRY_OFFSET ||
                end > ACCTDRV_HEADER_SIZE)
                return ACCTDRV_ERR_CONFIG;
        } else {
            regions[nregions].off = c->registry_offset;
            regions[nregions].len = 32 * c->registry_capacity;
            nregions++;
        }
    }
    if (c->profile == ACCTDRV_PROFILE_SPARSE) {
        uint64_t sss = c->sparse_slot_size;
        if (sss != 64 && sss != 32)
            return ACCTDRV_ERR_CONFIG;
        if (c->sparse_capacity < 2)
            return ACCTDRV_ERR_CONFIG;
        if (c->sparse_load_limit < 1 ||
            c->sparse_load_limit >= c->sparse_capacity)
            return ACCTDRV_ERR_CONFIG;
        if (c->sparse_offset < ACCTDRV_HEADER_SIZE ||
            c->sparse_offset % sss != 0)
            return ACCTDRV_ERR_CONFIG;
        regions[nregions].off = c->sparse_offset;
        regions[nregions].len = c->sparse_capacity * sss;
        nregions++;
    }
    for (i = 0; i < nregions; i++) {
        uint64_t end = regions[i].off + regions[i].len;
        if (end > c->drive_length || end < regions[i].off)
            return ACCTDRV_ERR_CONFIG;
    }
    for (i = 0; i < nregions; i++)
        for (j = i + 1; j < nregions; j++)
            if (regions[i].off < regions[j].off + regions[j].len &&
                regions[j].off < regions[i].off + regions[i].len)
                return ACCTDRV_ERR_CONFIG; /* overlap */
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_reload_registry(acctdrv_drive *d)
{
    uint64_t n, i;
    acctdrv_err err;
    d->token_count = 0;
    if (d->geo.registry_offset == 0)
        return ACCTDRV_OK;
    err = read_counter(d, OFF_TOKEN_COUNT, &n);
    if (err)
        return err;
    if (n > d->geo.registry_capacity)
        return ACCTDRV_ERR_CORRUPT;
    for (i = 0; i < n; i++) {
        uint8_t e[32];
        err = store_read(d, d->geo.registry_offset + 32 * i, e, 32);
        if (err)
            return err;
        if (e[20] != 8 && e[20] != 16 && e[20] != 32)
            return ACCTDRV_ERR_BAD_WIDTH;
    }
    d->token_count = n;
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_open(acctdrv_drive *d, const acctdrv_store *store)
{
    uint8_t h[ACCTDRV_HEADER_SIZE];
    acctdrv_config *c;
    memset(d, 0, sizeof *d);
    d->store = *store;
    if (store_read(d, 0, h, sizeof h))
        return ACCTDRV_ERR_IO;
    if (memcmp(h + OFF_MAGIC, acctdrv_magic, 8) != 0)
        return ACCTDRV_ERR_NOT_FORMATTED;
    if (get_le32(h + OFF_VERSION) != ACCTDRV_VERSION)
        return ACCTDRV_ERR_VERSION;
    if (get_le32(h + OFF_FLAGS) != 0)
        return ACCTDRV_ERR_VERSION; /* nonzero flags: later revision (§13) */
    c = &d->geo;
    c->capacity = get_le64(h + OFF_CAPACITY);
    c->table_offset = get_le64(h + OFF_TABLE_OFFSET);
    c->load_limit = get_le64(h + OFF_LOAD_LIMIT);
    memcpy(c->seed, h + OFF_SEED, 32);
    c->profile = get_le32(h + OFF_PROFILE);
    c->slot_size = get_le32(h + OFF_SLOT_SIZE);
    if (c->slot_size == 0)
        c->slot_size = 64;
    c->registry_offset = get_le64(h + OFF_REGISTRY_OFFSET);
    c->registry_capacity = get_le64(h + OFF_REGISTRY_CAP);
    c->sparse_offset = get_le64(h + OFF_SPARSE_OFFSET);
    c->sparse_capacity = get_le64(h + OFF_SPARSE_CAPACITY);
    c->sparse_load_limit = get_le64(h + OFF_SPARSE_LOAD_LIMIT);
    c->sparse_slot_size = get_le32(h + OFF_SPARSE_SLOT_SIZE);
    /*
     * Defensive checks beyond the Go reference (which would crash on these):
     * the slot-size implementation cap, and divisors that must be nonzero.
     */
    if (c->slot_size > ACCTDRV_MAX_SLOT_SIZE)
        return ACCTDRV_ERR_UNSUPPORTED;
    if (c->capacity == 0)
        return ACCTDRV_ERR_CORRUPT;
    /* The slot size fixes the record layout (spec §5, §7): 64 or 32 in
     * profile 0 (32 selects the compact record), 64 in the sparse profile. */
    if (c->profile == ACCTDRV_PROFILE_SINGLE_ASSET &&
        c->slot_size != 64 && c->slot_size != 32)
        return ACCTDRV_ERR_CORRUPT;
    if (c->profile == ACCTDRV_PROFILE_SPARSE && c->slot_size != 64)
        return ACCTDRV_ERR_CORRUPT;
    if (c->profile == ACCTDRV_PROFILE_SPARSE) {
        if (c->sparse_slot_size != 32 && c->sparse_slot_size != 64)
            return ACCTDRV_ERR_CORRUPT;
        if (c->sparse_capacity == 0)
            return ACCTDRV_ERR_CORRUPT;
    }
    return acctdrv_reload_registry(d);
}

acctdrv_err acctdrv_format(acctdrv_drive *d, const acctdrv_store *store,
                           const acctdrv_config *cfg)
{
    acctdrv_config c = *cfg;
    uint8_t h[ACCTDRV_HEADER_SIZE];
    acctdrv_err err;
    cfg_defaults(&c);
    err = cfg_validate(&c);
    if (err)
        return err;
    memset(h, 0, sizeof h);
    memcpy(h + OFF_MAGIC, acctdrv_magic, 8);
    put_le32(h + OFF_VERSION, ACCTDRV_VERSION);
    put_le64(h + OFF_CAPACITY, c.capacity);
    put_le64(h + OFF_TABLE_OFFSET, c.table_offset);
    put_le64(h + OFF_LOAD_LIMIT, c.load_limit);
    memcpy(h + OFF_SEED, c.seed, 32);
    put_le32(h + OFF_PROFILE, c.profile);
    put_le32(h + OFF_SLOT_SIZE, c.slot_size);
    put_le64(h + OFF_REGISTRY_OFFSET, c.registry_offset);
    put_le64(h + OFF_REGISTRY_CAP, c.registry_capacity);
    if (c.profile == ACCTDRV_PROFILE_SPARSE) {
        put_le64(h + OFF_SPARSE_OFFSET, c.sparse_offset);
        put_le64(h + OFF_SPARSE_CAPACITY, c.sparse_capacity);
        put_le64(h + OFF_SPARSE_LOAD_LIMIT, c.sparse_load_limit);
        put_le32(h + OFF_SPARSE_SLOT_SIZE, c.sparse_slot_size);
    }
    if (store->write_at(store->ctx, 0, h, sizeof h) != 0)
        return ACCTDRV_ERR_IO;
    return acctdrv_open(d, store);
}

const acctdrv_config *acctdrv_geometry(const acctdrv_drive *d)
{
    return &d->geo;
}

/* ---------------------------------------------------------------- counters */

acctdrv_err acctdrv_live_count(const acctdrv_drive *d, uint64_t *out)
{
    return read_counter(d, OFF_LIVE_COUNT, out);
}

acctdrv_err acctdrv_sparse_live_count(const acctdrv_drive *d, uint64_t *out)
{
    return read_counter(d, OFF_SPARSE_LIVE_COUNT, out);
}

acctdrv_err acctdrv_token_count(const acctdrv_drive *d, uint64_t *out)
{
    return read_counter(d, OFF_TOKEN_COUNT, out);
}

/* ---------------------------------------------------------------- accounts */

/* Whether the drive uses the 32-byte compact profile-0 record (spec §7). */
static int drive_compact(const acctdrv_drive *d)
{
    return d->geo.profile == ACCTDRV_PROFILE_SINGLE_ASSET &&
           d->geo.slot_size == 32;
}

/* Decode nonce and balance out of a live account slot (spec §7). */
static void decode_account(const acctdrv_drive *d, const uint8_t *slot,
                           acctdrv_account *out)
{
    if (drive_compact(d)) {
        out->nonce = get_le32(slot + 20);
        memcpy(out->balance + 24, slot + 24, 8);
    } else {
        out->nonce = get_le64(slot + 24);
        memcpy(out->balance, slot + 32, 32);
    }
}

/*
 * Encode a record into rec (rec_len receives 32 or 64 bytes). In the compact
 * record a nonce past uint32 or a balance past uint64 is
 * ACCTDRV_ERR_OVERFLOW — the input must be rejected (spec §7).
 */
static acctdrv_err encode_account(const acctdrv_drive *d, const uint8_t addr[20],
                                  uint64_t nonce, const uint8_t balance[32],
                                  uint8_t rec[64], size_t *rec_len)
{
    if (drive_compact(d)) {
        if (nonce > 0xffffffffull)
            return ACCTDRV_ERR_OVERFLOW;
        if (!is_zero(balance, 24))
            return ACCTDRV_ERR_OVERFLOW;
        memset(rec, 0, 32);
        memcpy(rec, addr, 20);
        put_le32(rec + 20, (uint32_t)nonce);
        memcpy(rec + 24, balance + 24, 8);
        *rec_len = 32;
        return ACCTDRV_OK;
    }
    memset(rec, 0, 64);
    memcpy(rec, addr, 20);
    put_le64(rec + 24, nonce);
    memcpy(rec + 32, balance, 32);
    *rec_len = 64;
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_get_account(const acctdrv_drive *d,
                                const uint8_t addr[20], acctdrv_account *out,
                                int *found)
{
    acct_table t;
    uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
    uint64_t i;
    acctdrv_err err;
    memset(out, 0, sizeof *out);
    *found = 0;
    if (is_zero(addr, 20))
        return ACCTDRV_ERR_ZERO_ADDRESS;
    accounts_table(d, &t);
    err = tbl_lookup(d, &t, addr, 20, &i, slot, found);
    if (err || !*found)
        return err;
    memcpy(out->address, addr, 20);
    decode_account(d, slot, out);
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_set_account(acctdrv_drive *d, const uint8_t addr[20],
                                uint64_t nonce, const uint8_t balance[32])
{
    acct_table t;
    uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
    uint8_t rec[64];
    size_t rec_len;
    uint64_t i, n;
    int found;
    acctdrv_err err;
    if (is_zero(addr, 20))
        return ACCTDRV_ERR_ZERO_ADDRESS;
    err = encode_account(d, addr, nonce, balance, rec, &rec_len);
    if (err)
        return err; /* compact overflow: reject the input (spec §7) */
    accounts_table(d, &t);
    err = tbl_lookup(d, &t, addr, 20, &i, slot, &found);
    if (err)
        return err;
    if (found) {
        /*
         * Update in place, touching only the record bytes: in the wide
         * profile the rest of the slot is token columns (spec §7).
         */
        memcpy(slot, rec, rec_len);
        return write_slot(d, &t, i, slot);
    }
    err = read_counter(d, t.count_off, &n);
    if (err)
        return err;
    if (n >= t.limit)
        return ACCTDRV_ERR_TABLE_FULL; /* reject the input (spec §6.3) */
    memset(slot, 0, (size_t)t.slot_size);
    memcpy(slot, rec, rec_len);
    return tbl_insert(d, &t, slot);
}

acctdrv_err acctdrv_delete_account(acctdrv_drive *d, const uint8_t addr[20],
                                   int force, int *deleted)
{
    acct_table t;
    uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
    uint64_t i;
    int found;
    acctdrv_err err;
    if (deleted)
        *deleted = 0;
    if (is_zero(addr, 20))
        return ACCTDRV_ERR_ZERO_ADDRESS;
    accounts_table(d, &t);
    err = tbl_lookup(d, &t, addr, 20, &i, slot, &found);
    if (err || !found)
        return err; /* absence is not an error */
    if (!force) {
        /* Compact records store the nonce as uint32 at offset 20 (spec §7). */
        int nonzero_nonce = drive_compact(d) ? !is_zero(slot + 20, 4)
                                             : !is_zero(slot + 24, 8);
        if (nonzero_nonce)
            return ACCTDRV_ERR_NONCE_PROTECTED; /* spec §7 replay protection */
    }
    if (deleted)
        *deleted = 1;
    return tbl_remove(d, &t, i);
}

/* ---------------------------------------------------------------- registry */

/* Read registry entry id: 20-byte address, width byte (spec §8). */
static acctdrv_err token_entry(const acctdrv_drive *d, uint64_t id,
                               uint8_t addr[20], unsigned *width)
{
    uint8_t e[32];
    acctdrv_err err = store_read(d, d->geo.registry_offset + 32 * id, e, 32);
    if (err)
        return err;
    if (addr)
        memcpy(addr, e, 20);
    if (width)
        *width = e[20];
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_token_at(const acctdrv_drive *d, uint16_t id,
                             acctdrv_token *out)
{
    unsigned w;
    acctdrv_err err;
    if ((uint64_t)id >= d->token_count)
        return ACCTDRV_ERR_UNKNOWN_TOKEN;
    err = token_entry(d, id, out->address, &w);
    if (err)
        return err;
    out->width = (int)w;
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_token_by_address(const acctdrv_drive *d,
                                     const uint8_t addr[20], uint16_t *id,
                                     acctdrv_token *out, int *found)
{
    uint64_t i;
    *found = 0;
    for (i = 0; i < d->token_count; i++) {
        uint8_t a[20];
        unsigned w;
        acctdrv_err err = token_entry(d, i, a, &w);
        if (err)
            return err;
        if (memcmp(a, addr, 20) == 0) {
            if (id)
                *id = (uint16_t)i;
            if (out) {
                memcpy(out->address, a, 20);
                out->width = (int)w;
            }
            *found = 1;
            return ACCTDRV_OK;
        }
    }
    return ACCTDRV_OK;
}

acctdrv_err acctdrv_register_token(acctdrv_drive *d, const uint8_t token[20],
                                   int width, uint16_t *id)
{
    uint64_t n;
    int dup;
    uint8_t e[32];
    acctdrv_err err;
    if (d->geo.registry_offset == 0)
        return ACCTDRV_ERR_PROFILE;
    if (width != 8 && width != 16 && width != 32)
        return ACCTDRV_ERR_BAD_WIDTH;
    err = acctdrv_token_by_address(d, token, NULL, NULL, &dup);
    if (err)
        return err;
    if (dup)
        return ACCTDRV_ERR_DUPLICATE_TOKEN;
    n = d->token_count;
    if (n >= d->geo.registry_capacity)
        return ACCTDRV_ERR_REGISTRY_FULL; /* reject the input (spec §8) */
    if (d->geo.profile == ACCTDRV_PROFILE_WIDE) {
        /* The sum of widths must not exceed slotSize − 64 (spec §7). */
        uint32_t sum = 64;
        uint64_t i;
        for (i = 0; i < n; i++) {
            unsigned w;
            err = token_entry(d, i, NULL, &w);
            if (err)
                return err;
            sum += w;
        }
        if (sum + (uint32_t)width > d->geo.slot_size)
            return ACCTDRV_ERR_TABLE_FULL; /* column overflow: reject */
    }
    if (d->geo.profile == ACCTDRV_PROFILE_SPARSE &&
        d->geo.sparse_slot_size == 32 && width != 8)
        return ACCTDRV_ERR_BAD_WIDTH; /* 32-byte sparse slots (spec §9) */
    if (drive_compact(d) && width != 8)
        return ACCTDRV_ERR_BAD_WIDTH; /* compact record: uint64 only (§7) */
    memset(e, 0, sizeof e);
    memcpy(e, token, 20);
    e[20] = (uint8_t)width;
    err = store_write(d, d->geo.registry_offset + 32 * n, e, 32);
    if (err)
        return err;
    err = write_counter(d, OFF_TOKEN_COUNT, n + 1);
    if (err)
        return err;
    d->token_count = n + 1;
    if (id)
        *id = (uint16_t)n;
    return ACCTDRV_OK;
}

/* ------------------------------------------------------------ token balances */

/*
 * Wide profile: column offset of token id and its width. The column for id
 * i is width_i bytes at 64 + Σ_{j<i} width_j (spec §7).
 */
static acctdrv_err wide_column(const acctdrv_drive *d, uint16_t id,
                               uint32_t *col, unsigned *width)
{
    uint32_t c = 64;
    unsigned w = 0;
    uint64_t j;
    for (j = 0; j <= (uint64_t)id; j++) {
        acctdrv_err err = token_entry(d, j, NULL, &w);
        if (err)
            return err;
        if (j < (uint64_t)id)
            c += w;
    }
    *col = c;
    *width = w;
    return ACCTDRV_OK;
}

/* Sparse-table key: 2-byte LE token id ‖ 20-byte address (spec §6.1). */
static void sparse_key(uint16_t id, const uint8_t addr[20], uint8_t out[22])
{
    put_le16(out, id);
    memcpy(out + 2, addr, 20);
}

/* value must not exceed the token's width: high 32−width bytes zero. */
static int fits_width(const uint8_t value[32], unsigned width)
{
    return is_zero(value, 32 - width);
}

acctdrv_err acctdrv_get_token_balance(const acctdrv_drive *d,
                                      const uint8_t addr[20], uint16_t id,
                                      uint8_t out[32])
{
    acctdrv_err err;
    memset(out, 0, 32);
    if (is_zero(addr, 20))
        return ACCTDRV_ERR_ZERO_ADDRESS;
    if ((uint64_t)id >= d->token_count)
        return ACCTDRV_ERR_UNKNOWN_TOKEN;
    switch (d->geo.profile) {
    case ACCTDRV_PROFILE_WIDE: {
        acct_table t;
        uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
        uint64_t i;
        int found;
        uint32_t col;
        unsigned w;
        accounts_table(d, &t);
        err = tbl_lookup(d, &t, addr, 20, &i, slot, &found);
        if (err || !found)
            return err; /* absent account: zero balance */
        err = wide_column(d, id, &col, &w);
        if (err)
            return err;
        memcpy(out + 32 - w, slot + col, w);
        return ACCTDRV_OK;
    }
    case ACCTDRV_PROFILE_SPARSE: {
        acct_table t;
        uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
        uint8_t key[22];
        uint64_t i;
        int found;
        sparse_table(d, &t);
        sparse_key(id, addr, key);
        err = tbl_lookup(d, &t, key, 22, &i, slot, &found);
        if (err || !found)
            return err; /* absent record: zero balance (spec §9) */
        if (d->geo.sparse_slot_size == 32)
            memcpy(out + 24, slot + 24, 8);
        else
            memcpy(out, slot + 32, 32);
        return ACCTDRV_OK;
    }
    default:
        return ACCTDRV_ERR_PROFILE;
    }
}

acctdrv_err acctdrv_set_token_balance(acctdrv_drive *d,
                                      const uint8_t addr[20], uint16_t id,
                                      const uint8_t value[32])
{
    unsigned width;
    acctdrv_err err;
    if (is_zero(addr, 20))
        return ACCTDRV_ERR_ZERO_ADDRESS;
    if ((uint64_t)id >= d->token_count)
        return ACCTDRV_ERR_UNKNOWN_TOKEN;
    err = token_entry(d, id, NULL, &width);
    if (err)
        return err;
    if (!fits_width(value, width))
        return ACCTDRV_ERR_OVERFLOW; /* reject the input (spec §8) */
    switch (d->geo.profile) {
    case ACCTDRV_PROFILE_WIDE: {
        acct_table t;
        uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
        uint64_t i, n;
        int found;
        uint32_t col;
        unsigned w;
        accounts_table(d, &t);
        err = tbl_lookup(d, &t, addr, 20, &i, slot, &found);
        if (err)
            return err;
        err = wide_column(d, id, &col, &w);
        if (err)
            return err;
        if (!found) {
            if (is_zero(value, 32))
                return ACCTDRV_OK; /* zero on absent account: no-op */
            err = read_counter(d, t.count_off, &n);
            if (err)
                return err;
            if (n >= t.limit)
                return ACCTDRV_ERR_TABLE_FULL;
            /* Auto-create: zero nonce and native balance (spec §7). */
            memset(slot, 0, (size_t)t.slot_size);
            memcpy(slot, addr, 20);
            memcpy(slot + col, value + 32 - w, w);
            return tbl_insert(d, &t, slot);
        }
        memcpy(slot + col, value + 32 - w, w); /* zero keeps the record */
        return write_slot(d, &t, i, slot);
    }
    case ACCTDRV_PROFILE_SPARSE: {
        acct_table t;
        uint8_t slot[ACCTDRV_MAX_SLOT_SIZE];
        uint8_t key[22];
        uint64_t i, n;
        int found;
        sparse_table(d, &t);
        sparse_key(id, addr, key);
        err = tbl_lookup(d, &t, key, 22, &i, slot, &found);
        if (err)
            return err;
        if (is_zero(value, 32)) {
            if (!found)
                return ACCTDRV_OK;
            return tbl_remove(d, &t, i); /* zero deletes (spec §9) */
        }
        if (found) {
            if (d->geo.sparse_slot_size == 32)
                memcpy(slot + 24, value + 24, 8);
            else
                memcpy(slot + 32, value, 32);
            return write_slot(d, &t, i, slot);
        }
        err = read_counter(d, t.count_off, &n);
        if (err)
            return err;
        if (n >= t.limit)
            return ACCTDRV_ERR_TABLE_FULL;
        memset(slot, 0, (size_t)t.slot_size);
        put_le16(slot, id);
        memcpy(slot + 4, addr, 20);
        if (d->geo.sparse_slot_size == 32)
            memcpy(slot + 24, value + 24, 8);
        else
            memcpy(slot + 32, value, 32);
        return tbl_insert(d, &t, slot);
    }
    default:
        return ACCTDRV_ERR_PROFILE;
    }
}
