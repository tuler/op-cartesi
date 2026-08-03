// Cartesi Accounts Drive Format, version 1 (docs/ACCOUNTS-DRIVE-SPEC.md):
// a fixed-layout accounts database inside a Cartesi Machine flash drive,
// written by the guest as part of the state transition and readable — and
// provable — from outside without executing the machine.
//
// The same code serves both sides of the drive. A guest opens it over a
// FileStore on the raw device; a host opens it over a ReadMemoryStore against
// a parked machine, or a MemStore holding a copied image. All layout, hashing
// and probing logic is shared, which is what keeps two independent parties
// byte-for-byte agreed about what the drive means.
//
// All Drive methods are async (the store interface is async because the host
// store speaks HTTP). Balances are BigInt, encoded big-endian at the declared
// width; nonces are BigInt (uint64 little-endian on the drive, uint32 in the
// compact profile-0 record); addresses are 20-byte Uint8Arrays (40-char hex
// strings are accepted everywhere).

import { keccak256 } from './keccak.ts';
import type { Store } from './store.ts';

// Profiles (spec §5).
export const ProfileSingleAsset = 0;
export const ProfileWide = 1;
export const ProfileSparse = 2;

/** Magic is the header's first 8 bytes, ASCII "ctsiacct". */
export const Magic = new Uint8Array([0x63, 0x74, 0x73, 0x69, 0x61, 0x63, 0x63, 0x74]);

/** Version is the format version this library implements. */
export const Version = 1;

const HEADER_SIZE = 4096;

// Header field offsets (spec §5).
const OFF_MAGIC = 0x00;
const OFF_VERSION = 0x08;
const OFF_FLAGS = 0x0c;
const OFF_CAPACITY = 0x10;
const OFF_TABLE_OFFSET = 0x18;
const OFF_LOAD_LIMIT = 0x20;
const OFF_LIVE_COUNT = 0x28;
const OFF_SEED = 0x30;
const OFF_PROFILE = 0x50;
const OFF_SLOT_SIZE = 0x54;
const OFF_REGISTRY_OFFSET = 0x58;
const OFF_REGISTRY_CAP = 0x60;
const OFF_TOKEN_COUNT = 0x68;
const OFF_SPARSE_OFFSET = 0x70;
const OFF_SPARSE_CAPACITY = 0x78;
const OFF_SPARSE_LOAD_LIMIT = 0x80;
const OFF_SPARSE_LIVE_COUNT = 0x88;
const OFF_SPARSE_SLOT_SIZE = 0x90;
const MIN_REGISTRY_OFFSET = 0x100;

/** An address: 20 raw bytes, or 40 hex chars (optionally 0x-prefixed). */
export type AddressLike = Uint8Array | string;

/** A registered token's balance width in bytes (spec §8). */
export type TokenWidth = 8 | 16 | 32;

/** A registry entry, in id order. */
export interface Token {
  address: Uint8Array;
  width: TokenWidth;
}

/** A token resolved by address: its registry id plus the entry. */
export interface ResolvedToken extends Token {
  id: number;
}

/** An account record: BigInt nonce and native balance. */
export interface Account {
  address: Uint8Array;
  nonce: bigint;
  balance: bigint;
}

/**
 * Config describes a drive's geometry for `format`. Zero (or absent) fields
 * take the same defaults as the Go reference — see `normalizeConfig`.
 */
export interface Config {
  driveLength?: number;
  capacity?: number;
  loadLimit?: number;
  /** 32 bytes or 64 hex chars; absent means zero. */
  seed?: Uint8Array | string;
  profile?: number;
  slotSize?: number;
  tableOffset?: number;
  registryOffset?: number;
  registryCapacity?: number;
  sparseOffset?: number;
  sparseCapacity?: number;
  sparseLoadLimit?: number;
  sparseSlotSize?: number;
}

/** NormalizedConfig is a Config with every default applied (deployment-
 * constant geometry, as cached by an open Drive). */
export interface NormalizedConfig {
  driveLength: number;
  capacity: number;
  loadLimit: number;
  seed: Uint8Array;
  profile: number;
  slotSize: number;
  tableOffset: number;
  registryOffset: number;
  registryCapacity: number;
  sparseOffset: number;
  sparseCapacity: number;
  sparseLoadLimit: number;
  sparseSlotSize: number;
}

/**
 * ErrorKind is the golden taxonomy: every condition a caller is expected to
 * branch on. In a guest, tableFull, registryFull and overflow are exactly
 * the conditions the spec requires to be answered by rejecting the input.
 */
export type ErrorKind =
  | 'tableFull'
  | 'registryFull'
  | 'overflow'
  | 'nonceProtected'
  | 'duplicateToken'
  | 'unknownToken'
  | 'badWidth'
  | 'zeroAddress'
  | 'profile';

/**
 * AccountsDriveError is the error type for every condition a caller is
 * expected to branch on. `kind` is one of the golden taxonomy kinds:
 * "tableFull", "registryFull", "overflow", "nonceProtected",
 * "duplicateToken", "unknownToken", "badWidth", "zeroAddress", "profile".
 * In a guest, tableFull, registryFull and overflow are exactly the
 * conditions the spec requires to be answered by rejecting the input.
 */
export class AccountsDriveError extends Error {
  kind: ErrorKind;

  constructor(kind: ErrorKind, message?: string) {
    super(message ?? kind);
    this.name = 'AccountsDriveError';
    this.kind = kind;
  }
}

const err = {
  tableFull: (msg?: string) => new AccountsDriveError('tableFull', msg ?? 'table at load limit'),
  registryFull: () => new AccountsDriveError('registryFull', 'token registry full'),
  overflow: () => new AccountsDriveError('overflow', 'balance exceeds declared width'),
  zeroAddress: () => new AccountsDriveError('zeroAddress', 'zero address is invalid'),
  nonceProtected: () => new AccountsDriveError('nonceProtected',
    'record has nonzero nonce (replay protection); pass force to delete'),
  profile: () => new AccountsDriveError('profile', 'operation not available in this profile'),
  unknownToken: () => new AccountsDriveError('unknownToken', 'token not registered'),
  duplicateToken: () => new AccountsDriveError('duplicateToken', 'token already registered'),
  badWidth: (msg?: string) => new AccountsDriveError('badWidth', msg ?? 'balance width must be 8, 16 or 32'),
};

// ---------------------------------------------------------------- helpers

/** Decodes a hex string (optionally 0x-prefixed) into a Uint8Array. */
export function hexToBytes(hex: string): Uint8Array {
  if (hex.startsWith('0x') || hex.startsWith('0X')) hex = hex.slice(2);
  if (hex.length % 2 !== 0 || /[^0-9a-fA-F]/.test(hex)) {
    throw new Error(`invalid hex string ${JSON.stringify(hex)}`);
  }
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.slice(2 * i, 2 * i + 2), 16);
  }
  return out;
}

/** Encodes bytes as a lowercase hex string (no 0x prefix). */
export function bytesToHex(bytes: Uint8Array): string {
  let s = '';
  for (const b of bytes) s += b.toString(16).padStart(2, '0');
  return s;
}

function toAddress(a: AddressLike): Uint8Array {
  const b = typeof a === 'string' ? hexToBytes(a) : a;
  if (!(b instanceof Uint8Array) || b.length !== 20) {
    throw new Error('address must be 20 bytes (or 40 hex chars)');
  }
  return b;
}

function isZero(b: Uint8Array): boolean {
  for (const x of b) if (x !== 0) return false;
  return true;
}

function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}

function isPow2(n: number): boolean {
  const b = BigInt(n);
  return b > 0n && (b & (b - 1n)) === 0n;
}

/** Reads bytes as an unsigned big-endian BigInt. */
function readBE(bytes: Uint8Array): bigint {
  let v = 0n;
  for (const b of bytes) v = (v << 8n) | BigInt(b);
  return v;
}

/** Writes a non-negative BigInt big-endian into buf (zero-padded on the
 * left, like Go's big.Int.FillBytes), or throws overflow. */
function putBE(buf: Uint8Array, v: bigint): void {
  if (v < 0n || v >> BigInt(8 * buf.length) !== 0n) throw err.overflow();
  for (let i = buf.length - 1; i >= 0; i--) {
    buf[i] = Number(v & 0xffn);
    v >>= 8n;
  }
}

function getLE64(bytes: Uint8Array, off: number): bigint {
  return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getBigUint64(off, true);
}

function getLE32(bytes: Uint8Array, off: number): number {
  return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getUint32(off, true);
}

/** home implements spec §6.1: LE64(keccak256(seed ‖ key)[0..8]) mod capacity. */
export function home(seed: Uint8Array, key: Uint8Array, capacity: number): number {
  const h = keccak256(seed, key);
  return Number(getLE64(h, 0) % BigInt(capacity));
}

/** sparseKey builds the sparse table's 22-byte key (spec §6.1):
 * 2-byte little-endian token id ‖ 20-byte address. */
export function sparseKey(id: number, addr: Uint8Array): Uint8Array {
  const k = new Uint8Array(22);
  k[0] = id & 0xff;
  k[1] = (id >> 8) & 0xff;
  k.set(addr, 2);
  return k;
}

// ---------------------------------------------------------------- config

/**
 * normalizeConfig applies the same defaults as the Go reference:
 * slotSize 0 → 64, tableOffset 0 → 4096, loadLimit 0 → ⅞ of capacity, and
 * for the sparse profile sparseSlotSize 0 → 64, sparseLoadLimit 0 → ⅞.
 * `seed` may be a 32-byte Uint8Array or 64 hex chars; absent means zero.
 */
function normalizeConfig(config: Config): NormalizedConfig {
  const c: NormalizedConfig = {
    driveLength: Number(config.driveLength ?? 0),
    capacity: Number(config.capacity ?? 0),
    loadLimit: Number(config.loadLimit ?? 0),
    seed: new Uint8Array(32),
    profile: Number(config.profile ?? 0),
    slotSize: Number(config.slotSize ?? 0),
    tableOffset: Number(config.tableOffset ?? 0),
    registryOffset: Number(config.registryOffset ?? 0),
    registryCapacity: Number(config.registryCapacity ?? 0),
    sparseOffset: Number(config.sparseOffset ?? 0),
    sparseCapacity: Number(config.sparseCapacity ?? 0),
    sparseLoadLimit: Number(config.sparseLoadLimit ?? 0),
    sparseSlotSize: Number(config.sparseSlotSize ?? 0),
  };
  if (config.seed != null) {
    const s = typeof config.seed === 'string' ? hexToBytes(config.seed) : config.seed;
    if (s.length !== 32) throw new Error('seed must be 32 bytes');
    c.seed.set(s);
  }
  if (c.slotSize === 0) c.slotSize = 64;
  if (c.tableOffset === 0) c.tableOffset = HEADER_SIZE;
  if (c.loadLimit === 0 && c.capacity > 0) c.loadLimit = c.capacity - Math.floor(c.capacity / 8);
  if (c.profile === ProfileSparse) {
    if (c.sparseSlotSize === 0) c.sparseSlotSize = 64;
    if (c.sparseLoadLimit === 0 && c.sparseCapacity > 0) {
      c.sparseLoadLimit = c.sparseCapacity - Math.floor(c.sparseCapacity / 8);
    }
  }
  return c;
}

function validateConfig(c: NormalizedConfig): void {
  if (c.driveLength === 0 || !isPow2(c.driveLength)) {
    throw new Error(`drive length ${c.driveLength} is not a power of two`);
  }
  if (c.profile > ProfileSparse || c.profile < 0) {
    throw new Error(`unknown profile ${c.profile}`);
  }
  const ss = c.slotSize;
  if (c.profile === ProfileWide) {
    if (ss < 128 || !isPow2(ss)) {
      throw new Error(`wide slot size ${ss}: want a power of two >= 128`);
    }
  } else if (c.profile === ProfileSingleAsset) {
    if (ss !== 64 && ss !== 32) {
      throw new Error(`slot size ${ss}: profile 0 takes 64 (standard record) or 32 (compact record)`);
    }
  } else if (ss !== 64) {
    throw new Error(`slot size ${ss}: must be 64 in the sparse profile`);
  }
  if (c.capacity < 2) throw new Error(`capacity ${c.capacity} < 2`);
  if (c.loadLimit < 1 || c.loadLimit >= c.capacity) {
    throw new Error(`load limit ${c.loadLimit} outside [1, capacity-1]`);
  }
  if (c.tableOffset < HEADER_SIZE || c.tableOffset % ss !== 0) {
    throw new Error(`table offset ${c.tableOffset}: want >= ${HEADER_SIZE} and a multiple of the slot size`);
  }
  interface Region { name: string; off: number; len: number }
  const regions: Region[] = [
    { name: 'header', off: 0, len: HEADER_SIZE },
    { name: 'accounts table', off: c.tableOffset, len: c.capacity * ss },
  ];
  if (c.profile !== ProfileSingleAsset && c.registryOffset === 0) {
    throw new Error(`profile ${c.profile} requires a token registry`);
  }
  if (c.registryOffset !== 0) {
    if (c.registryCapacity === 0 || c.registryCapacity > 65536) {
      throw new Error(`registry capacity ${c.registryCapacity} outside [1, 65536]`);
    }
    if (c.registryOffset % 32 !== 0) {
      throw new Error(`registry offset ${c.registryOffset} not a multiple of 32`);
    }
    const end = c.registryOffset + 32 * c.registryCapacity;
    if (c.registryOffset < HEADER_SIZE) { // in-header placement (spec §5)
      if (c.registryOffset < MIN_REGISTRY_OFFSET || end > HEADER_SIZE) {
        throw new Error(`in-header registry [${c.registryOffset},${end}) outside [${MIN_REGISTRY_OFFSET},${HEADER_SIZE})`);
      }
    } else {
      regions.push({ name: 'registry', off: c.registryOffset, len: 32 * c.registryCapacity });
    }
  }
  if (c.profile === ProfileSparse) {
    const sss = c.sparseSlotSize;
    if (sss !== 64 && sss !== 32) {
      throw new Error(`sparse slot size ${sss}: want 64 or 32`);
    }
    if (c.sparseCapacity < 2) throw new Error(`sparse capacity ${c.sparseCapacity} < 2`);
    if (c.sparseLoadLimit < 1 || c.sparseLoadLimit >= c.sparseCapacity) {
      throw new Error(`sparse load limit ${c.sparseLoadLimit} outside [1, capacity-1]`);
    }
    if (c.sparseOffset < HEADER_SIZE || c.sparseOffset % sss !== 0) {
      throw new Error(`sparse offset ${c.sparseOffset}: want >= ${HEADER_SIZE} and a multiple of the slot size`);
    }
    regions.push({ name: 'sparse table', off: c.sparseOffset, len: c.sparseCapacity * sss });
  }
  for (const r of regions) {
    if (r.off + r.len > c.driveLength) {
      throw new Error(`${r.name} [${r.off},${r.off + r.len}) outside drive of ${c.driveLength} bytes`);
    }
  }
  for (let i = 0; i < regions.length; i++) {
    for (let j = i + 1; j < regions.length; j++) {
      const a = regions[i];
      const b = regions[j];
      if (a.off < b.off + b.len && b.off < a.off + a.len) {
        throw new Error(`${a.name} overlaps ${b.name}`);
      }
    }
  }
}

// ---------------------------------------------------------------- drive

// A table descriptor makes the probing code (spec §6) profile-agnostic:
// keyOf() extracts the record's key; a slot is live iff the address part
// (20 bytes at liveOff) is nonzero.
interface Table {
  base: number;
  capacity: number;
  slotSize: number;
  countOff: number;
  limit: number;
  keyOf: (slot: Uint8Array) => Uint8Array;
  liveOff: number;
}

/**
 * Drive is an open accounts drive. Geometry is cached at open (it is
 * immutable by spec); the registry is cached too and refreshed on writes
 * through this instance — a host watching a drive someone else appends to
 * should call reloadRegistry() when the token count moves.
 *
 * Construct through `format` or `open`, not `new`.
 */
export class Drive {
  store: Store;
  cfg: NormalizedConfig;
  tokens_: Token[];
  colOff: number[]; // wide profile: column byte offset per token id

  constructor(store: Store, cfg: NormalizedConfig) {
    this.store = store;
    this.cfg = cfg;
    this.tokens_ = [];
    this.colOff = [];
  }

  /** Returns the drive's geometry (normalized, deployment-constant). */
  config(): NormalizedConfig {
    return { ...this.cfg, seed: this.cfg.seed.slice() };
  }

  /** Returns the cached registry, in id order: [{address, width}, ...]. */
  tokens(): Token[] {
    return this.tokens_.map((t) => ({ address: t.address.slice(), width: t.width }));
  }

  /** Re-reads the registry (and the wide column offsets). */
  async reloadRegistry(): Promise<void> {
    this.tokens_ = [];
    this.colOff = [];
    if (this.cfg.registryOffset === 0) return;
    const n = Number(await this.#counter(OFF_TOKEN_COUNT));
    if (n > this.cfg.registryCapacity) {
      throw new Error(`token count ${n} exceeds registry capacity ${this.cfg.registryCapacity}`);
    }
    let col = 64;
    for (let i = 0; i < n; i++) {
      const e = await this.store.readAt(this.cfg.registryOffset + 32 * i, 32);
      const width = e[20];
      if (width !== 8 && width !== 16 && width !== 32) {
        throw err.badWidth(`registry entry ${i}: balance width must be 8, 16 or 32 (${width})`);
      }
      this.tokens_.push({ address: e.slice(0, 20), width });
      this.colOff.push(col);
      col += width;
    }
  }

  // -------------------------------------------------------------- counters

  async #counter(off: number): Promise<bigint> {
    const b = await this.store.readAt(off, 8);
    return getLE64(b, 0);
  }

  async #setCounter(off: number, v: bigint): Promise<void> {
    const b = new Uint8Array(8);
    new DataView(b.buffer).setBigUint64(0, BigInt(v), true);
    await this.store.writeAt(off, b);
  }

  /** The accounts table's occupied-slot count (BigInt). */
  async liveCount(): Promise<bigint> {
    return this.#counter(OFF_LIVE_COUNT);
  }

  /** The sparse table's occupied-slot count (BigInt). */
  async sparseLiveCount(): Promise<bigint> {
    return this.#counter(OFF_SPARSE_LIVE_COUNT);
  }

  /** The number of registered tokens (BigInt). */
  async tokenCount(): Promise<bigint> {
    return this.#counter(OFF_TOKEN_COUNT);
  }

  // -------------------------------------------------------------- tables

  #accountsTable(): Table {
    return {
      base: this.cfg.tableOffset,
      capacity: this.cfg.capacity,
      slotSize: this.cfg.slotSize,
      countOff: OFF_LIVE_COUNT,
      limit: this.cfg.loadLimit,
      keyOf: (s) => s.subarray(0, 20),
      liveOff: 0,
    };
  }

  #sparseTable(): Table {
    return {
      base: this.cfg.sparseOffset,
      capacity: this.cfg.sparseCapacity,
      slotSize: this.cfg.sparseSlotSize,
      countOff: OFF_SPARSE_LIVE_COUNT,
      limit: this.cfg.sparseLoadLimit,
      keyOf: (s) => {
        const k = new Uint8Array(22);
        k.set(s.subarray(0, 2), 0);
        k.set(s.subarray(4, 24), 2);
        return k;
      },
      liveOff: 4,
    };
  }

  #home(key: Uint8Array, capacity: number): number {
    return home(this.cfg.seed, key, capacity);
  }

  async #readSlot(t: Table, i: number): Promise<Uint8Array> {
    return this.store.readAt(t.base + t.slotSize * i, t.slotSize);
  }

  async #writeSlot(t: Table, i: number, s: Uint8Array): Promise<void> {
    await this.store.writeAt(t.base + t.slotSize * i, s);
  }

  #live(t: Table, slot: Uint8Array): boolean {
    return !isZero(slot.subarray(t.liveOff, t.liveOff + 20));
  }

  #displacement(t: Table, slot: Uint8Array, at: number): number {
    return (at + t.capacity - this.#home(t.keyOf(slot), t.capacity)) % t.capacity;
  }

  // lookup implements spec §6.2, returning {index, slot} when found, null
  // when absent.
  async #lookup(t: Table, key: Uint8Array): Promise<{ index: number; slot: Uint8Array } | null> {
    const h = this.#home(key, t.capacity);
    for (let dist = 0; dist < t.capacity; dist++) {
      const i = (h + dist) % t.capacity;
      const slot = await this.#readSlot(t, i);
      if (!this.#live(t, slot)) return null;
      if (bytesEqual(t.keyOf(slot), key)) return { index: i, slot };
      if (this.#displacement(t, slot, i) < dist) return null;
    }
    return null; // unreachable while load limit < capacity
  }

  // insert implements spec §6.3. The caller has checked the key is absent
  // and the load limit has room; insert bumps the live counter itself.
  async #insert(t: Table, rec: Uint8Array): Promise<void> {
    let carry = rec;
    let h = this.#home(t.keyOf(carry), t.capacity);
    let dist = 0;
    for (;;) {
      const i = (h + dist) % t.capacity;
      const slot = await this.#readSlot(t, i);
      if (!this.#live(t, slot)) {
        await this.#writeSlot(t, i, carry);
        const n = await this.#counter(t.countOff);
        await this.#setCounter(t.countOff, n + 1n);
        return;
      }
      const disp = this.#displacement(t, slot, i);
      if (disp < dist) {
        await this.#writeSlot(t, i, carry);
        carry = slot;
        h = this.#home(t.keyOf(carry), t.capacity);
        dist = disp;
      }
      dist++;
    }
  }

  // remove implements spec §6.4 (backward shift) from a known slot index.
  async #remove(t: Table, i: number): Promise<void> {
    for (;;) {
      const j = (i + 1) % t.capacity;
      const next = await this.#readSlot(t, j);
      if (!this.#live(t, next) || this.#displacement(t, next, j) === 0) {
        await this.#writeSlot(t, i, new Uint8Array(t.slotSize));
        const n = await this.#counter(t.countOff);
        await this.#setCounter(t.countOff, n - 1n);
        return;
      }
      await this.#writeSlot(t, i, next);
      i = j;
    }
  }

  // -------------------------------------------------------------- accounts

  // compact reports whether the drive uses the 32-byte profile-0 record
  // (spec §7): address at 0..20, uint32 little-endian nonce at 20..24,
  // uint64 big-endian balance at 24..32.
  #compact(): boolean {
    return this.cfg.profile === ProfileSingleAsset && this.cfg.slotSize === 32;
  }

  // decodeAccount reads nonce and balance out of a live account slot.
  #decodeAccount(slot: Uint8Array): { nonce: bigint; balance: bigint } {
    if (this.#compact()) {
      return { nonce: BigInt(getLE32(slot, 20)), balance: readBE(slot.subarray(24, 32)) };
    }
    return { nonce: getLE64(slot, 24), balance: readBE(slot.subarray(32, 64)) };
  }

  // encodeAccount builds a record. In the compact record a nonce past
  // uint32 or a balance past uint64 is kind "overflow" — the input must be
  // rejected.
  #encodeAccount(addr: Uint8Array, nonce: bigint, balance: bigint): Uint8Array {
    if (this.#compact()) {
      if (nonce > 0xffffffffn) throw err.overflow();
      const rec = new Uint8Array(32);
      rec.set(addr, 0);
      new DataView(rec.buffer).setUint32(20, Number(nonce), true);
      putBE(rec.subarray(24, 32), balance);
      return rec;
    }
    const rec = new Uint8Array(64);
    rec.set(addr, 0);
    new DataView(rec.buffer).setBigUint64(24, nonce, true);
    putBE(rec.subarray(32, 64), balance);
    return rec;
  }

  /**
   * Looks an account up. Absence is a result, not an error: returns
   * `{address, nonce, balance}` (BigInt nonce/balance) or null.
   */
  async getAccount(addr: AddressLike): Promise<Account | null> {
    const address = toAddress(addr);
    if (isZero(address)) throw err.zeroAddress();
    const found = await this.#lookup(this.#accountsTable(), address);
    if (!found) return null;
    const { nonce, balance } = this.#decodeAccount(found.slot);
    return { address: address.slice(), nonce, balance };
  }

  /**
   * Writes an account's nonce and balance, inserting the record if the
   * address has none. Throws kind "tableFull" when the insert would pass
   * the load limit — in a guest that means rejecting the input (spec §6.3).
   * In the compact record (spec §7) a nonce past uint32 or a balance past
   * uint64 is kind "overflow", which also means rejecting the input.
   */
  async setAccount(addr: AddressLike, nonce?: bigint | number, balance?: bigint | number): Promise<void> {
    const address = toAddress(addr);
    if (isZero(address)) throw err.zeroAddress();
    const rec = this.#encodeAccount(address, BigInt(nonce ?? 0), BigInt(balance ?? 0));
    const t = this.#accountsTable();
    const found = await this.#lookup(t, address);
    if (found) {
      // Update in place, touching only the record bytes: in the wide
      // profile the rest of the slot is token columns.
      found.slot.set(rec, 0);
      await this.#writeSlot(t, found.index, found.slot);
      return;
    }
    const n = await this.#counter(t.countOff);
    if (n >= BigInt(t.limit)) throw err.tableFull();
    const full = new Uint8Array(t.slotSize);
    full.set(rec, 0);
    await this.#insert(t, full);
  }

  /**
   * Removes an account's record. A record with a nonzero nonce is refused
   * (kind "nonceProtected") unless force is set: deleting it re-enables
   * replay of that account's past transactions (spec §7). In the wide
   * profile the record's token columns go with it. Returns false when there
   * was no record.
   */
  async deleteAccount(addr: AddressLike, force = false): Promise<boolean> {
    const address = toAddress(addr);
    if (isZero(address)) throw err.zeroAddress();
    const t = this.#accountsTable();
    const found = await this.#lookup(t, address);
    if (!found) return false;
    const { nonce } = this.#decodeAccount(found.slot);
    if (!force && nonce !== 0n) throw err.nonceProtected();
    await this.#remove(t, found.index);
    return true;
  }

  // -------------------------------------------------------------- registry

  /** Resolves a token address to `{id, address, width}`, or null. */
  tokenByAddress(addr: AddressLike): ResolvedToken | null {
    const address = toAddress(addr);
    for (let i = 0; i < this.tokens_.length; i++) {
      if (bytesEqual(this.tokens_[i].address, address)) {
        return { id: i, address: this.tokens_[i].address.slice(), width: this.tokens_[i].width };
      }
    }
    return null;
  }

  /**
   * Appends a token to the registry and returns its id. In the wide profile
   * the new column must fit the slot (spec §7, kind "tableFull"); everywhere
   * the registry must have room (spec §8, kind "registryFull"). Both mean
   * rejecting the input in a guest.
   */
  async registerToken(addr: AddressLike, width: number): Promise<number> {
    const address = toAddress(addr);
    if (this.cfg.registryOffset === 0) throw err.profile();
    if (width !== 8 && width !== 16 && width !== 32) throw err.badWidth();
    if (this.tokenByAddress(address)) throw err.duplicateToken();
    const n = this.tokens_.length;
    if (n >= this.cfg.registryCapacity) throw err.registryFull();
    if (this.cfg.profile === ProfileWide) {
      let sum = 64;
      for (const t of this.tokens_) sum += t.width;
      if (sum + width > this.cfg.slotSize) {
        throw err.tableFull(`columns would need ${sum + width} bytes in a ${this.cfg.slotSize}-byte slot`);
      }
    }
    if (this.cfg.profile === ProfileSparse && this.cfg.sparseSlotSize === 32 && width !== 8) {
      throw err.badWidth('32-byte sparse slots require width 8');
    }
    if (this.#compact() && width !== 8) {
      throw err.badWidth('the compact account record requires width 8');
    }
    const e = new Uint8Array(32);
    e.set(address, 0);
    e[20] = width;
    await this.store.writeAt(this.cfg.registryOffset + 32 * n, e);
    await this.#setCounter(OFF_TOKEN_COUNT, BigInt(n + 1));
    let col = 64;
    if (this.colOff.length > 0) {
      col = this.colOff[this.colOff.length - 1] + this.tokens_[this.tokens_.length - 1].width;
    }
    this.tokens_.push({ address: address.slice(), width });
    this.colOff.push(col);
    return n;
  }

  // -------------------------------------------------------- token balances

  /**
   * Reads an account's balance of a registered token (BigInt). An absent
   * record (or column of an absent account) is zero, not an error.
   */
  async getTokenBalance(addr: AddressLike, id: number): Promise<bigint> {
    const address = toAddress(addr);
    if (isZero(address)) throw err.zeroAddress();
    if (!(Number.isInteger(id) && id >= 0 && id < this.tokens_.length)) throw err.unknownToken();
    switch (this.cfg.profile) {
      case ProfileWide: {
        const found = await this.#lookup(this.#accountsTable(), address);
        if (!found) return 0n;
        const off = this.colOff[id];
        return readBE(found.slot.subarray(off, off + this.tokens_[id].width));
      }
      case ProfileSparse: {
        const found = await this.#lookup(this.#sparseTable(), sparseKey(id, address));
        if (!found) return 0n;
        if (this.cfg.sparseSlotSize === 32) return readBE(found.slot.subarray(24, 32));
        return readBE(found.slot.subarray(32, 64));
      }
      default:
        throw err.profile();
    }
  }

  /**
   * Writes an account's balance of a registered token. Kind "overflow"
   * (value exceeds the token's declared width) and "tableFull" both mean
   * rejecting the input in a guest. In the wide profile a missing account
   * record is created with a zero nonce and native balance; in the sparse
   * profile a balance reaching zero deletes its record (spec §9).
   */
  async setTokenBalance(addr: AddressLike, id: number, value?: bigint | number): Promise<void> {
    const address = toAddress(addr);
    const v = BigInt(value ?? 0);
    if (isZero(address)) throw err.zeroAddress();
    if (!(Number.isInteger(id) && id >= 0 && id < this.tokens_.length)) throw err.unknownToken();
    const width = this.tokens_[id].width;
    if (v < 0n || v >> BigInt(8 * width) !== 0n) throw err.overflow();
    switch (this.cfg.profile) {
      case ProfileWide: {
        const t = this.#accountsTable();
        const found = await this.#lookup(t, address);
        const off = this.colOff[id];
        if (!found) {
          if (v === 0n) return;
          const n = await this.#counter(t.countOff);
          if (n >= BigInt(t.limit)) throw err.tableFull();
          const full = new Uint8Array(t.slotSize);
          full.set(address, 0);
          putBE(full.subarray(off, off + width), v);
          await this.#insert(t, full);
          return;
        }
        putBE(found.slot.subarray(off, off + width), v);
        await this.#writeSlot(t, found.index, found.slot);
        return;
      }
      case ProfileSparse: {
        const t = this.#sparseTable();
        const key = sparseKey(id, address);
        const found = await this.#lookup(t, key);
        if (v === 0n) {
          if (!found) return;
          await this.#remove(t, found.index);
          return;
        }
        if (found) {
          this.#putSparseBalance(found.slot, v);
          await this.#writeSlot(t, found.index, found.slot);
          return;
        }
        const n = await this.#counter(t.countOff);
        if (n >= BigInt(t.limit)) throw err.tableFull();
        const rec = new Uint8Array(t.slotSize);
        rec[0] = id & 0xff;
        rec[1] = (id >> 8) & 0xff;
        rec.set(address, 4);
        this.#putSparseBalance(rec, v);
        await this.#insert(t, rec);
        return;
      }
      default:
        throw err.profile();
    }
  }

  #putSparseBalance(slot: Uint8Array, v: bigint): void {
    if (this.cfg.sparseSlotSize === 32) {
      putBE(slot.subarray(24, 32), v);
    } else {
      putBE(slot.subarray(32, 64), v);
    }
  }
}

// ---------------------------------------------------------------- open/format

/**
 * format writes a v1 header (and nothing else) describing `config` onto a
 * store that must be all-zero, then opens it. It is how a snapshot build
 * formats the drive image, and how tests build drives from nothing.
 */
export async function format(store: Store, config: Config): Promise<Drive> {
  const cfg = normalizeConfig(config);
  validateConfig(cfg);
  const h = new Uint8Array(HEADER_SIZE);
  const dv = new DataView(h.buffer);
  h.set(Magic, OFF_MAGIC);
  dv.setUint32(OFF_VERSION, Version, true);
  dv.setBigUint64(OFF_CAPACITY, BigInt(cfg.capacity), true);
  dv.setBigUint64(OFF_TABLE_OFFSET, BigInt(cfg.tableOffset), true);
  dv.setBigUint64(OFF_LOAD_LIMIT, BigInt(cfg.loadLimit), true);
  h.set(cfg.seed, OFF_SEED);
  dv.setUint32(OFF_PROFILE, cfg.profile, true);
  dv.setUint32(OFF_SLOT_SIZE, cfg.slotSize, true);
  dv.setBigUint64(OFF_REGISTRY_OFFSET, BigInt(cfg.registryOffset), true);
  dv.setBigUint64(OFF_REGISTRY_CAP, BigInt(cfg.registryCapacity), true);
  if (cfg.profile === ProfileSparse) {
    dv.setBigUint64(OFF_SPARSE_OFFSET, BigInt(cfg.sparseOffset), true);
    dv.setBigUint64(OFF_SPARSE_CAPACITY, BigInt(cfg.sparseCapacity), true);
    dv.setBigUint64(OFF_SPARSE_LOAD_LIMIT, BigInt(cfg.sparseLoadLimit), true);
    dv.setUint32(OFF_SPARSE_SLOT_SIZE, cfg.sparseSlotSize, true);
  }
  await store.writeAt(0, h);
  return open(store);
}

/**
 * open reads and validates the header, caches the geometry and the registry.
 */
export async function open(store: Store): Promise<Drive> {
  const h = await store.readAt(0, HEADER_SIZE);
  const dv = new DataView(h.buffer, h.byteOffset, h.byteLength);
  if (!bytesEqual(h.subarray(OFF_MAGIC, OFF_MAGIC + 8), Magic)) {
    throw new Error('not an accounts drive (bad magic)');
  }
  const version = dv.getUint32(OFF_VERSION, true);
  if (version !== Version) {
    throw new Error(`unsupported accounts drive version: ${version}`);
  }
  const flags = dv.getUint32(OFF_FLAGS, true);
  if (flags !== 0) {
    throw new Error(`unsupported accounts drive version: nonzero flags 0x${flags.toString(16)}`);
  }
  const cfg: NormalizedConfig = {
    driveLength: 0,
    capacity: Number(dv.getBigUint64(OFF_CAPACITY, true)),
    tableOffset: Number(dv.getBigUint64(OFF_TABLE_OFFSET, true)),
    loadLimit: Number(dv.getBigUint64(OFF_LOAD_LIMIT, true)),
    seed: h.slice(OFF_SEED, OFF_SEED + 32),
    profile: dv.getUint32(OFF_PROFILE, true),
    slotSize: dv.getUint32(OFF_SLOT_SIZE, true),
    registryOffset: Number(dv.getBigUint64(OFF_REGISTRY_OFFSET, true)),
    registryCapacity: Number(dv.getBigUint64(OFF_REGISTRY_CAP, true)),
    sparseOffset: Number(dv.getBigUint64(OFF_SPARSE_OFFSET, true)),
    sparseCapacity: Number(dv.getBigUint64(OFF_SPARSE_CAPACITY, true)),
    sparseLoadLimit: Number(dv.getBigUint64(OFF_SPARSE_LOAD_LIMIT, true)),
    sparseSlotSize: dv.getUint32(OFF_SPARSE_SLOT_SIZE, true),
  };
  if (cfg.slotSize === 0) cfg.slotSize = 64;
  const d = new Drive(store, cfg);
  await d.reloadRegistry();
  return d;
}
