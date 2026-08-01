// Store implementations: FileStore over a real image file, and
// ReadMemoryStore over a local JSON-RPC server speaking machine.read_memory.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:http';
import { Buffer } from 'node:buffer';
import { MemStore, FileStore, ReadMemoryStore, format, open } from '../index.js';

function repAddr(b) {
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
    assert.equal(a.nonce, 7n);
    assert.equal(a.balance, 5n);
    await fs2.close();
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test('ReadMemoryStore: reads via machine.read_memory, refuses writes', async (t) => {
  // A quiescent "machine": one MemStore image served over JSON-RPC.
  const image = new MemStore(1 << 16);
  const written = await format(image, { driveLength: 1 << 16, capacity: 8 });
  await written.setAccount(repAddr(0xbb), 7n, 5n);
  const base = 0x90000000000000n; // drive start in machine address space (> 2^53)

  const server = createServer((req, res) => {
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      // Addresses can exceed 2^53; parse the request without JSON.parse's
      // number precision loss.
      const address = BigInt(body.match(/"address":\s*(\d+)/)[1]);
      const length = Number(body.match(/"length":\s*(\d+)/)[1]);
      const id = JSON.parse(body).id;
      const off = Number(address - base);
      const data = Buffer.from(image.bytes.subarray(off, off + length)).toString('base64');
      res.setHeader('content-type', 'application/json');
      res.end(JSON.stringify({ jsonrpc: '2.0', id, result: data }));
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());

  const endpoint = `http://127.0.0.1:${server.address().port}/`;
  const store = new ReadMemoryStore(endpoint, base);
  const d = await open(store);
  const a = await d.getAccount(repAddr(0xbb));
  assert.equal(a.nonce, 7n);
  assert.equal(a.balance, 5n);
  assert.equal(await d.getAccount(repAddr(0x99)), null);
  assert.equal(await d.liveCount(), 1n);
  await assert.rejects(store.writeAt(0, new Uint8Array(1)), /read-only/);
});

test('MemStore: bounds are enforced', async () => {
  const s = new MemStore(64);
  await assert.rejects(s.readAt(60, 8), /outside drive/);
  await assert.rejects(s.writeAt(63, new Uint8Array(2)), /outside drive/);
  await s.writeAt(62, new Uint8Array([1, 2]));
  assert.deepEqual(await s.readAt(62, 2), new Uint8Array([1, 2]));
});
