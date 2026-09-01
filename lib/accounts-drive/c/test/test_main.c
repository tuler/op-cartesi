/*
 * test_main.c — conformance tests for the C accounts-drive library.
 *
 * Part (a): native pins — the spec Appendix B keccak / home-slot vectors
 * and record encodings.
 * Part (b): replays every scenario of ../testdata/golden.json (compiled
 * into golden_vectors.h by test/gen_golden.py) against a zeroed memory
 * buffer, asserting each op's expected outcome, every check, and finally
 * the SHA-256 of the entire drive image.
 */
#include <stdio.h>
#include <string.h>

#include "accounts_drive.h"
#include "golden_vectors.h"

static int failures;

/* --------------------------------------------------------------- SHA-256 */

static const uint32_t sha_k[64] = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1,
    0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
    0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786,
    0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147,
    0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
    0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
    0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a,
    0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
    0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2};

static uint32_t ror32(uint32_t x, int n)
{
    return (x >> n) | (x << (32 - n));
}

static void sha256_block(uint32_t h[8], const uint8_t p[64])
{
    uint32_t w[64], a, b, c, d, e, f, g, hh;
    int i;
    for (i = 0; i < 16; i++)
        w[i] = (uint32_t)p[4 * i] << 24 | (uint32_t)p[4 * i + 1] << 16 |
               (uint32_t)p[4 * i + 2] << 8 | (uint32_t)p[4 * i + 3];
    for (i = 16; i < 64; i++) {
        uint32_t s0 =
            ror32(w[i - 15], 7) ^ ror32(w[i - 15], 18) ^ (w[i - 15] >> 3);
        uint32_t s1 =
            ror32(w[i - 2], 17) ^ ror32(w[i - 2], 19) ^ (w[i - 2] >> 10);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    a = h[0], b = h[1], c = h[2], d = h[3];
    e = h[4], f = h[5], g = h[6], hh = h[7];
    for (i = 0; i < 64; i++) {
        uint32_t s1 = ror32(e, 6) ^ ror32(e, 11) ^ ror32(e, 25);
        uint32_t ch = (e & f) ^ (~e & g);
        uint32_t t1 = hh + s1 + ch + sha_k[i] + w[i];
        uint32_t s0 = ror32(a, 2) ^ ror32(a, 13) ^ ror32(a, 22);
        uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        uint32_t t2 = s0 + maj;
        hh = g, g = f, f = e, e = d + t1;
        d = c, c = b, b = a, a = t1 + t2;
    }
    h[0] += a, h[1] += b, h[2] += c, h[3] += d;
    h[4] += e, h[5] += f, h[6] += g, h[7] += hh;
}

static void sha256(const uint8_t *p, size_t len, uint8_t out[32])
{
    uint32_t h[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
                     0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
    uint64_t bitlen = (uint64_t)len * 8;
    uint8_t tail[128];
    size_t i, rem, tlen;
    int j;
    for (i = 0; i + 64 <= len; i += 64)
        sha256_block(h, p + i);
    rem = len - i;
    memset(tail, 0, sizeof tail);
    memcpy(tail, p + i, rem);
    tail[rem] = 0x80;
    tlen = rem < 56 ? 64 : 128;
    for (j = 0; j < 8; j++)
        tail[tlen - 1 - j] = (uint8_t)(bitlen >> (8 * j));
    sha256_block(h, tail);
    if (tlen == 128)
        sha256_block(h, tail + 64);
    for (j = 0; j < 8; j++) {
        out[4 * j] = (uint8_t)(h[j] >> 24);
        out[4 * j + 1] = (uint8_t)(h[j] >> 16);
        out[4 * j + 2] = (uint8_t)(h[j] >> 8);
        out[4 * j + 3] = (uint8_t)h[j];
    }
}

/* --------------------------------------------------------------- helpers */

#define FAIL(...)                                                             \
    do {                                                                      \
        printf("FAIL " __VA_ARGS__);                                          \
        printf("\n");                                                         \
        failures++;                                                           \
    } while (0)

static void tohex(const uint8_t *b, size_t n, char *out)
{
    static const char dig[] = "0123456789abcdef";
    size_t i;
    for (i = 0; i < n; i++) {
        out[2 * i] = dig[b[i] >> 4];
        out[2 * i + 1] = dig[b[i] & 15];
    }
    out[2 * n] = 0;
}

static int hexval(char c)
{
    if (c >= '0' && c <= '9')
        return c - '0';
    if (c >= 'a' && c <= 'f')
        return c - 'a' + 10;
    return -1;
}

static void unhex(const char *s, uint8_t *out, size_t n)
{
    size_t i;
    for (i = 0; i < n; i++)
        out[i] = (uint8_t)(hexval(s[2 * i]) << 4 | hexval(s[2 * i + 1]));
}

/* One drive image buffer, big enough for every scenario. */
static uint8_t image[GOLDEN_MAX_DRIVE_LENGTH];

/* ---------------------------------------------- (a) keccak and home pins */

static void check_keccak(const char *what, const uint8_t *data, size_t len,
                         const char *want)
{
    uint8_t h[32];
    char got[65];
    acctdrv_keccak256(data, len, h);
    tohex(h, 32, got);
    if (strcmp(got, want) != 0)
        FAIL("keccak256(%s) = %s, want %s", what, got, want);
}

static uint64_t home_calc(const uint8_t *key, size_t klen, uint64_t cap)
{
    uint8_t buf[54], h[32];
    uint64_t v = 0;
    int i;
    memset(buf, 0, 32); /* zero seed */
    memcpy(buf + 32, key, klen);
    acctdrv_keccak256(buf, 32 + klen, h);
    for (i = 7; i >= 0; i--)
        v = (v << 8) | h[i];
    return v % cap;
}

static void test_keccak_and_homes(void)
{
    uint8_t buf[54];
    static const struct {
        uint8_t addr;
        uint64_t home;
    } homes[] = {{0xaa, 5}, {0xbb, 1}, {0xcc, 7}, {0xdd, 1}, {0xee, 1}};
    size_t i;

    memset(buf, 0, sizeof buf);
    check_keccak("\"\"", buf, 0,
                 "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d"
                 "85a470");

    memset(buf, 0, 32);
    memset(buf + 32, 0xbb, 20);
    check_keccak("seed || bb..", buf, 52,
                 "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211d"
                 "ff1a92");

    /* Sparse key: token id 3, address bb.. (spec Appendix B). */
    memset(buf, 0, 32);
    buf[32] = 0x03;
    buf[33] = 0x00;
    memset(buf + 34, 0xbb, 20);
    check_keccak("seed || sparse key", buf, 54,
                 "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26"
                 "d147d3");

    for (i = 0; i < sizeof homes / sizeof homes[0]; i++) {
        uint8_t a[20];
        uint64_t got;
        memset(a, homes[i].addr, 20);
        got = home_calc(a, 20, 8);
        if (got != homes[i].home)
            FAIL("home(%02x..) mod 8 = %llu, want %llu", homes[i].addr,
                 (unsigned long long)got, (unsigned long long)homes[i].home);
    }
    {
        uint8_t a[20];
        memset(a, 0xbb, 20);
        if (home_calc(a, 20, 1u << 19) != 342121)
            FAIL("home(bb..) mod 2^19 != 342121");
    }
    {
        uint8_t k[22];
        k[0] = 0x03;
        k[1] = 0x00;
        memset(k + 2, 0xbb, 20);
        if (home_calc(k, 22, 1u << 19) != 117477)
            FAIL("sparse home(id 3, bb..) mod 2^19 != 117477");
    }
    printf("PASS keccak and home-slot vectors\n");
}

/* --------------------------------------------- (a) record encoding pins */

static void test_account_record_encoding(void)
{
    acctdrv_memstore ms;
    acctdrv_store st;
    acctdrv_config cfg;
    acctdrv_drive d;
    acctdrv_err err;
    uint8_t bb[20], bal[32], want[64];

    memset(image, 0, 1u << 16);
    ms.data = image;
    ms.length = 1u << 16;
    st = acctdrv_mem_store(&ms);
    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = 1u << 16;
    cfg.capacity = 8;
    err = acctdrv_format(&d, &st, &cfg);
    if (err) {
        FAIL("record encoding: format: %s", acctdrv_strerror(err));
        return;
    }
    memset(bb, 0xbb, 20);
    memset(bal, 0, 32);
    bal[31] = 5;
    err = acctdrv_set_account(&d, bb, 7, bal);
    if (err) {
        FAIL("record encoding: setAccount: %s", acctdrv_strerror(err));
        return;
    }
    unhex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000"
          "0000000000000000000000000000000000000000000000000000000000000005",
          want, 64);
    /* home(bb..) mod 8 = 1: the record sits in slot 1. */
    if (memcmp(image + 4096 + 64 * 1, want, 64) != 0) {
        char got[129];
        tohex(image + 4096 + 64, 64, got);
        FAIL("account record = %s", got);
        return;
    }
    printf("PASS account record encoding\n");
}

static void test_sparse32_record_encoding(void)
{
    acctdrv_memstore ms;
    acctdrv_store st;
    acctdrv_config cfg;
    acctdrv_drive d;
    acctdrv_err err;
    uint8_t bb[20], val[32], want[32];
    int i;

    memset(image, 0, 1u << 16);
    ms.data = image;
    ms.length = 1u << 16;
    st = acctdrv_mem_store(&ms);
    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = 1u << 16;
    cfg.capacity = 8;
    cfg.profile = ACCTDRV_PROFILE_SPARSE;
    cfg.registry_offset = 0x100;
    cfg.registry_capacity = 8;
    cfg.sparse_offset = 8192;
    cfg.sparse_capacity = 8;
    cfg.sparse_slot_size = 32;
    err = acctdrv_format(&d, &st, &cfg);
    if (err) {
        FAIL("sparse32 encoding: format: %s", acctdrv_strerror(err));
        return;
    }
    for (i = 0; i < 4; i++) {
        uint8_t tok[20];
        memset(tok, i + 1, 20);
        err = acctdrv_register_token(&d, tok, 8, NULL);
        if (err) {
            FAIL("sparse32 encoding: registerToken: %s",
                 acctdrv_strerror(err));
            return;
        }
    }
    memset(bb, 0xbb, 20);
    memset(val, 0, 32);
    val[29] = 0x0f; /* 1000000 = 0x0f4240 */
    val[30] = 0x42;
    val[31] = 0x40;
    err = acctdrv_set_token_balance(&d, bb, 3, val);
    if (err) {
        FAIL("sparse32 encoding: setTokenBalance: %s", acctdrv_strerror(err));
        return;
    }
    unhex("03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240",
          want, 32);
    /* sparse home(id 3, bb..): LE64 = 0xbfc3cb1987d1cae5, mod 8 = 5. */
    if (memcmp(image + 8192 + 32 * 5, want, 32) != 0) {
        char got[65];
        tohex(image + 8192 + 32 * 5, 32, got);
        FAIL("sparse32 record = %s", got);
        return;
    }
    printf("PASS sparse 32-byte record encoding\n");
}

static void test_compact_record_encoding(void)
{
    acctdrv_memstore ms;
    acctdrv_store st;
    acctdrv_config cfg;
    acctdrv_drive d;
    acctdrv_account a;
    acctdrv_err err;
    uint8_t bb[20], tok[20], bal[32], big[32], want[32];
    int found, deleted;

    memset(image, 0, 1u << 16);
    ms.data = image;
    ms.length = 1u << 16;
    st = acctdrv_mem_store(&ms);
    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = 1u << 16;
    cfg.capacity = 8;
    cfg.slot_size = 32; /* profile 0: the compact record (spec §7) */
    cfg.registry_offset = 0x100;
    cfg.registry_capacity = 2;
    err = acctdrv_format(&d, &st, &cfg);
    if (err) {
        FAIL("compact: format: %s", acctdrv_strerror(err));
        return;
    }
    memset(bb, 0xbb, 20);
    memset(bal, 0, 32);
    bal[31] = 5;
    err = acctdrv_set_account(&d, bb, 7, bal);
    if (err) {
        FAIL("compact: setAccount: %s", acctdrv_strerror(err));
        return;
    }
    unhex("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb070000000000000000000005",
          want, 32);
    /* home(bb..) mod 8 = 1: the record sits in slot 1. */
    if (memcmp(image + 4096 + 32 * 1, want, 32) != 0) {
        char got[65];
        tohex(image + 4096 + 32 * 1, 32, got);
        FAIL("compact record = %s", got);
        return;
    }
    err = acctdrv_get_account(&d, bb, &a, &found);
    if (err || !found || a.nonce != 7 || memcmp(a.balance, bal, 32) != 0) {
        FAIL("compact: getAccount: err=%s found=%d nonce=%llu",
             acctdrv_strerror(err), found, (unsigned long long)a.nonce);
        return;
    }
    /* Nonce past uint32 must be refused (spec §7). */
    err = acctdrv_set_account(&d, bb, 0x100000000ull, bal);
    if (err != ACCTDRV_ERR_OVERFLOW)
        FAIL("compact: nonce past uint32: %s, want overflow",
             acctdrv_strerror(err));
    /* Balance past uint64 (any nonzero byte above the low 8) too. */
    memset(big, 0, 32);
    big[23] = 1; /* 2^64 */
    err = acctdrv_set_account(&d, bb, 7, big);
    if (err != ACCTDRV_ERR_OVERFLOW)
        FAIL("compact: balance past uint64: %s, want overflow",
             acctdrv_strerror(err));
    /* Registry entries on a compact drive must carry width 8 (spec §7). */
    memset(tok, 0x01, 20);
    err = acctdrv_register_token(&d, tok, 32, NULL);
    if (err != ACCTDRV_ERR_BAD_WIDTH)
        FAIL("compact: registerToken width 32: %s, want bad width",
             acctdrv_strerror(err));
    err = acctdrv_register_token(&d, tok, 8, NULL);
    if (err)
        FAIL("compact: registerToken width 8: %s", acctdrv_strerror(err));
    /* Delete protection reads the uint32 nonce (spec §7). */
    err = acctdrv_delete_account(&d, bb, 0, &deleted);
    if (err != ACCTDRV_ERR_NONCE_PROTECTED)
        FAIL("compact: delete without force: %s, want nonce protected",
             acctdrv_strerror(err));
    err = acctdrv_delete_account(&d, bb, 1, &deleted);
    if (err || !deleted) {
        FAIL("compact: forced delete: %s deleted=%d", acctdrv_strerror(err),
             deleted);
        return;
    }
    printf("PASS compact record encoding\n");
}

static void test_compact_slot_size_only_profile0(void)
{
    acctdrv_memstore ms;
    acctdrv_store st;
    acctdrv_config cfg;
    acctdrv_drive d;
    acctdrv_err err;

    memset(image, 0, 1u << 16);
    ms.data = image;
    ms.length = 1u << 16;
    st = acctdrv_mem_store(&ms);

    /* Format: 32-byte account slots are refused outside profile 0. */
    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = 1u << 16;
    cfg.capacity = 8;
    cfg.slot_size = 32;
    cfg.profile = ACCTDRV_PROFILE_SPARSE;
    cfg.registry_offset = 0x100;
    cfg.registry_capacity = 2;
    cfg.sparse_offset = 8192;
    cfg.sparse_capacity = 8;
    err = acctdrv_format(&d, &st, &cfg);
    if (err != ACCTDRV_ERR_CONFIG)
        FAIL("sparse format with 32-byte slots: %s, want config error",
             acctdrv_strerror(err));
    cfg.profile = ACCTDRV_PROFILE_WIDE;
    err = acctdrv_format(&d, &st, &cfg);
    if (err != ACCTDRV_ERR_CONFIG)
        FAIL("wide format with 32-byte slots: %s, want config error",
             acctdrv_strerror(err));

    /* Open: a sparse header patched to slotSize 32 must not open. */
    memset(image, 0, 1u << 16);
    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = 1u << 16;
    cfg.capacity = 8;
    cfg.profile = ACCTDRV_PROFILE_SPARSE;
    cfg.registry_offset = 0x100;
    cfg.registry_capacity = 2;
    cfg.sparse_offset = 8192;
    cfg.sparse_capacity = 8;
    err = acctdrv_format(&d, &st, &cfg);
    if (err) {
        FAIL("sparse format: %s", acctdrv_strerror(err));
        return;
    }
    image[0x54] = 32; /* slotSize field (LE32 at 0x54): 64 -> 32 */
    err = acctdrv_open(&d, &st);
    if (err != ACCTDRV_ERR_CORRUPT)
        FAIL("open sparse drive with 32-byte slots: %s, want corrupt",
             acctdrv_strerror(err));
    printf("PASS compact slot size only in profile 0\n");
}

/* ------------------------------------------------- (b) the golden replay */

static const char *op_name(golden_op_kind k)
{
    switch (k) {
    case GOP_SET_ACCOUNT:
        return "setAccount";
    case GOP_DELETE_ACCOUNT:
        return "deleteAccount";
    case GOP_REGISTER_TOKEN:
        return "registerToken";
    case GOP_SET_TOKEN_BALANCE:
        return "setTokenBalance";
    }
    return "?";
}

static void run_scenario(const golden_scenario *sc)
{
    acctdrv_memstore ms;
    acctdrv_store st;
    acctdrv_config cfg;
    acctdrv_drive d;
    acctdrv_err err;
    size_t i;
    int bad = failures;

    memset(image, 0, sizeof image);
    ms.data = image;
    ms.length = sc->drive_length;
    st = acctdrv_mem_store(&ms);

    memset(&cfg, 0, sizeof cfg);
    cfg.drive_length = sc->drive_length;
    cfg.capacity = sc->capacity;
    cfg.load_limit = sc->load_limit;
    memcpy(cfg.seed, sc->seed, 32);
    cfg.profile = sc->profile;
    cfg.slot_size = sc->slot_size;
    cfg.table_offset = sc->table_offset;
    cfg.registry_offset = sc->registry_offset;
    cfg.registry_capacity = sc->registry_capacity;
    cfg.sparse_offset = sc->sparse_offset;
    cfg.sparse_capacity = sc->sparse_capacity;
    cfg.sparse_load_limit = sc->sparse_load_limit;
    cfg.sparse_slot_size = sc->sparse_slot_size;

    err = acctdrv_format(&d, &st, &cfg);
    if (err) {
        FAIL("%s: format: %s", sc->name, acctdrv_strerror(err));
        return;
    }

    for (i = 0; i < sc->nops; i++) {
        const golden_op *op = &sc->ops[i];
        switch (op->op) {
        case GOP_SET_ACCOUNT:
            err = acctdrv_set_account(&d, op->addr, op->nonce, op->value);
            break;
        case GOP_DELETE_ACCOUNT:
            err = acctdrv_delete_account(&d, op->addr, op->force, NULL);
            break;
        case GOP_REGISTER_TOKEN:
            err = acctdrv_register_token(&d, op->token, op->width, NULL);
            break;
        case GOP_SET_TOKEN_BALANCE:
            err = acctdrv_set_token_balance(&d, op->addr, op->id, op->value);
            break;
        default:
            FAIL("%s op %zu: unknown op", sc->name, i);
            return;
        }
        if ((int)err != op->expect) {
            FAIL("%s op %zu (%s): outcome %s, want %s", sc->name, i,
                 op_name(op->op), acctdrv_strerror(err),
                 acctdrv_strerror((acctdrv_err)op->expect));
            return;
        }
    }

    for (i = 0; i < sc->nchecks; i++) {
        const golden_check *c = &sc->checks[i];
        switch (c->check) {
        case GCHK_ACCOUNT: {
            acctdrv_account a;
            int found;
            err = acctdrv_get_account(&d, c->addr, &a, &found);
            if (err) {
                FAIL("%s check %zu (account): %s", sc->name, i,
                     acctdrv_strerror(err));
                break;
            }
            if (found != c->found ||
                (found && (a.nonce != c->nonce ||
                           memcmp(a.balance, c->value, 32) != 0)))
                FAIL("%s check %zu (account): found=%d nonce=%llu mismatch",
                     sc->name, i, found, (unsigned long long)a.nonce);
            break;
        }
        case GCHK_TOKEN_BALANCE: {
            uint8_t v[32];
            err = acctdrv_get_token_balance(&d, c->addr, c->id, v);
            if (err) {
                FAIL("%s check %zu (tokenBalance): %s", sc->name, i,
                     acctdrv_strerror(err));
                break;
            }
            if (memcmp(v, c->value, 32) != 0) {
                char got[65], want[65];
                tohex(v, 32, got);
                tohex(c->value, 32, want);
                FAIL("%s check %zu (tokenBalance id %u): %s, want %s",
                     sc->name, i, c->id, got, want);
            }
            break;
        }
        case GCHK_LIVE_COUNT: {
            uint64_t n;
            err = c->sparse ? acctdrv_sparse_live_count(&d, &n)
                            : acctdrv_live_count(&d, &n);
            if (err) {
                FAIL("%s check %zu (liveCount): %s", sc->name, i,
                     acctdrv_strerror(err));
                break;
            }
            if (n != c->count)
                FAIL("%s check %zu: %s liveCount %llu, want %llu", sc->name,
                     i, c->sparse ? "sparse" : "accounts",
                     (unsigned long long)n, (unsigned long long)c->count);
            break;
        }
        case GCHK_SLOT: {
            uint64_t base = c->sparse ? sc->sparse_offset : sc->table_offset;
            uint64_t ssz = c->sparse ? sc->sparse_slot_size : sc->slot_size;
            if (c->slot_len != ssz) {
                FAIL("%s check %zu: slot check length %zu != slot size %llu",
                     sc->name, i, c->slot_len, (unsigned long long)ssz);
                break;
            }
            if (memcmp(image + base + ssz * c->index, c->slot_bytes,
                       c->slot_len) != 0) {
                char got[2 * ACCTDRV_MAX_SLOT_SIZE + 1];
                tohex(image + base + ssz * c->index, c->slot_len, got);
                FAIL("%s check %zu: slot %llu = %s", sc->name, i,
                     (unsigned long long)c->index, got);
            }
            break;
        }
        default:
            FAIL("%s check %zu: unknown check", sc->name, i);
            break;
        }
    }

    {
        uint8_t sum[32];
        sha256(image, (size_t)sc->drive_length, sum);
        if (memcmp(sum, sc->sha256, 32) != 0) {
            char got[65], want[65];
            tohex(sum, 32, got);
            tohex(sc->sha256, 32, want);
            FAIL("%s: drive sha256 = %s, want %s", sc->name, got, want);
        }
    }

    if (failures == bad)
        printf("PASS golden: %s (%zu ops, %zu checks, drive hash ok)\n",
               sc->name, sc->nops, sc->nchecks);
}

int main(void)
{
    size_t i;
    test_keccak_and_homes();
    test_account_record_encoding();
    test_sparse32_record_encoding();
    test_compact_record_encoding();
    test_compact_slot_size_only_profile0();
    for (i = 0; i < GOLDEN_NUM_SCENARIOS; i++)
        run_scenario(&golden_scenarios[i]);
    if (failures) {
        printf("FAILED: %d failure(s)\n", failures);
        return 1;
    }
    printf("OK: all tests passed (%d golden scenarios)\n",
           GOLDEN_NUM_SCENARIOS);
    return 0;
}
