//! The drive proper: header, tables, registry, and the four §6 operations.

use crate::keccak::keccak256;
use crate::store::Store;
use crate::{Balance, Error};

/// Profile 0: one native/single-asset balance per account (spec §5).
pub const PROFILE_SINGLE_ASSET: u32 = 0;
/// Profile 1: token balance columns packed inside each account slot.
pub const PROFILE_WIDE: u32 = 1;
/// Profile 2: token balances in a separate sparse table.
pub const PROFILE_SPARSE: u32 = 2;

/// The header's first 8 bytes, ASCII `ctsiacct`.
pub const MAGIC: [u8; 8] = *b"ctsiacct";

/// The format version this crate implements.
pub const VERSION: u32 = 1;

const HEADER_SIZE: u64 = 4096;

// Header field offsets (spec §5).
const OFF_MAGIC: u64 = 0x00;
const OFF_VERSION: u64 = 0x08;
const OFF_FLAGS: u64 = 0x0c;
const OFF_CAPACITY: u64 = 0x10;
const OFF_TABLE_OFFSET: u64 = 0x18;
const OFF_LOAD_LIMIT: u64 = 0x20;
const OFF_LIVE_COUNT: u64 = 0x28;
const OFF_SEED: u64 = 0x30;
const OFF_PROFILE: u64 = 0x50;
const OFF_SLOT_SIZE: u64 = 0x54;
const OFF_REGISTRY_OFFSET: u64 = 0x58;
const OFF_REGISTRY_CAP: u64 = 0x60;
const OFF_TOKEN_COUNT: u64 = 0x68;
const OFF_SPARSE_OFFSET: u64 = 0x70;
const OFF_SPARSE_CAPACITY: u64 = 0x78;
const OFF_SPARSE_LOAD_LIMIT: u64 = 0x80;
const OFF_SPARSE_LIVE_COUNT: u64 = 0x88;
const OFF_SPARSE_SLOT_SIZE: u64 = 0x90;
const MIN_REGISTRY_OFFSET: u64 = 0x100;

/// The geometry of a drive to be formatted. Every field is a deployment
/// constant (spec §5); [`Drive::format`] validates the lot. Zero fields take
/// defaults where noted.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Config {
    /// Total drive length in bytes; must be a power of two (spec §4).
    pub drive_length: u64,
    /// Slot count of the accounts table; ≥ 2.
    pub capacity: u64,
    /// Max occupied accounts slots; 0 → ⅞ of `capacity`.
    pub load_limit: u64,
    /// Hash key for §6.1; any value, fixed at deployment.
    pub seed: [u8; 32],
    /// One of [`PROFILE_SINGLE_ASSET`], [`PROFILE_WIDE`], [`PROFILE_SPARSE`].
    pub profile: u32,
    /// Accounts-table slot size; 0 → 64. Must be 64 outside the wide
    /// profile, a power of two ≥ 128 in it.
    pub slot_size: u32,
    /// Byte offset of accounts slot 0; 0 → 4096.
    pub table_offset: u64,
    /// Registry byte offset; 0 → no registry (allowed only in profile 0).
    pub registry_offset: u64,
    /// Max token entries; ≤ 65536, nonzero iff there is a registry.
    pub registry_capacity: u64,
    /// Sparse-table byte offset (profile 2).
    pub sparse_offset: u64,
    /// Slot count of the sparse table (profile 2); ≥ 2.
    pub sparse_capacity: u64,
    /// Max occupied sparse slots; 0 → ⅞ of `sparse_capacity`.
    pub sparse_load_limit: u64,
    /// Sparse slot size: 64, or 32 iff every registered width is 8; 0 → 64.
    pub sparse_slot_size: u32,
}

impl Config {
    fn apply_defaults(&mut self) {
        if self.slot_size == 0 {
            self.slot_size = 64;
        }
        if self.table_offset == 0 {
            self.table_offset = HEADER_SIZE;
        }
        if self.load_limit == 0 && self.capacity > 0 {
            self.load_limit = self.capacity - self.capacity / 8;
        }
        if self.profile == PROFILE_SPARSE {
            if self.sparse_slot_size == 0 {
                self.sparse_slot_size = 64;
            }
            if self.sparse_load_limit == 0 && self.sparse_capacity > 0 {
                self.sparse_load_limit = self.sparse_capacity - self.sparse_capacity / 8;
            }
        }
    }

    fn validate(&self) -> Result<(), Error> {
        let cfg = |m: String| Err(Error::Config(m));
        if self.drive_length == 0 || !self.drive_length.is_power_of_two() {
            return cfg(format!(
                "drive length {} is not a power of two",
                self.drive_length
            ));
        }
        if self.profile > PROFILE_SPARSE {
            return cfg(format!("unknown profile {}", self.profile));
        }
        let ss = self.slot_size as u64;
        if self.profile == PROFILE_WIDE {
            if ss < 128 || !ss.is_power_of_two() {
                return cfg(format!("wide slot size {ss}: want a power of two >= 128"));
            }
        } else if ss != 64 {
            return cfg(format!("slot size {ss}: must be 64 outside the wide profile"));
        }
        if self.capacity < 2 {
            return cfg(format!("capacity {} < 2", self.capacity));
        }
        if self.load_limit < 1 || self.load_limit >= self.capacity {
            return cfg(format!(
                "load limit {} outside [1, capacity-1]",
                self.load_limit
            ));
        }
        if self.table_offset < HEADER_SIZE || self.table_offset % ss != 0 {
            return cfg(format!(
                "table offset {}: want >= {HEADER_SIZE} and a multiple of the slot size",
                self.table_offset
            ));
        }
        // Regions to bounds- and overlap-check; the in-header registry is the
        // one sanctioned overlap (spec §5 geometry rules).
        let mut regions: Vec<(&str, u64, u64)> = vec![
            ("header", 0, HEADER_SIZE),
            ("accounts table", self.table_offset, self.capacity * ss),
        ];
        if self.profile != PROFILE_SINGLE_ASSET && self.registry_offset == 0 {
            return cfg(format!("profile {} requires a token registry", self.profile));
        }
        if self.registry_offset != 0 {
            if self.registry_capacity == 0 || self.registry_capacity > 65536 {
                return cfg(format!(
                    "registry capacity {} outside [1, 65536]",
                    self.registry_capacity
                ));
            }
            if self.registry_offset % 32 != 0 {
                return cfg(format!(
                    "registry offset {} not a multiple of 32",
                    self.registry_offset
                ));
            }
            let end = self.registry_offset + 32 * self.registry_capacity;
            if self.registry_offset < HEADER_SIZE {
                // In-header placement (spec §5).
                if self.registry_offset < MIN_REGISTRY_OFFSET || end > HEADER_SIZE {
                    return cfg(format!(
                        "in-header registry [{:#x},{:#x}) outside [{:#x},{:#x})",
                        self.registry_offset, end, MIN_REGISTRY_OFFSET, HEADER_SIZE
                    ));
                }
            } else {
                regions.push(("registry", self.registry_offset, 32 * self.registry_capacity));
            }
        }
        if self.profile == PROFILE_SPARSE {
            let sss = self.sparse_slot_size as u64;
            if sss != 64 && sss != 32 {
                return cfg(format!("sparse slot size {sss}: want 64 or 32"));
            }
            if self.sparse_capacity < 2 {
                return cfg(format!("sparse capacity {} < 2", self.sparse_capacity));
            }
            if self.sparse_load_limit < 1 || self.sparse_load_limit >= self.sparse_capacity {
                return cfg(format!(
                    "sparse load limit {} outside [1, capacity-1]",
                    self.sparse_load_limit
                ));
            }
            if self.sparse_offset < HEADER_SIZE || self.sparse_offset % sss != 0 {
                return cfg(format!(
                    "sparse offset {}: want >= {HEADER_SIZE} and a multiple of the slot size",
                    self.sparse_offset
                ));
            }
            regions.push(("sparse table", self.sparse_offset, self.sparse_capacity * sss));
        }
        for &(name, off, len) in &regions {
            let end = off.checked_add(len);
            match end {
                Some(end) if end <= self.drive_length => {}
                _ => {
                    return cfg(format!(
                        "{name} [{off},{}) outside drive of {} bytes",
                        off.wrapping_add(len),
                        self.drive_length
                    ))
                }
            }
        }
        for (i, &(an, ao, al)) in regions.iter().enumerate() {
            for &(bn, bo, bl) in &regions[i + 1..] {
                if ao < bo + bl && bo < ao + al {
                    return cfg(format!("{an} overlaps {bn}"));
                }
            }
        }
        Ok(())
    }
}

/// One registry entry (spec §8).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Token {
    /// The token's 20-byte address; the zero address conventionally stands
    /// for the native asset in a profile-0 declaration.
    pub address: [u8; 20],
    /// Declared balance width in bytes: 8, 16 or 32.
    pub width: u32,
}

/// The fixed 64-byte record of spec §7, decoded.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Account {
    /// The account's 20-byte address (nonzero).
    pub address: [u8; 20],
    /// Transaction nonce (`uint64` little-endian on the drive).
    pub nonce: u64,
    /// Native/single-asset balance, 32 bytes big-endian.
    pub balance: Balance,
}

/// Which of the two hash tables a raw-slot read refers to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum TableKind {
    Accounts,
    Sparse,
}

/// One table's geometry, so the probing code (spec §6) is written once.
#[derive(Debug, Clone, Copy)]
struct Table {
    base: u64,
    capacity: u64,
    slot_size: u64,
    count_off: u64, // header offset of the live counter
    limit: u64,
    kind: TableKind,
}

impl Table {
    /// The record's key: 20-byte address (accounts) or token id ‖ address,
    /// 22 bytes (sparse). Returns the filled prefix of a stack buffer.
    fn key<'a>(&self, slot: &[u8], buf: &'a mut [u8; 22]) -> &'a [u8] {
        match self.kind {
            TableKind::Accounts => {
                buf[..20].copy_from_slice(&slot[0..20]);
                &buf[..20]
            }
            TableKind::Sparse => {
                buf[..2].copy_from_slice(&slot[0..2]);
                buf[2..22].copy_from_slice(&slot[4..24]);
                &buf[..22]
            }
        }
    }

    /// A slot is live iff its address field is nonzero (spec §6).
    fn live(&self, slot: &[u8]) -> bool {
        let off = match self.kind {
            TableKind::Accounts => 0,
            TableKind::Sparse => 4,
        };
        !is_zero(&slot[off..off + 20])
    }
}

fn is_zero(b: &[u8]) -> bool {
    b.iter().all(|&x| x == 0)
}

/// Spec §6.1: `home(key) = LE64(keccak256(seed ‖ key)[0..8]) mod C`.
fn home(seed: &[u8; 32], key: &[u8], capacity: u64) -> u64 {
    let h = keccak256(&[seed, key]);
    let mut le = [0u8; 8];
    le.copy_from_slice(&h[..8]);
    u64::from_le_bytes(le) % capacity
}

/// The sparse table's key encoding: 2-byte little-endian token id ‖ address.
fn sparse_key(id: u16, addr: &[u8; 20]) -> [u8; 22] {
    let mut k = [0u8; 22];
    k[..2].copy_from_slice(&id.to_le_bytes());
    k[2..].copy_from_slice(addr);
    k
}

/// An open accounts drive over a [`Store`].
///
/// Geometry is cached at open (it is immutable by spec); the registry is
/// cached too and refreshed by writes through this instance — a host watching
/// a drive someone else appends to should call [`Drive::reload_registry`]
/// when the token count moves.
#[derive(Debug)]
pub struct Drive<S: Store> {
    store: S,
    cfg: Config,
    tokens: Vec<Token>,
    col_off: Vec<u32>, // wide profile: column byte offset per token id
}

impl<S: Store> Drive<S> {
    /// Writes a v1 header (and nothing else) describing `cfg` onto a store
    /// that must be all-zero, then opens it. It is how a snapshot build
    /// formats the drive image, and how tests build drives from nothing.
    pub fn format(mut store: S, mut cfg: Config) -> Result<Self, Error> {
        cfg.apply_defaults();
        cfg.validate()?;
        let mut h = vec![0u8; HEADER_SIZE as usize];
        h[OFF_MAGIC as usize..OFF_MAGIC as usize + 8].copy_from_slice(&MAGIC);
        put_u32(&mut h, OFF_VERSION, VERSION);
        put_u64(&mut h, OFF_CAPACITY, cfg.capacity);
        put_u64(&mut h, OFF_TABLE_OFFSET, cfg.table_offset);
        put_u64(&mut h, OFF_LOAD_LIMIT, cfg.load_limit);
        h[OFF_SEED as usize..OFF_SEED as usize + 32].copy_from_slice(&cfg.seed);
        put_u32(&mut h, OFF_PROFILE, cfg.profile);
        put_u32(&mut h, OFF_SLOT_SIZE, cfg.slot_size);
        put_u64(&mut h, OFF_REGISTRY_OFFSET, cfg.registry_offset);
        put_u64(&mut h, OFF_REGISTRY_CAP, cfg.registry_capacity);
        if cfg.profile == PROFILE_SPARSE {
            put_u64(&mut h, OFF_SPARSE_OFFSET, cfg.sparse_offset);
            put_u64(&mut h, OFF_SPARSE_CAPACITY, cfg.sparse_capacity);
            put_u64(&mut h, OFF_SPARSE_LOAD_LIMIT, cfg.sparse_load_limit);
            put_u32(&mut h, OFF_SPARSE_SLOT_SIZE, cfg.sparse_slot_size);
        }
        store.write_at(0, &h)?;
        Self::open(store)
    }

    /// Reads and validates the header, caches the geometry and the registry.
    pub fn open(store: S) -> Result<Self, Error> {
        let mut h = vec![0u8; HEADER_SIZE as usize];
        store.read_at(0, &mut h)?;
        if h[..8] != MAGIC {
            return Err(Error::NotFormatted);
        }
        let version = get_u32(&h, OFF_VERSION);
        if version != VERSION {
            return Err(Error::UnsupportedVersion(version));
        }
        let flags = get_u32(&h, OFF_FLAGS);
        if flags != 0 {
            // A nonzero flag marks a later, unknown revision (spec §13).
            return Err(Error::UnsupportedVersion(version));
        }
        let mut cfg = Config {
            capacity: get_u64(&h, OFF_CAPACITY),
            table_offset: get_u64(&h, OFF_TABLE_OFFSET),
            load_limit: get_u64(&h, OFF_LOAD_LIMIT),
            profile: get_u32(&h, OFF_PROFILE),
            slot_size: get_u32(&h, OFF_SLOT_SIZE),
            registry_offset: get_u64(&h, OFF_REGISTRY_OFFSET),
            registry_capacity: get_u64(&h, OFF_REGISTRY_CAP),
            sparse_offset: get_u64(&h, OFF_SPARSE_OFFSET),
            sparse_capacity: get_u64(&h, OFF_SPARSE_CAPACITY),
            sparse_load_limit: get_u64(&h, OFF_SPARSE_LOAD_LIMIT),
            sparse_slot_size: get_u32(&h, OFF_SPARSE_SLOT_SIZE),
            ..Config::default()
        };
        cfg.seed
            .copy_from_slice(&h[OFF_SEED as usize..OFF_SEED as usize + 32]);
        if cfg.slot_size == 0 {
            cfg.slot_size = 64;
        }
        let mut d = Drive {
            store,
            cfg,
            tokens: Vec::new(),
            col_off: Vec::new(),
        };
        d.reload_registry()?;
        Ok(d)
    }

    /// The drive's geometry.
    pub fn config(&self) -> &Config {
        &self.cfg
    }

    /// The underlying store.
    pub fn store(&self) -> &S {
        &self.store
    }

    /// The underlying store, mutably (e.g. to sync a [`crate::FileStore`]).
    pub fn store_mut(&mut self) -> &mut S {
        &mut self.store
    }

    /// Consumes the drive, returning the store.
    pub fn into_store(self) -> S {
        self.store
    }

    /// The cached registry, in id order.
    pub fn tokens(&self) -> &[Token] {
        &self.tokens
    }

    /// Re-reads the registry (and the wide column offsets) from the store.
    pub fn reload_registry(&mut self) -> Result<(), Error> {
        self.tokens.clear();
        self.col_off.clear();
        if self.cfg.registry_offset == 0 {
            return Ok(());
        }
        let n = self.counter(OFF_TOKEN_COUNT)?;
        if n > self.cfg.registry_capacity {
            return Err(Error::Corrupt(format!(
                "token count {n} exceeds registry capacity {}",
                self.cfg.registry_capacity
            )));
        }
        let mut col: u32 = 64;
        for i in 0..n {
            let mut e = [0u8; 32];
            self.store.read_at(self.cfg.registry_offset + 32 * i, &mut e)?;
            let width = e[20] as u32;
            if width != 8 && width != 16 && width != 32 {
                return Err(Error::BadWidth);
            }
            let mut address = [0u8; 20];
            address.copy_from_slice(&e[..20]);
            self.tokens.push(Token { address, width });
            self.col_off.push(col);
            col += width;
        }
        Ok(())
    }

    // ------------------------------------------------------------- counters

    fn counter(&self, off: u64) -> Result<u64, Error> {
        let mut b = [0u8; 8];
        self.store.read_at(off, &mut b)?;
        Ok(u64::from_le_bytes(b))
    }

    fn set_counter(&mut self, off: u64, v: u64) -> Result<(), Error> {
        self.store.write_at(off, &v.to_le_bytes())
    }

    /// The accounts table's occupied-slot count.
    pub fn live_count(&self) -> Result<u64, Error> {
        self.counter(OFF_LIVE_COUNT)
    }

    /// The sparse table's occupied-slot count.
    pub fn sparse_live_count(&self) -> Result<u64, Error> {
        self.counter(OFF_SPARSE_LIVE_COUNT)
    }

    /// The number of registered tokens.
    pub fn token_count(&self) -> Result<u64, Error> {
        self.counter(OFF_TOKEN_COUNT)
    }

    // --------------------------------------------------------------- tables

    fn accounts_table(&self) -> Table {
        Table {
            base: self.cfg.table_offset,
            capacity: self.cfg.capacity,
            slot_size: self.cfg.slot_size as u64,
            count_off: OFF_LIVE_COUNT,
            limit: self.cfg.load_limit,
            kind: TableKind::Accounts,
        }
    }

    fn sparse_table(&self) -> Table {
        Table {
            base: self.cfg.sparse_offset,
            capacity: self.cfg.sparse_capacity,
            slot_size: self.cfg.sparse_slot_size as u64,
            count_off: OFF_SPARSE_LIVE_COUNT,
            limit: self.cfg.sparse_load_limit,
            kind: TableKind::Sparse,
        }
    }

    fn read_slot(&self, t: &Table, i: u64) -> Result<Vec<u8>, Error> {
        let mut s = vec![0u8; t.slot_size as usize];
        self.store.read_at(t.base + t.slot_size * i, &mut s)?;
        Ok(s)
    }

    fn write_slot(&mut self, t: &Table, i: u64, s: &[u8]) -> Result<(), Error> {
        self.store.write_at(t.base + t.slot_size * i, s)
    }

    fn displacement(&self, t: &Table, slot: &[u8], at: u64) -> u64 {
        let mut kb = [0u8; 22];
        let key = t.key(slot, &mut kb);
        (at + t.capacity - home(&self.cfg.seed, key, t.capacity)) % t.capacity
    }

    /// Spec §6.2, returning the found record's slot index and bytes.
    fn lookup(&self, t: &Table, key: &[u8]) -> Result<Option<(u64, Vec<u8>)>, Error> {
        let h = home(&self.cfg.seed, key, t.capacity);
        for dist in 0..t.capacity {
            let i = (h + dist) % t.capacity;
            let slot = self.read_slot(t, i)?;
            if !t.live(&slot) {
                return Ok(None);
            }
            let mut kb = [0u8; 22];
            if t.key(&slot, &mut kb) == key {
                return Ok(Some((i, slot)));
            }
            if self.displacement(t, &slot, i) < dist {
                return Ok(None);
            }
        }
        Ok(None) // unreachable while load limit < capacity
    }

    /// Spec §6.3. The caller has checked the key is absent and the load
    /// limit has room; insert bumps the live counter itself. The strict `<`
    /// swap is normative — chains are first-come-first-placed among equal
    /// displacements.
    fn insert(&mut self, t: &Table, rec: Vec<u8>) -> Result<(), Error> {
        let mut carry = rec;
        let mut kb = [0u8; 22];
        let mut h = home(&self.cfg.seed, t.key(&carry, &mut kb), t.capacity);
        let mut dist: u64 = 0;
        loop {
            let i = (h + dist) % t.capacity;
            let slot = self.read_slot(t, i)?;
            if !t.live(&slot) {
                self.write_slot(t, i, &carry)?;
                let n = self.counter(t.count_off)?;
                return self.set_counter(t.count_off, n + 1);
            }
            let disp = self.displacement(t, &slot, i);
            if disp < dist {
                self.write_slot(t, i, &carry)?;
                carry = slot;
                let mut kb = [0u8; 22];
                h = home(&self.cfg.seed, t.key(&carry, &mut kb), t.capacity);
                dist = disp;
            }
            dist += 1;
        }
    }

    /// Spec §6.4 (backward shift) from a known slot index; no tombstones.
    fn remove(&mut self, t: &Table, mut i: u64) -> Result<(), Error> {
        loop {
            let j = (i + 1) % t.capacity;
            let next = self.read_slot(t, j)?;
            if !t.live(&next) || self.displacement(t, &next, j) == 0 {
                let zero = vec![0u8; t.slot_size as usize];
                self.write_slot(t, i, &zero)?;
                let n = self.counter(t.count_off)?;
                return self.set_counter(t.count_off, n - 1);
            }
            self.write_slot(t, i, &next)?;
            i = j;
        }
    }

    /// Reads the raw bytes of accounts-table slot `index` — for hosts and
    /// verifiers working at the byte level (and for conformance tests).
    pub fn account_slot(&self, index: u64) -> Result<Vec<u8>, Error> {
        let t = self.accounts_table();
        if index >= t.capacity {
            return Err(Error::OutOfBounds {
                off: index,
                len: 1,
                size: t.capacity,
            });
        }
        self.read_slot(&t, index)
    }

    /// Reads the raw bytes of sparse-table slot `index` (profile 2 only).
    pub fn sparse_slot(&self, index: u64) -> Result<Vec<u8>, Error> {
        if self.cfg.sparse_capacity == 0 {
            return Err(Error::Profile);
        }
        let t = self.sparse_table();
        if index >= t.capacity {
            return Err(Error::OutOfBounds {
                off: index,
                len: 1,
                size: t.capacity,
            });
        }
        self.read_slot(&t, index)
    }

    // ------------------------------------------------------------- accounts

    /// Looks an account up; `None` when the address has no record (which is
    /// a proven statement, not a failure).
    pub fn get_account(&self, addr: &[u8; 20]) -> Result<Option<Account>, Error> {
        if is_zero(addr) {
            return Err(Error::ZeroAddress);
        }
        let t = self.accounts_table();
        match self.lookup(&t, addr)? {
            None => Ok(None),
            Some((_, slot)) => {
                let mut nonce = [0u8; 8];
                nonce.copy_from_slice(&slot[24..32]);
                let mut balance = [0u8; 32];
                balance.copy_from_slice(&slot[32..64]);
                Ok(Some(Account {
                    address: *addr,
                    nonce: u64::from_le_bytes(nonce),
                    balance,
                }))
            }
        }
    }

    /// Writes an account's nonce and balance, inserting the record if the
    /// address has none. [`Error::TableFull`] means the input carrying this
    /// change must be rejected (spec §6.3).
    pub fn set_account(
        &mut self,
        addr: &[u8; 20],
        nonce: u64,
        balance: &Balance,
    ) -> Result<(), Error> {
        if is_zero(addr) {
            return Err(Error::ZeroAddress);
        }
        let mut rec = [0u8; 64];
        rec[0..20].copy_from_slice(addr);
        rec[24..32].copy_from_slice(&nonce.to_le_bytes());
        rec[32..64].copy_from_slice(balance);
        let t = self.accounts_table();
        if let Some((i, mut slot)) = self.lookup(&t, addr)? {
            // Update in place, touching only the 64-byte record: in the wide
            // profile the rest of the slot is token columns.
            slot[..64].copy_from_slice(&rec);
            return self.write_slot(&t, i, &slot);
        }
        let n = self.counter(t.count_off)?;
        if n >= t.limit {
            return Err(Error::TableFull);
        }
        let mut full = vec![0u8; t.slot_size as usize];
        full[..64].copy_from_slice(&rec);
        self.insert(&t, full)
    }

    /// Removes an account's record. A record with a nonzero nonce is refused
    /// unless `force` is set: deleting it re-enables replay of that account's
    /// past transactions (spec §7). In the wide profile the record's token
    /// columns go with it. Returns `false` when there was no record.
    pub fn delete_account(&mut self, addr: &[u8; 20], force: bool) -> Result<bool, Error> {
        if is_zero(addr) {
            return Err(Error::ZeroAddress);
        }
        let t = self.accounts_table();
        let Some((i, slot)) = self.lookup(&t, addr)? else {
            return Ok(false);
        };
        if !force && !is_zero(&slot[24..32]) {
            return Err(Error::NonceProtected);
        }
        self.remove(&t, i)?;
        Ok(true)
    }

    // ------------------------------------------------------------- registry

    /// Resolves a token address to its id and entry.
    pub fn token_by_address(&self, addr: &[u8; 20]) -> Option<(u16, Token)> {
        self.tokens
            .iter()
            .position(|t| &t.address == addr)
            .map(|i| (i as u16, self.tokens[i]))
    }

    /// Appends a token to the registry and returns its id. In the wide
    /// profile the new column must fit the slot (spec §7); everywhere the
    /// registry must have room (spec §8). Both failures mean rejecting the
    /// input.
    pub fn register_token(&mut self, addr: &[u8; 20], width: u32) -> Result<u16, Error> {
        if self.cfg.registry_offset == 0 {
            return Err(Error::Profile);
        }
        if width != 8 && width != 16 && width != 32 {
            return Err(Error::BadWidth);
        }
        if self.token_by_address(addr).is_some() {
            return Err(Error::DuplicateToken);
        }
        let n = self.tokens.len() as u64;
        if n >= self.cfg.registry_capacity {
            return Err(Error::RegistryFull);
        }
        if self.cfg.profile == PROFILE_WIDE {
            // Wide column budget: all columns must fit slotSize − 64.
            let sum: u32 = 64 + self.tokens.iter().map(|t| t.width).sum::<u32>();
            if sum + width > self.cfg.slot_size {
                return Err(Error::TableFull);
            }
        }
        if self.cfg.profile == PROFILE_SPARSE && self.cfg.sparse_slot_size == 32 && width != 8 {
            // 32-byte sparse slots hold u64 balances only (spec §9).
            return Err(Error::BadWidth);
        }
        let mut e = [0u8; 32];
        e[..20].copy_from_slice(addr);
        e[20] = width as u8;
        self.store.write_at(self.cfg.registry_offset + 32 * n, &e)?;
        self.set_counter(OFF_TOKEN_COUNT, n + 1)?;
        let col = match (self.col_off.last(), self.tokens.last()) {
            (Some(&c), Some(t)) => c + t.width,
            _ => 64,
        };
        self.tokens.push(Token {
            address: *addr,
            width,
        });
        self.col_off.push(col);
        Ok(n as u16)
    }

    // ------------------------------------------------------- token balances

    /// Reads an account's balance of a registered token. An absent record
    /// (or column of an absent account) is zero, not an error.
    pub fn get_token_balance(&self, addr: &[u8; 20], id: u16) -> Result<Balance, Error> {
        if is_zero(addr) {
            return Err(Error::ZeroAddress);
        }
        if id as usize >= self.tokens.len() {
            return Err(Error::UnknownToken);
        }
        let mut out = [0u8; 32];
        match self.cfg.profile {
            PROFILE_WIDE => {
                let t = self.accounts_table();
                if let Some((_, slot)) = self.lookup(&t, addr)? {
                    let off = self.col_off[id as usize] as usize;
                    let w = self.tokens[id as usize].width as usize;
                    out[32 - w..].copy_from_slice(&slot[off..off + w]);
                }
                Ok(out)
            }
            PROFILE_SPARSE => {
                let t = self.sparse_table();
                if let Some((_, slot)) = self.lookup(&t, &sparse_key(id, addr))? {
                    if self.cfg.sparse_slot_size == 32 {
                        out[24..].copy_from_slice(&slot[24..32]);
                    } else {
                        out.copy_from_slice(&slot[32..64]);
                    }
                }
                Ok(out)
            }
            _ => Err(Error::Profile),
        }
    }

    /// Writes an account's balance of a registered token.
    /// [`Error::Overflow`] (value exceeds the token's declared width) and
    /// [`Error::TableFull`] both mean rejecting the input. In the wide
    /// profile a missing account record is created with a zero nonce and
    /// native balance; in the sparse profile a balance reaching zero deletes
    /// its record (spec §9).
    pub fn set_token_balance(
        &mut self,
        addr: &[u8; 20],
        id: u16,
        value: &Balance,
    ) -> Result<(), Error> {
        if is_zero(addr) {
            return Err(Error::ZeroAddress);
        }
        if id as usize >= self.tokens.len() {
            return Err(Error::UnknownToken);
        }
        let w = self.tokens[id as usize].width as usize;
        // Big-endian: the value fits its width iff every byte above it is 0.
        if !is_zero(&value[..32 - w]) {
            return Err(Error::Overflow);
        }
        match self.cfg.profile {
            PROFILE_WIDE => {
                let t = self.accounts_table();
                let off = self.col_off[id as usize] as usize;
                match self.lookup(&t, addr)? {
                    Some((i, mut slot)) => {
                        slot[off..off + w].copy_from_slice(&value[32 - w..]);
                        self.write_slot(&t, i, &slot)
                    }
                    None => {
                        if is_zero(value) {
                            return Ok(());
                        }
                        let n = self.counter(t.count_off)?;
                        if n >= t.limit {
                            return Err(Error::TableFull);
                        }
                        let mut full = vec![0u8; t.slot_size as usize];
                        full[0..20].copy_from_slice(addr);
                        full[off..off + w].copy_from_slice(&value[32 - w..]);
                        self.insert(&t, full)
                    }
                }
            }
            PROFILE_SPARSE => {
                let t = self.sparse_table();
                let key = sparse_key(id, addr);
                let found = self.lookup(&t, &key)?;
                if is_zero(value) {
                    return match found {
                        None => Ok(()),
                        Some((i, _)) => self.remove(&t, i),
                    };
                }
                match found {
                    Some((i, mut slot)) => {
                        self.put_sparse_balance(&mut slot, value);
                        self.write_slot(&t, i, &slot)
                    }
                    None => {
                        let n = self.counter(t.count_off)?;
                        if n >= t.limit {
                            return Err(Error::TableFull);
                        }
                        let mut rec = vec![0u8; t.slot_size as usize];
                        rec[0..2].copy_from_slice(&id.to_le_bytes());
                        rec[4..24].copy_from_slice(addr);
                        self.put_sparse_balance(&mut rec, value);
                        self.insert(&t, rec)
                    }
                }
            }
            _ => Err(Error::Profile),
        }
    }

    fn put_sparse_balance(&self, slot: &mut [u8], value: &Balance) {
        if self.cfg.sparse_slot_size == 32 {
            slot[24..32].copy_from_slice(&value[24..]);
        } else {
            slot[32..64].copy_from_slice(value);
        }
    }
}

fn put_u32(h: &mut [u8], off: u64, v: u32) {
    h[off as usize..off as usize + 4].copy_from_slice(&v.to_le_bytes());
}

fn put_u64(h: &mut [u8], off: u64, v: u64) {
    h[off as usize..off as usize + 8].copy_from_slice(&v.to_le_bytes());
}

fn get_u32(h: &[u8], off: u64) -> u32 {
    let mut b = [0u8; 4];
    b.copy_from_slice(&h[off as usize..off as usize + 4]);
    u32::from_le_bytes(b)
}

fn get_u64(h: &[u8], off: u64) -> u64 {
    let mut b = [0u8; 8];
    b.copy_from_slice(&h[off as usize..off as usize + 8]);
    u64::from_le_bytes(b)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{balance_from_u128, balance_from_u64, balance_to_u64, MemStore};

    fn addr(b: u8) -> [u8; 20] {
        [b; 20]
    }

    fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    fn unhex(s: &str) -> Vec<u8> {
        (0..s.len() / 2)
            .map(|i| u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap())
            .collect()
    }

    /// Pins the five spec home slots at C=8 and the two mod-2^19 vectors
    /// (Appendix B).
    #[test]
    fn home_slots() {
        let seed = [0u8; 32];
        for (b, want) in [(0xaau8, 5u64), (0xbb, 1), (0xcc, 7), (0xdd, 1), (0xee, 1)] {
            assert_eq!(home(&seed, &addr(b), 8), want, "home({b:02x}..) mod 8");
        }
        assert_eq!(home(&seed, &addr(0xbb), 1 << 19), 342121);
        assert_eq!(home(&seed, &sparse_key(3, &addr(0xbb)), 1 << 19), 117477);
    }

    /// Pins the sparse-key keccak vector from Appendix B.
    #[test]
    fn sparse_key_vector() {
        let seed = [0u8; 32];
        let h = keccak256(&[&seed, &sparse_key(3, &addr(0xbb))]);
        assert_eq!(
            hex(&h),
            "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3"
        );
    }

    /// Pins the spec's account record encoding (Appendix B).
    #[test]
    fn account_record_encoding() {
        let s = MemStore::new(1 << 16);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                ..Config::default()
            },
        )
        .unwrap();
        d.set_account(&addr(0xbb), 7, &balance_from_u64(5)).unwrap();
        let want = unhex(
            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000\
             0000000000000000000000000000000000000000000000000000000000000005",
        );
        // home(bb..) mod 8 = 1.
        let got = &d.store().bytes()[4096 + 64..4096 + 128];
        assert_eq!(hex(got), hex(&want));
    }

    /// Pins the spec's 32-byte sparse record encoding (Appendix B).
    #[test]
    fn sparse32_record_encoding() {
        let s = MemStore::new(1 << 16);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                profile: PROFILE_SPARSE,
                registry_offset: 0x100,
                registry_capacity: 8,
                sparse_offset: 8192,
                sparse_capacity: 8,
                sparse_slot_size: 32,
                ..Config::default()
            },
        )
        .unwrap();
        for i in 0..4u8 {
            d.register_token(&addr(i + 1), 8).unwrap();
        }
        d.set_token_balance(&addr(0xbb), 3, &balance_from_u64(1_000_000))
            .unwrap();
        // sparse home(id 3, bb..) mod 8: LE64(e5cad187…) = 0xbfc3cb1987d1cae5 → 5.
        let got = &d.store().bytes()[8192 + 32 * 5..8192 + 32 * 6];
        assert_eq!(
            hex(got),
            "03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240"
        );
    }

    /// Replays the spec's Appendix B probe walkthrough at C=8.
    #[test]
    fn walkthrough() {
        let s = MemStore::new(1 << 16);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                load_limit: 7,
                ..Config::default()
            },
        )
        .unwrap();
        let one = balance_from_u64(1);
        for b in [0xbbu8, 0xdd, 0xee, 0xaa] {
            d.set_account(&addr(b), 0, &one).unwrap();
        }
        let slot_addr = |d: &Drive<MemStore>, i: usize| d.store().bytes()[4096 + 64 * i];
        for (slot, want) in [(1, 0xbbu8), (2, 0xdd), (3, 0xee), (5, 0xaa)] {
            assert_eq!(slot_addr(&d, slot), want, "slot {slot}");
        }
        // Absent key with home 1 terminates via the empty slot 4.
        assert!(d.get_account(&addr(0x99)).unwrap().is_none());
        // Delete dd: ee backward-shifts into slot 2, slot 3 zeroes.
        assert!(d.delete_account(&addr(0xdd), false).unwrap());
        assert_eq!(slot_addr(&d, 2), 0xee);
        assert_eq!(slot_addr(&d, 3), 0);
        for b in [0xbbu8, 0xee, 0xaa] {
            assert!(d.get_account(&addr(b)).unwrap().is_some(), "{b:02x}.. lost");
        }
        assert!(d.get_account(&addr(0xdd)).unwrap().is_none());
        assert_eq!(d.live_count().unwrap(), 3);
    }

    #[test]
    fn table_full_and_nonce_protection() {
        let s = MemStore::new(1 << 16);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                load_limit: 4,
                ..Config::default()
            },
        )
        .unwrap();
        for b in [0xaau8, 0xbb, 0xcc, 0xdd] {
            d.set_account(&addr(b), 5, &balance_from_u64(1)).unwrap();
        }
        assert!(matches!(
            d.set_account(&addr(0xee), 0, &balance_from_u64(1)),
            Err(Error::TableFull)
        ));
        // Updating an existing account is not an insert and must still work.
        d.set_account(&addr(0xaa), 6, &balance_from_u64(2)).unwrap();
        assert!(matches!(
            d.delete_account(&addr(0xbb), false),
            Err(Error::NonceProtected)
        ));
        assert!(d.delete_account(&addr(0xbb), true).unwrap());
        d.set_account(&addr(0xee), 0, &balance_from_u64(1)).unwrap();
    }

    #[test]
    fn wide_profile() {
        let s = MemStore::new(1 << 18);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 18,
                capacity: 64,
                profile: PROFILE_WIDE,
                slot_size: 128,
                registry_offset: 0x100,
                registry_capacity: 8,
                ..Config::default()
            },
        )
        .unwrap();
        d.register_token(&addr(0x01), 32).unwrap();
        let id2 = d.register_token(&addr(0x02), 8).unwrap();
        d.register_token(&addr(0x03), 8).unwrap();
        // 64 + 32 + 8 + 8 = 112; another 32 would need 144 > 128.
        assert!(matches!(
            d.register_token(&addr(0x04), 32),
            Err(Error::TableFull)
        ));
        assert!(matches!(
            d.register_token(&addr(0x01), 8),
            Err(Error::DuplicateToken)
        ));
        // Auto-create an account by setting a token balance, then check the
        // native record write preserves the column.
        let user = addr(0x77);
        d.set_token_balance(&user, id2, &balance_from_u64(12345))
            .unwrap();
        d.set_account(&user, 9, &balance_from_u64(500)).unwrap();
        let v = d.get_token_balance(&user, id2).unwrap();
        assert_eq!(balance_to_u64(&v), Some(12345));
        let a = d.get_account(&user).unwrap().unwrap();
        assert_eq!(a.nonce, 9);
        assert_eq!(balance_to_u64(&a.balance), Some(500));
        // Width 8 column overflows past 2^64-1.
        assert!(matches!(
            d.set_token_balance(&user, id2, &balance_from_u128(1u128 << 64)),
            Err(Error::Overflow)
        ));
        // Reopen: registry and balances survive.
        let d2 = Drive::open(d.into_store()).unwrap();
        assert_eq!(d2.tokens().len(), 3);
        let v = d2.get_token_balance(&user, id2).unwrap();
        assert_eq!(balance_to_u64(&v), Some(12345));
    }

    #[test]
    fn sparse_profile() {
        let s = MemStore::new(1 << 18);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 18,
                capacity: 64,
                profile: PROFILE_SPARSE,
                registry_offset: 0x100,
                registry_capacity: 8,
                sparse_offset: 1 << 17,
                sparse_capacity: 16,
                sparse_load_limit: 8,
                ..Config::default()
            },
        )
        .unwrap();
        let id = d.register_token(&addr(0x01), 32).unwrap();
        let user = addr(0x77);
        let huge = balance_from_u128(123456789012345678901234567890u128);
        d.set_token_balance(&user, id, &huge).unwrap();
        assert_eq!(d.get_token_balance(&user, id).unwrap(), huge);
        assert_eq!(d.sparse_live_count().unwrap(), 1);
        // Zero deletes the record.
        d.set_token_balance(&user, id, &[0u8; 32]).unwrap();
        assert_eq!(d.sparse_live_count().unwrap(), 0);
        assert_eq!(d.get_token_balance(&user, id).unwrap(), [0u8; 32]);
        assert!(matches!(
            d.get_token_balance(&user, 9),
            Err(Error::UnknownToken)
        ));
    }

    #[test]
    fn sparse32_requires_width8() {
        let s = MemStore::new(1 << 16);
        let mut d = Drive::format(
            s,
            Config {
                drive_length: 1 << 16,
                capacity: 8,
                profile: PROFILE_SPARSE,
                registry_offset: 0x100,
                registry_capacity: 8,
                sparse_offset: 8192,
                sparse_capacity: 8,
                sparse_slot_size: 32,
                ..Config::default()
            },
        )
        .unwrap();
        assert!(matches!(
            d.register_token(&addr(0x01), 16),
            Err(Error::BadWidth)
        ));
    }
}
