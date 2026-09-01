#!/usr/bin/env python3
"""Tests for accounts_drive (Cartesi Accounts Drive Format v1).

Run directly:  python3 test_accounts_drive.py

Covers the spec's Appendix B vectors (keccak, home slots, record encodings,
the probe walkthrough with its backward-shift delete), the error taxonomy,
the stores, and -- the conformance core -- a golden harness replaying every
scenario in ../testdata/golden.json and matching the SHA-256 of the entire
resulting drive image.
"""

import hashlib
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import accounts_drive as ad

GOLDEN_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                           "..", "testdata", "golden.json")

ZERO_SEED = bytes(32)


def rep_addr(b):
    """Address 'NN..': the byte b repeated 20 times (spec Appendix B)."""
    return bytes([b]) * 20


class TestKeccak(unittest.TestCase):
    def test_empty(self):
        self.assertEqual(
            ad.keccak256(b"").hex(),
            "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
        self.assertEqual(ad.keccak256(), ad.keccak256(b""))

    def test_seed_bb(self):
        self.assertEqual(
            ad.keccak256(ZERO_SEED, rep_addr(0xBB)).hex(),
            "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92")

    def test_sparse_key(self):
        self.assertEqual(
            ad.keccak256(ZERO_SEED, ad.sparse_key(3, rep_addr(0xBB))).hex(),
            "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3")

    def test_chunking_and_multiblock(self):
        # Chunk boundaries must not matter, including across the 136-byte rate.
        data = bytes(range(256)) * 3
        whole = ad.keccak256(data)
        self.assertEqual(ad.keccak256(data[:1], data[1:137], data[137:]), whole)
        self.assertEqual(ad.keccak256(*[data[i:i + 7] for i in range(0, len(data), 7)]),
                         whole)

    def test_pure_fallback_agrees(self):
        # The pure-Python fallback stays pinned even when pycryptodome is
        # installed, so a guest without packages computes the same drive.
        self.assertEqual(
            ad._keccak256_pure(b"").hex(),
            "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
        self.assertEqual(
            ad._keccak256_pure(ZERO_SEED, rep_addr(0xBB)).hex(),
            "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92")
        data = bytes(range(256)) * 3
        self.assertEqual(ad._keccak256_pure(data), ad.keccak256(data))


class TestHomeSlots(unittest.TestCase):
    def test_mod8(self):
        want = {0xAA: 5, 0xBB: 1, 0xCC: 7, 0xDD: 1, 0xEE: 1}
        for b, w in want.items():
            self.assertEqual(ad.home_slot(ZERO_SEED, rep_addr(b), 8), w,
                             f"home({b:02x}..) mod 8")

    def test_large_capacity(self):
        self.assertEqual(ad.home_slot(ZERO_SEED, rep_addr(0xBB), 1 << 19), 342121)
        self.assertEqual(
            ad.home_slot(ZERO_SEED, ad.sparse_key(3, rep_addr(0xBB)), 1 << 19),
            117477)


class TestRecordEncodings(unittest.TestCase):
    def test_account_record(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(drive_length=1 << 16, capacity=8))
        d.set_account(rep_addr(0xBB), 7, 5)
        want = bytes.fromhex(
            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000"
            "0000000000000000000000000000000000000000000000000000000000000005")
        got = s.read_at(4096 + 64 * 1, 64)  # home(bb..) mod 8 = 1
        self.assertEqual(got, want)

    def test_sparse32_record(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(
            drive_length=1 << 16, capacity=8, profile=ad.PROFILE_SPARSE,
            registry_offset=0x100, registry_capacity=8,
            sparse_offset=8192, sparse_capacity=8, sparse_slot_size=32))
        for i in range(4):
            d.register_token(rep_addr(i + 1), 8)
        d.set_token_balance(rep_addr(0xBB), 3, 1000000)
        # sparse home(id 3, bb..) mod 8: LE64(e5cad187...) = 0xbfc3cb1987d1cae5 -> 5
        got = s.read_at(8192 + 32 * 5, 32)
        want = bytes.fromhex(
            "03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240")
        self.assertEqual(got, want)


class TestWalkthrough(unittest.TestCase):
    """Spec Appendix B probe walkthrough at C=8, including the slot
    placements after the backward-shift delete."""

    def test_walkthrough(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(drive_length=1 << 16, capacity=8, load_limit=7))

        def slot_addr(i):
            return s.read_at(4096 + 64 * i, 1)[0]

        for b in (0xBB, 0xDD, 0xEE, 0xAA):
            d.set_account(rep_addr(b), 0, 1)
        for slot, want in {1: 0xBB, 2: 0xDD, 3: 0xEE, 5: 0xAA}.items():
            self.assertEqual(slot_addr(slot), want, f"slot {slot}")
        # Absent key with home 1 terminates via the empty slot 4.
        self.assertIsNone(d.get_account(rep_addr(0x99)))
        # Delete dd: ee backward-shifts into slot 2, slot 3 zeroes.
        self.assertTrue(d.delete_account(rep_addr(0xDD)))
        self.assertEqual(slot_addr(1), 0xBB)
        self.assertEqual(slot_addr(2), 0xEE)
        self.assertEqual(s.read_at(4096 + 64 * 3, 64), bytes(64), "slot 3 zeroed")
        self.assertEqual(slot_addr(5), 0xAA)
        for b in (0xBB, 0xEE, 0xAA):
            self.assertIsNotNone(d.get_account(rep_addr(b)), f"{b:02x}.. lost")
        self.assertIsNone(d.get_account(rep_addr(0xDD)))
        self.assertEqual(d.live_count(), 3)


class TestErrorKinds(unittest.TestCase):
    def kind(self, fn, *args, **kw):
        try:
            fn(*args, **kw)
            return "ok"
        except ad.AccountsDriveError as e:
            return e.kind

    def test_table_full_and_nonce_protection(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(drive_length=1 << 16, capacity=8, load_limit=4))
        for b in (0xAA, 0xBB, 0xCC, 0xDD):
            d.set_account(rep_addr(b), 5, 1)
        self.assertEqual(self.kind(d.set_account, rep_addr(0xEE), 0, 1), "tableFull")
        # Updating an existing account is not an insert and must still work.
        d.set_account(rep_addr(0xAA), 6, 2)
        self.assertEqual(self.kind(d.delete_account, rep_addr(0xBB)), "nonceProtected")
        self.assertTrue(d.delete_account(rep_addr(0xBB), force=True))
        d.set_account(rep_addr(0xEE), 0, 1)

    def test_wide_profile(self):
        s = ad.MemStore(1 << 18)
        d = ad.format(s, ad.Config(
            drive_length=1 << 18, capacity=64, profile=ad.PROFILE_WIDE,
            slot_size=128, registry_offset=0x100, registry_capacity=8))
        d.register_token(rep_addr(0x01), 32)
        id2 = d.register_token(rep_addr(0x02), 8)
        d.register_token(rep_addr(0x03), 8)
        # 64 + 32 + 8 + 8 = 112; another 32 would need 144 > 128.
        self.assertEqual(self.kind(d.register_token, rep_addr(0x04), 32), "tableFull")
        self.assertEqual(self.kind(d.register_token, rep_addr(0x01), 8),
                         "duplicateToken")
        self.assertEqual(self.kind(d.register_token, rep_addr(0x05), 20), "badWidth")
        user = rep_addr(0x77)
        d.set_token_balance(user, id2, 12345)   # auto-creates the record
        d.set_account(user, 9, 500)             # preserves the column
        self.assertEqual(d.get_token_balance(user, id2), 12345)
        a = d.get_account(user)
        self.assertEqual((a.nonce, a.balance), (9, 500))
        self.assertEqual(self.kind(d.set_token_balance, user, id2, 1 << 64),
                         "overflow")
        # Zeroing a wide column keeps the record.
        d.set_token_balance(user, id2, 0)
        self.assertIsNotNone(d.get_account(user))
        # Zero balance for an absent account creates nothing.
        before = d.live_count()
        d.set_token_balance(rep_addr(0x66), id2, 0)
        self.assertEqual(d.live_count(), before)
        # Reopen: registry and records survive.
        d2 = ad.open(s)
        self.assertEqual(len(d2.tokens()), 3)
        self.assertEqual(d2.get_account(user).balance, 500)

    def test_sparse_profile(self):
        s = ad.MemStore(1 << 18)
        d = ad.format(s, ad.Config(
            drive_length=1 << 18, capacity=64, profile=ad.PROFILE_SPARSE,
            registry_offset=0x100, registry_capacity=8,
            sparse_offset=1 << 17, sparse_capacity=16, sparse_load_limit=8))
        tid = d.register_token(rep_addr(0x01), 32)
        user = rep_addr(0x77)
        huge = 123456789012345678901234567890
        d.set_token_balance(user, tid, huge)
        self.assertEqual(d.get_token_balance(user, tid), huge)
        self.assertEqual(d.sparse_live_count(), 1)
        d.set_token_balance(user, tid, 0)  # zero deletes the record
        self.assertEqual(d.sparse_live_count(), 0)
        self.assertEqual(d.get_token_balance(user, tid), 0)
        self.assertEqual(self.kind(d.get_token_balance, user, 9), "unknownToken")

    def test_sparse32_requires_width8(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(
            drive_length=1 << 16, capacity=8, profile=ad.PROFILE_SPARSE,
            registry_offset=0x100, registry_capacity=8,
            sparse_offset=8192, sparse_capacity=8, sparse_slot_size=32))
        self.assertEqual(self.kind(d.register_token, rep_addr(0x01), 16), "badWidth")

    def test_registry_full_zero_address_profile(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(drive_length=1 << 16, capacity=8,
                                   registry_offset=0x100, registry_capacity=1))
        d.register_token(bytes(20), 32)  # zero address = native asset: allowed
        self.assertEqual(self.kind(d.register_token, rep_addr(0x01), 8),
                         "registryFull")
        self.assertEqual(self.kind(d.set_account, bytes(20), 0, 1), "zeroAddress")
        self.assertEqual(self.kind(d.get_token_balance, rep_addr(0x01), 0),
                         "profile")
        s2 = ad.MemStore(1 << 16)
        d2 = ad.format(s2, ad.Config(drive_length=1 << 16, capacity=8))
        self.assertEqual(self.kind(d2.register_token, rep_addr(0x01), 8), "profile")

    def test_hex_addresses_accepted(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(drive_length=1 << 16, capacity=8))
        d.set_account("0x" + "bb" * 20, 7, 5)
        a = d.get_account("bb" * 20)
        self.assertEqual((a.nonce, a.balance), (7, 5))
        self.assertEqual(a.address, rep_addr(0xBB))


class TestCompactRecord(unittest.TestCase):
    """The 32-byte profile-0 record (spec section 7): the encoding pin from
    Appendix B, the uint32 nonce and uint64 balance bounds, the registry
    width rule, and slot size 32 being profile-0-only."""

    def kind(self, fn, *args, **kw):
        try:
            fn(*args, **kw)
            return "ok"
        except ad.AccountsDriveError as e:
            return e.kind

    def test_compact_record(self):
        s = ad.MemStore(1 << 16)
        d = ad.format(s, ad.Config(
            drive_length=1 << 16, capacity=8, slot_size=32,
            registry_offset=0x100, registry_capacity=2))
        d.set_account(rep_addr(0xBB), 7, 5)
        want = bytes.fromhex(
            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb070000000000000000000005")
        got = s.read_at(4096 + 32 * 1, 32)  # home(bb..) mod 8 = 1
        self.assertEqual(got, want)
        a = d.get_account(rep_addr(0xBB))
        self.assertEqual((a.nonce, a.balance), (7, 5))
        # Bounds: uint32 nonce and uint64 balance exactly fit; one past
        # either is kind "overflow" (reject the input).
        d.set_account(rep_addr(0xCC), (1 << 32) - 1, (1 << 64) - 1)
        a = d.get_account(rep_addr(0xCC))
        self.assertEqual((a.nonce, a.balance), ((1 << 32) - 1, (1 << 64) - 1))
        self.assertEqual(self.kind(d.set_account, rep_addr(0xBB), 1 << 32, 5),
                         "overflow")
        self.assertEqual(self.kind(d.set_account, rep_addr(0xBB), 7, 1 << 64),
                         "overflow")
        # A registry entry on a compact drive must declare width 8.
        self.assertEqual(self.kind(d.register_token, rep_addr(0x01), 32),
                         "badWidth")
        d.register_token(bytes(20), 8)  # width 8, zero addr = native asset
        # The nonce protection reads the u32.
        self.assertEqual(self.kind(d.delete_account, rep_addr(0xBB)),
                         "nonceProtected")
        self.assertTrue(d.delete_account(rep_addr(0xBB), force=True))

    def test_slot_size_32_only_in_profile0(self):
        with self.assertRaises(ValueError):
            ad.format(ad.MemStore(1 << 16), ad.Config(
                drive_length=1 << 16, capacity=8, slot_size=32,
                profile=ad.PROFILE_SPARSE,
                registry_offset=0x100, registry_capacity=2,
                sparse_offset=8192, sparse_capacity=8))
        with self.assertRaises(ValueError):
            ad.format(ad.MemStore(1 << 18), ad.Config(
                drive_length=1 << 18, capacity=8, slot_size=32,
                profile=ad.PROFILE_WIDE,
                registry_offset=0x100, registry_capacity=2))


class TestStores(unittest.TestCase):
    def test_file_store(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "drive.img")
            with open(path, "wb") as f:
                f.write(bytes(1 << 16))
            with ad.FileStore(path) as fs:
                d = ad.format(fs, ad.Config(drive_length=1 << 16, capacity=8))
                d.set_account(rep_addr(0xBB), 7, 5)
                fs.sync()  # spec section 10: flush before finishing the input
            with ad.FileStore(path) as fs2:
                d2 = ad.open(fs2)
                a = d2.get_account(rep_addr(0xBB))
                self.assertEqual((a.nonce, a.balance), (7, 5))
                self.assertEqual(d2.live_count(), 1)

    def test_machine_store(self):
        # Read a formatted image back through the MachineStore adapter as a
        # host would: the adapter translates drive offsets into the
        # (address, length) arguments of an existing machine client.
        base = 0x90000000000000  # > 2**53, as real drive addresses are
        mem = ad.MemStore(1 << 16)
        d = ad.format(mem, ad.Config(drive_length=1 << 16, capacity=8))
        d.set_account(rep_addr(0xBB), 7, 5)

        calls = []

        def read_memory(address, length):
            self.assertGreaterEqual(address, base)
            self.assertLessEqual(address + length, base + (1 << 16))
            calls.append((address, length))
            return mem.read_at(address - base, length)

        ms = ad.MachineStore(read_memory, base)
        host = ad.open(ms)
        a = host.get_account(rep_addr(0xBB))
        self.assertEqual((a.nonce, a.balance), (7, 5))
        self.assertIsNone(host.get_account(rep_addr(0x99)))
        self.assertTrue(calls, "adapter was never called")
        with self.assertRaises(ad.ReadOnlyStoreError):
            ms.write_at(0, b"\x00")


class TestGolden(unittest.TestCase):
    """Replay every scenario from ../testdata/golden.json: format a zeroed
    MemStore, apply every op asserting its expected kind, run every check,
    then hash the entire image."""

    @staticmethod
    def _kind_of(fn, *args, **kw):
        try:
            fn(*args, **kw)
            return "ok"
        except ad.AccountsDriveError as e:
            return e.kind

    def _replay(self, sc):
        c = sc["config"]
        cfg = ad.Config(
            drive_length=c.get("driveLength", 0),
            capacity=c.get("capacity", 0),
            load_limit=c.get("loadLimit", 0),
            seed=bytes.fromhex(c.get("seed", "00" * 32)),
            profile=c.get("profile", 0),
            slot_size=c.get("slotSize", 0),
            table_offset=c.get("tableOffset", 0),
            registry_offset=c.get("registryOffset", 0),
            registry_capacity=c.get("registryCapacity", 0),
            sparse_offset=c.get("sparseOffset", 0),
            sparse_capacity=c.get("sparseCapacity", 0),
            sparse_load_limit=c.get("sparseLoadLimit", 0),
            sparse_slot_size=c.get("sparseSlotSize", 0),
        )
        store = ad.MemStore(cfg.drive_length)
        d = ad.format(store, cfg)

        for i, op in enumerate(sc["ops"]):
            name = op["op"]
            if name == "setAccount":
                got = self._kind_of(
                    d.set_account, bytes.fromhex(op["addr"]),
                    op.get("nonce", 0), int(op.get("value", "0") or "0", 16))
            elif name == "deleteAccount":
                got = self._kind_of(
                    d.delete_account, bytes.fromhex(op["addr"]),
                    op.get("force", False))
            elif name == "registerToken":
                got = self._kind_of(
                    d.register_token, bytes.fromhex(op["token"]),
                    op.get("width", 0))
            elif name == "setTokenBalance":
                got = self._kind_of(
                    d.set_token_balance, bytes.fromhex(op["addr"]),
                    op.get("id", 0), int(op.get("value", "0") or "0", 16))
            else:
                self.fail(f"op {i}: unknown op {name!r}")
            self.assertEqual(got, op["expect"], f"op {i} ({name})")

        dcfg = d.config
        for i, check in enumerate(sc["checks"]):
            kind = check["check"]
            if kind == "account":
                acct = d.get_account(bytes.fromhex(check["addr"]))
                found = acct is not None
                self.assertEqual(found, check.get("found", False),
                                 f"check {i} (account {check['addr']}) found")
                if found:
                    self.assertEqual(acct.nonce, check.get("nonce", 0),
                                     f"check {i} nonce")
                    self.assertEqual(f"{acct.balance:064x}", check["value"],
                                     f"check {i} balance")
            elif kind == "tokenBalance":
                v = d.get_token_balance(bytes.fromhex(check["addr"]),
                                        check.get("id", 0))
                self.assertEqual(f"{v:064x}", check["value"],
                                 f"check {i} (token {check.get('id', 0)} "
                                 f"of {check['addr']})")
            elif kind == "liveCount":
                if check.get("table") == "sparse":
                    n = d.sparse_live_count()
                else:
                    n = d.live_count()
                self.assertEqual(n, check.get("count", 0),
                                 f"check {i} ({check.get('table')} liveCount)")
            elif kind == "slot":
                if check.get("table") == "sparse":
                    base, ss = dcfg.sparse_offset, dcfg.sparse_slot_size
                else:
                    base, ss = dcfg.table_offset, dcfg.slot_size
                raw = store.read_at(base + ss * check.get("index", 0), ss)
                self.assertEqual(raw.hex(), check["hex"],
                                 f"check {i} (slot {check.get('index', 0)})")
            else:
                self.fail(f"check {i}: unknown check {kind!r}")

        digest = hashlib.sha256(bytes(store.image)).hexdigest()
        self.assertEqual(digest, sc["sha256"], "drive image sha256")

    def test_golden(self):
        with open(GOLDEN_PATH) as f:
            golden = json.load(f)
        scenarios = golden["scenarios"]
        self.assertGreater(len(scenarios), 0, "golden file has no scenarios")
        for sc in scenarios:
            with self.subTest(scenario=sc["name"]):
                self._replay(sc)


if __name__ == "__main__":
    unittest.main(verbosity=2)
