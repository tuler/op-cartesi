// Store implementations: FileStore over a real image file, and the
// MachineStore adapter over a fake machine client's read_memory.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { MemStore, FileStore, MachineStore, format, open } from '../src/index.ts';

function repAddr(b: number): Uint8Array {
  return new Uint8Array(20).fill(b);
}

test('FileStore: format, write, sync, reopen byte-identical', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'accounts-drive-'));
  const path = join(dir, 'drive.img');
  try {
    await writeFile(path, new Uint8Array(1 << 16));
    const fs1 = await FileStore.open(path);
    const d = await format(fs1, { driveLength: 1 << 16, capacity: 8 });
    await d.setAccount(repAddr(0xbb), 7n, 5n);
    await fs1.sync(); // spec §10: a guest MUST sync before finishing each input
    await fs1.close();

    // The file image equals what a MemStore run produces.
    const ms = new MemStore(1 << 16);
    const dm = await format(ms, { driveLength: 1 << 16, capacity: 8 });
    await dm.setAccount(repAddr(0xbb), 7n, 5n);
    const fileBytes = new Uint8Array(await readFile(path));
    assert.deepEqual(fileBytes, ms.bytes);

    // And reopens as a valid drive.
    const fs2 = await FileStore.open(path);
    const d2 = await open(fs2);
    const a = await d2.getAccount(repAddr(0xbb));
    assert.equal(a!.nonce, 7n);
    assert.equal(a!.balance, 5n);
    await fs2.close();
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test('MachineStore: translates offsets into read_memory arguments, refuses writes', async () => {
  // A quiescent "machine": one MemStore image behind a fake machine client.
  const image = new MemStore(1 << 16);
  const written = await format(image, { driveLength: 1 << 16, capacity: 8 });
  await written.setAccount(repAddr(0xbb), 7n, 5n);
  const base = 0x90000000000000n; // drive start in machine address space (> 2^53)

  let calls = 0;
  const readMemory = async (address: bigint, length: number): Promise<Uint8Array> => {
    assert.equal(typeof address, 'bigint');
    assert.ok(address >= base && address + BigInt(length) <= base + BigInt(1 << 16),
      `read outside the drive: ${address} + ${length}`);
    calls++;
    const off = Number(address - base);
    return image.bytes.subarray(off, off + length);
  };

  const store = new MachineStore(readMemory, base);
  const d = await open(store);
  const a = await d.getAccount(repAddr(0xbb));
  assert.equal(a!.nonce, 7n);
  assert.equal(a!.balance, 5n);
  assert.equal(await d.getAccount(repAddr(0x99)), null);
  assert.equal(await d.liveCount(), 1n);
  assert.ok(calls > 0, 'adapter was never called');
  await assert.rejects(store.writeAt(0, new Uint8Array(1)), /read-only/);
});

test('MemStore: bounds are enforced', async () => {
  const s = new MemStore(64);
  await assert.rejects(s.readAt(60, 8), /outside drive/);
  await assert.rejects(s.writeAt(63, new Uint8Array(2)), /outside drive/);
  await s.writeAt(62, new Uint8Array([1, 2]));
  assert.deepEqual(await s.readAt(62, 2), new Uint8Array([1, 2]));
});
