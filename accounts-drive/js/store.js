// Stores: the byte-level access the library needs. Offsets are relative to
// the drive's first byte. The interface is asynchronous because the host
// store speaks HTTP:
//
//   { async readAt(off, len) -> Uint8Array, async writeAt(off, bytes) }
//
// Implementations decide what backs it: a byte array, a device file inside
// the guest, or a remote machine's memory on the host.

import { open as fsOpen } from 'node:fs/promises';
import { Buffer } from 'node:buffer';

/** MemStore is an in-memory drive image: the whole drive as one Uint8Array,
 * exposed as `.bytes` so callers can hash or snapshot the image directly. */
export class MemStore {
  /** @param {number|Uint8Array} lengthOrBytes zeroed length to allocate, or an existing image */
  constructor(lengthOrBytes) {
    this.bytes = lengthOrBytes instanceof Uint8Array
      ? lengthOrBytes
      : new Uint8Array(Number(lengthOrBytes));
  }

  #bounds(off, n) {
    if (!Number.isSafeInteger(off) || off < 0 || !Number.isSafeInteger(n) || n < 0 || off + n > this.bytes.length) {
      throw new Error(`access [${off},${off + n}) outside drive of ${this.bytes.length} bytes`);
    }
  }

  async readAt(off, len) {
    off = Number(off);
    len = Number(len);
    this.#bounds(off, len);
    return this.bytes.slice(off, off + len);
  }

  async writeAt(off, bytes) {
    off = Number(off);
    this.#bounds(off, bytes.length);
    this.bytes.set(bytes, off);
  }
}

/** FileStore accesses the drive through a file — in the guest, the raw device
 * (/dev/pmemN). Guests on a pmem-backed drive MUST call sync() before
 * finishing each input (spec §10): file I/O goes through the guest kernel's
 * page cache, and un-synced bytes are not in machine state at the yield. */
export class FileStore {
  /** @param {import('node:fs/promises').FileHandle} handle */
  constructor(handle) {
    this.handle = handle;
  }

  /** Opens a device or image file read-write. */
  static async open(path) {
    return new FileStore(await fsOpen(path, 'r+'));
  }

  async readAt(off, len) {
    off = Number(off);
    len = Number(len);
    const buf = new Uint8Array(len);
    let got = 0;
    while (got < len) {
      const { bytesRead } = await this.handle.read(buf, got, len - got, off + got);
      if (bytesRead === 0) throw new Error(`short read at ${off}: got ${got} of ${len} bytes`);
      got += bytesRead;
    }
    return buf;
  }

  async writeAt(off, bytes) {
    off = Number(off);
    let put = 0;
    while (put < bytes.length) {
      const { bytesWritten } = await this.handle.write(bytes, put, bytes.length - put, off + put);
      if (bytesWritten === 0) throw new Error(`short write at ${off}`);
      put += bytesWritten;
    }
  }

  /** Flushes written bytes to the device (fdatasync). Per spec §10 a guest
   * MUST call this before finishing each input on a pmem-backed drive. */
  async sync() {
    await this.handle.datasync();
  }

  async close() {
    await this.handle.close();
  }
}

/** ReadMemoryStore reads the drive out of a Cartesi Machine through the
 * cartesi-jsonrpc-machine protocol's machine.read_memory — a pure state read
 * that never executes the machine. It is read-only, and per spec §11 it must
 * only be used against a quiescent (parked or stored) machine. */
export class ReadMemoryStore {
  /**
   * @param {string} endpoint the machine server's JSON-RPC URL
   * @param {number|bigint} baseAddress the drive's start address in the machine's address space
   */
  constructor(endpoint, baseAddress) {
    this.endpoint = endpoint;
    this.base = BigInt(baseAddress);
    this.reqId = 0;
  }

  async readAt(off, len) {
    const address = this.base + BigInt(off);
    const id = ++this.reqId;
    // Built by hand so 64-bit addresses survive as exact JSON integers.
    const body = `{"jsonrpc":"2.0","id":${id},"method":"machine.read_memory",` +
      `"params":{"address":${address.toString()},"length":${Number(len)}}}`;
    const resp = await fetch(this.endpoint, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body,
    });
    if (!resp.ok) {
      throw new Error(`machine.read_memory: HTTP ${resp.status}`);
    }
    const rr = await resp.json();
    if (rr.error) {
      throw new Error(`machine.read_memory: ${rr.error.message} (code ${rr.error.code})`);
    }
    if (typeof rr.result !== 'string') {
      throw new Error('machine.read_memory: bad response');
    }
    const data = new Uint8Array(Buffer.from(rr.result, 'base64'));
    if (data.length !== Number(len)) {
      throw new Error(`machine.read_memory: got ${data.length} bytes, want ${len}`);
    }
    return data;
  }

  async writeAt() {
    throw new Error('store is read-only');
  }
}
