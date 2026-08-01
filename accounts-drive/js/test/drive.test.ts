// Behavior tests ported from the Go reference (drive_test.go): record
// encodings, the spec Appendix B probe walkthrough, table-full and nonce
// protection, and the wide/sparse profiles.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  MemStore, format, open, AccountsDriveError,
  ProfileWide, ProfileSparse, bytesToHex, hexToBytes,
} from '../src/index.ts';

function repAddr(b: number): Uint8Array {
  return new Uint8Array(20).fill(b);
}

async function kindOf(promise: Promise<unknown>): Promise<string> {
  try {
    await promise;
    return 'ok';
  } catch (e) {
    if (e instanceof AccountsDriveError) return e.kind;
    throw e;
  }
}

test('account record encoding (spec Appendix B)', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, { driveLength: 1 << 16, capacity: 8 });
  await d.setAccount(repAddr(0xbb), 7n, 5n);
  // home(bb..) mod 8 = 1
  const got = s.bytes.subarray(4096 + 64 * 1, 4096 + 64 * 2);
  assert.equal(
    bytesToHex(got),
    'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000' +
    '0000000000000000000000000000000000000000000000000000000000000005',
  );
});

test('sparse 32-byte record encoding (spec Appendix B)', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, {
    driveLength: 1 << 16, capacity: 8, profile: ProfileSparse,
    registryOffset: 0x100, registryCapacity: 8,
    sparseOffset: 8192, sparseCapacity: 8, sparseSlotSize: 32,
  });
  for (let i = 0; i < 4; i++) {
    await d.registerToken(repAddr(i + 1), 8);
  }
  await d.setTokenBalance(repAddr(0xbb), 3, 1000000n);
  // sparse home(id 3, bb..) mod 8: LE64(e5cad187...) = 0xbfc3cb1987d1cae5 → 5
  const got = s.bytes.subarray(8192 + 32 * 5, 8192 + 32 * 6);
  assert.equal(
    bytesToHex(got),
    '03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240',
  );
});

test('probe walkthrough (spec Appendix B, C=8)', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, { driveLength: 1 << 16, capacity: 8, loadLimit: 7 });
  for (const b of [0xbb, 0xdd, 0xee, 0xaa]) {
    await d.setAccount(repAddr(b), 0n, 1n);
  }
  const slotAddr = (i: number): number => s.bytes[4096 + 64 * i];
  // bb home 1 → slot 1; dd home 1 → slot 2; ee home 1 → slot 3; aa home 5 → slot 5.
  for (const [slot, want] of [[1, 0xbb], [2, 0xdd], [3, 0xee], [5, 0xaa], [0, 0], [4, 0], [6, 0], [7, 0]]) {
    assert.equal(slotAddr(slot), want, `slot ${slot}`);
  }
  // Absent key with home 1 terminates via the empty slot 4.
  assert.equal(await d.getAccount(repAddr(0x99)), null);
  // Delete dd: ee backward-shifts into slot 2, slot 3 zeroes.
  assert.equal(await d.deleteAccount(repAddr(0xdd), false), true);
  assert.equal(slotAddr(2), 0xee);
  assert.equal(slotAddr(3), 0);
  for (const b of [0xbb, 0xee, 0xaa]) {
    assert.notEqual(await d.getAccount(repAddr(b)), null, `${b.toString(16)}.. lost after delete`);
  }
  assert.equal(await d.getAccount(repAddr(0xdd)), null);
  assert.equal(await d.liveCount(), 3n);
});

test('table full and nonce protection', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, { driveLength: 1 << 16, capacity: 8, loadLimit: 4 });
  for (const b of [0xaa, 0xbb, 0xcc, 0xdd]) {
    await d.setAccount(repAddr(b), 5n, 1n);
  }
  assert.equal(await kindOf(d.setAccount(repAddr(0xee), 0n, 1n)), 'tableFull');
  // Updating an existing account is not an insert and must still work.
  await d.setAccount(repAddr(0xaa), 6n, 2n);
  assert.equal(await kindOf(d.deleteAccount(repAddr(0xbb), false)), 'nonceProtected');
  assert.equal(await d.deleteAccount(repAddr(0xbb), true), true);
  await d.setAccount(repAddr(0xee), 0n, 1n);
});

test('addresses accepted as hex strings; zero address refused', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, { driveLength: 1 << 16, capacity: 8 });
  await d.setAccount('bb'.repeat(20), 7n, 5n);
  const a = await d.getAccount('0x' + 'bb'.repeat(20));
  assert.equal(a!.nonce, 7n);
  assert.equal(a!.balance, 5n);
  assert.equal(await kindOf(d.getAccount(new Uint8Array(20))), 'zeroAddress');
  assert.equal(await kindOf(d.setAccount('00'.repeat(20), 0n, 1n)), 'zeroAddress');
});

test('wide profile: columns, overflow, auto-create, reopen', async () => {
  const s = new MemStore(1 << 18);
  const d = await format(s, {
    driveLength: 1 << 18, capacity: 64, profile: ProfileWide, slotSize: 128,
    registryOffset: 0x100, registryCapacity: 8,
  });
  await d.registerToken(repAddr(0x01), 32);
  const id2 = await d.registerToken(repAddr(0x02), 8);
  await d.registerToken(repAddr(0x03), 8);
  // 64 + 32 + 8 + 8 = 112; another 32 would need 144 > 128.
  assert.equal(await kindOf(d.registerToken(repAddr(0x04), 32)), 'tableFull');
  assert.equal(await kindOf(d.registerToken(repAddr(0x01), 8)), 'duplicateToken');
  // Auto-create an account by setting a token balance, then check the
  // native record write preserves the column.
  const user = repAddr(0x77);
  await d.setTokenBalance(user, id2, 12345n);
  await d.setAccount(user, 9n, 500n);
  assert.equal(await d.getTokenBalance(user, id2), 12345n);
  const a = await d.getAccount(user);
  assert.equal(a!.nonce, 9n);
  assert.equal(a!.balance, 500n);
  // Width 8 column overflows past 2^64-1.
  assert.equal(await kindOf(d.setTokenBalance(user, id2, 1n << 64n)), 'overflow');
  // Reopen: registry and balances survive.
  const d2 = await open(s);
  assert.equal(d2.tokens().length, 3);
  assert.equal(await d2.getTokenBalance(user, id2), 12345n);
});

test('sparse profile: u256 values, zero deletes', async () => {
  const s = new MemStore(1 << 18);
  const d = await format(s, {
    driveLength: 1 << 18, capacity: 64, profile: ProfileSparse,
    registryOffset: 0x100, registryCapacity: 8,
    sparseOffset: 1 << 17, sparseCapacity: 16, sparseLoadLimit: 8,
  });
  const id = await d.registerToken(repAddr(0x01), 32);
  const user = repAddr(0x77);
  const huge = 123456789012345678901234567890n;
  await d.setTokenBalance(user, id, huge);
  assert.equal(await d.getTokenBalance(user, id), huge);
  assert.equal(await d.sparseLiveCount(), 1n);
  // Zero deletes the record.
  await d.setTokenBalance(user, id, 0n);
  assert.equal(await d.sparseLiveCount(), 0n);
  assert.equal(await d.getTokenBalance(user, id), 0n);
  assert.equal(await kindOf(d.getTokenBalance(user, 9)), 'unknownToken');
});

test('compact record: encoding, bounds, registry width (spec §7)', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, {
    driveLength: 1 << 16, capacity: 8, slotSize: 32,
    registryOffset: 0x100, registryCapacity: 2,
  });
  await d.setAccount(repAddr(0xbb), 7n, 5n);
  // home(bb..) mod 8 = 1
  const got = s.bytes.subarray(4096 + 32 * 1, 4096 + 32 * 2);
  assert.equal(
    bytesToHex(got),
    'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb070000000000000000000005',
  );
  const a = await d.getAccount(repAddr(0xbb));
  assert.equal(a!.nonce, 7n);
  assert.equal(a!.balance, 5n);
  // Nonce past uint32 and balance past uint64 must be refused.
  assert.equal(await kindOf(d.setAccount(repAddr(0xbb), 1n << 32n, 5n)), 'overflow');
  assert.equal(await kindOf(d.setAccount(repAddr(0xbb), 7n, 1n << 64n)), 'overflow');
  // A registry entry, if declared, must carry width 8.
  assert.equal(await kindOf(d.registerToken(repAddr(0x01), 32)), 'badWidth');
  assert.equal(await kindOf(d.registerToken(new Uint8Array(20), 8)), 'ok');
  // Nonce protection reads the uint32.
  assert.equal(await kindOf(d.deleteAccount(repAddr(0xbb), false)), 'nonceProtected');
});

test('32-byte account slots only format in profile 0', async () => {
  await assert.rejects(format(new MemStore(1 << 16), {
    driveLength: 1 << 16, capacity: 8, slotSize: 32, profile: ProfileSparse,
    registryOffset: 0x100, registryCapacity: 2,
    sparseOffset: 8192, sparseCapacity: 8,
  }), /slot size/);
});

test('sparse 32-byte slots require width 8', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, {
    driveLength: 1 << 16, capacity: 8, profile: ProfileSparse,
    registryOffset: 0x100, registryCapacity: 8,
    sparseOffset: 8192, sparseCapacity: 8, sparseSlotSize: 32,
  });
  assert.equal(await kindOf(d.registerToken(repAddr(0x01), 16)), 'badWidth');
});

test('profile 0 has no token operations without a registry', async () => {
  const s = new MemStore(1 << 16);
  const d = await format(s, { driveLength: 1 << 16, capacity: 8 });
  assert.equal(await kindOf(d.registerToken(repAddr(0x01), 32)), 'profile');
  assert.equal(await kindOf(d.getTokenBalance(repAddr(0xbb), 0)), 'unknownToken');
});

test('open validates magic and version', async () => {
  await assert.rejects(open(new MemStore(1 << 16)), /bad magic/);
  const s = new MemStore(1 << 16);
  await format(s, { driveLength: 1 << 16, capacity: 8 });
  s.bytes[0x08] = 2; // version
  await assert.rejects(open(s), /version/);
  s.bytes[0x08] = 1;
  s.bytes[0x0c] = 1; // flags
  await assert.rejects(open(s), /flags/);
});

test('hex helpers round-trip', () => {
  const b = hexToBytes('0xdeadBEEF');
  assert.equal(bytesToHex(b), 'deadbeef');
  assert.throws(() => hexToBytes('xyz'));
});
