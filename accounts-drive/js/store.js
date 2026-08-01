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

/** MachineStore reads the drive out of a Cartesi Machine through whatever
 * machine client the host already uses — this library deliberately does not
 * speak the machine's JSON-RPC itself. The store only translates
 * drive-relative offsets into the (address, length) arguments of the
 * client's machine.read_memory. Read-only; per spec §11 use it only against
 * a quiescent (parked or stored) machine. */
export class MachineStore {
  /**
   * @param {(address: bigint, length: number) => Promise<Uint8Array>} readMemory
   *   the machine client's read_memory: bytes at an absolute machine address
   * @param {number|bigint} baseAddress the drive's start address in the machine's address space
   */
  constructor(readMemory, baseAddress) {
    this.readMemory = readMemory;
    this.base = BigInt(baseAddress);
  }

  async readAt(off, len) {
    const data = await this.readMemory(this.base + BigInt(off), Number(len));
    if (data.length !== Number(len)) {
      throw new Error(`read_memory returned ${data.length} bytes, want ${len}`);
    }
    return data;
  }

  async writeAt() {
    throw new Error('store is read-only');
  }
}
