-- accounts_drive.lua — Cartesi Accounts Drive Format v1, pure Lua 5.4.
--
-- Implements docs/ACCOUNTS-DRIVE-SPEC.md, byte-for-byte compatible with the
-- Go reference implementation (accounts-drive/go). Pure Lua 5.4, standard
-- library only: native 64-bit integers, bit operators and string.pack/unpack
-- carry the whole format, including Keccak-256.
--
-- The same code serves both sides of the drive. A guest opens it over a
-- file_store on the raw device (/dev/pmemN); a host opens it over a
-- mem_store holding a copied quiescent image (spec §11). All layout, hashing
-- and probing logic is shared, which is what keeps two independent parties
-- byte-for-byte agreed about what the drive means.
--
-- Data conventions:
--   * Addresses are Lua strings of exactly 20 raw bytes.
--   * Balances are Lua strings of raw bytes, big-endian. Inputs shorter than
--     32 bytes are left-padded with zeros; longer inputs must have all-zero
--     leading bytes (otherwise the operation fails with "overflow"). Outputs
--     are always exactly 32 bytes.
--   * Nonces are Lua integers. Lua integers are signed 64-bit, so a nonce
--     >= 2^63 round-trips through the drive with the correct bytes but
--     appears as a negative Lua integer; callers that ever reach such
--     nonces must treat the value as the unsigned interpretation of those
--     64 bits.
--   * Token ids are 0-based Lua integers (the registry index, spec §8).
--
-- Error style (used consistently across the module): failing drive
-- operations return nil, kind where kind is one of the exact taxonomy
-- strings shared by every implementation and the golden vectors:
--
--   "tableFull", "registryFull", "overflow", "nonceProtected",
--   "duplicateToken", "unknownToken", "badWidth", "zeroAddress", "profile"
--
-- In a guest, "tableFull", "registryFull" and "overflow" are exactly the
-- conditions the spec requires to be answered by rejecting the input.
-- format/open return nil, message (free text, not part of the taxonomy) for
-- invalid geometry or a malformed header. API misuse — wrong argument types
-- or lengths, out-of-bounds store access — raises with error(), since it is
-- a caller bug rather than a drive outcome.

local M = {}

-- Profiles (spec §5).
M.PROFILE_SINGLE_ASSET = 0
M.PROFILE_WIDE = 1
M.PROFILE_SPARSE = 2

-- The error-kind strings, for callers that prefer named constants.
M.kinds = {
  table_full = "tableFull",
  registry_full = "registryFull",
  overflow = "overflow",
  nonce_protected = "nonceProtected",
  duplicate_token = "duplicateToken",
  unknown_token = "unknownToken",
  bad_width = "badWidth",
  zero_address = "zeroAddress",
  profile = "profile",
}

-- ======================================================================
-- Keccak-256 (Ethereum's, pre-FIPS 0x01 padding)
--
-- The permutation follows the public-domain tiny_sha3 structure, on native
-- Lua 5.4 64-bit integers. Lua integers are *signed* 64-bit; that is fine:
-- xor, and, or, not and shifts operate on the raw 64 bits, `>>` is a
-- logical shift, and hex literals that overflow the signed range wrap to
-- the intended bit pattern. Correctness is pinned by the spec's Appendix B
-- vectors, replayed in test.lua.
-- ======================================================================

local RNDC = {
  0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
  0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
  0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
  0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
  0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
  0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}
local ROTC = { 1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14,
               27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44 }
local PILN = { 10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4,
               15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1 }

local function rotl64(x, n)
  return (x << n) | (x >> (64 - n))
end

-- st is a 0-indexed table of 25 lanes.
local function keccakf(st)
  local bc = {}
  for round = 1, 24 do
    -- theta
    for i = 0, 4 do
      bc[i] = st[i] ~ st[i + 5] ~ st[i + 10] ~ st[i + 15] ~ st[i + 20]
    end
    for i = 0, 4 do
      local t = bc[(i + 4) % 5] ~ rotl64(bc[(i + 1) % 5], 1)
      st[i] = st[i] ~ t
      st[i + 5] = st[i + 5] ~ t
      st[i + 10] = st[i + 10] ~ t
      st[i + 15] = st[i + 15] ~ t
      st[i + 20] = st[i + 20] ~ t
    end
    -- rho + pi
    local t = st[1]
    for i = 1, 24 do
      local j = PILN[i]
      local tmp = st[j]
      st[j] = rotl64(t, ROTC[i])
      t = tmp
    end
    -- chi
    for j = 0, 20, 5 do
      local b0, b1, b2, b3, b4 = st[j], st[j + 1], st[j + 2], st[j + 3], st[j + 4]
      st[j] = b0 ~ (~b1 & b2)
      st[j + 1] = b1 ~ (~b2 & b3)
      st[j + 2] = b2 ~ (~b3 & b4)
      st[j + 3] = b3 ~ (~b4 & b0)
      st[j + 4] = b4 ~ (~b0 & b1)
    end
    -- iota
    st[0] = st[0] ~ RNDC[round]
  end
end

local KECCAK_RATE = 136 -- 1088-bit rate for 256-bit output

-- keccak256(...) hashes the concatenation of its string arguments and
-- returns the 32-byte digest as a raw string.
function M.keccak256(...)
  local data = table.concat({ ... })
  local st = {}
  for i = 0, 24 do st[i] = 0 end
  local rem = #data % KECCAK_RATE
  local pad
  if rem == KECCAK_RATE - 1 then
    pad = "\x81" -- 0x01 padding byte and 0x80 final byte coincide
  else
    pad = "\x01" .. ("\0"):rep(KECCAK_RATE - rem - 2) .. "\x80"
  end
  local msg = data .. pad
  for base = 1, #msg, KECCAK_RATE do
    for i = 0, 16 do
      -- "<i8" loads the lane as a signed integer: same 64 bits, and xor
      -- does not care about the sign.
      st[i] = st[i] ~ string.unpack("<i8", msg, base + i * 8)
    end
    keccakf(st)
  end
  return string.pack("<i8i8i8i8", st[0], st[1], st[2], st[3])
end

local keccak256 = M.keccak256

-- ======================================================================
-- home slot (spec §6.1)
-- ======================================================================

-- home(seed, key, capacity) = LE64(keccak256(seed .. key)[0..8]) mod capacity,
-- with the 8 bytes interpreted as an *unsigned* 64-bit integer. For a
-- power-of-two capacity the mod is a mask, exact for any value. For other
-- capacities the mod is computed byte-by-byte (Horner, most-significant
-- first), exact for any capacity < 2^55 — far beyond any drive that fits in
-- an address space.
function M.home(seed, key, capacity)
  local h = keccak256(seed, key)
  if capacity & (capacity - 1) == 0 then
    return string.unpack("<i8", h) & (capacity - 1)
  end
  local v = 0
  for i = 8, 1, -1 do
    v = (v * 256 + h:byte(i)) % capacity
  end
  return v
end

-- ======================================================================
-- Stores
--
-- A store is any object with
--   :read_at(off, len) -> string   read len bytes at drive offset off
--   :write_at(off, str)            write str at drive offset off
-- Offsets are relative to the drive's first byte. Out-of-bounds access is
-- a caller bug and raises.
-- ======================================================================

local PAGE = 4096

local MemStore = {}
MemStore.__index = MemStore

-- mem_store(length) is an in-memory drive image: a table of fixed-size
-- (4096-byte) pages, so writes splice one page instead of copying the whole
-- image. Suits a host holding a copied quiescent image, and tests.
function M.mem_store(length)
  if type(length) ~= "number" or length <= 0 or math.tointeger(length) == nil then
    error("mem_store: length must be a positive integer", 2)
  end
  length = math.tointeger(length)
  local zeros = ("\0"):rep(PAGE)
  local pages = {}
  local np = (length + PAGE - 1) // PAGE
  for i = 1, np do
    local sz = math.min(PAGE, length - (i - 1) * PAGE)
    pages[i] = sz == PAGE and zeros or ("\0"):rep(sz)
  end
  return setmetatable({ length = length, pages = pages }, MemStore)
end

local function mem_bounds(self, off, len)
  if off < 0 or len < 0 or off + len > self.length then
    error(("mem_store: access [%d,%d) outside drive of %d bytes")
      :format(off, off + len, self.length), 3)
  end
end

function MemStore:read_at(off, len)
  mem_bounds(self, off, len)
  if len == 0 then return "" end
  local parts = {}
  local pos, remaining = off, len
  while remaining > 0 do
    local pi = pos // PAGE
    local po = pos % PAGE
    local n = math.min(PAGE - po, remaining)
    parts[#parts + 1] = self.pages[pi + 1]:sub(po + 1, po + n)
    pos = pos + n
    remaining = remaining - n
  end
  return table.concat(parts)
end

function MemStore:write_at(off, str)
  mem_bounds(self, off, #str)
  local pos, si = off, 1
  while si <= #str do
    local pi = pos // PAGE
    local po = pos % PAGE
    local page = self.pages[pi + 1]
    local n = math.min(#page - po, #str - si + 1)
    self.pages[pi + 1] = page:sub(1, po) .. str:sub(si, si + n - 1) .. page:sub(po + n + 1)
    pos = pos + n
    si = si + n
  end
end

-- snapshot() returns the whole drive image as one string — e.g. for hashing.
function MemStore:snapshot()
  return table.concat(self.pages)
end

local FileStore = {}
FileStore.__index = FileStore

-- file_store(path) accesses the drive through a file — in the guest, the raw
-- device (/dev/pmemN). Returns store or nil, message.
function M.file_store(path)
  local f, err = io.open(path, "r+b")
  if not f then return nil, err end
  return setmetatable({ f = f, path = path }, FileStore)
end

function FileStore:read_at(off, len)
  if len == 0 then return "" end
  assert(self.f:seek("set", off))
  local s = self.f:read(len)
  if s == nil or #s ~= len then
    error(("file_store: short read at %d (wanted %d bytes)"):format(off, len), 2)
  end
  return s
end

function FileStore:write_at(off, str)
  assert(self.f:seek("set", off))
  assert(self.f:write(str))
end

-- sync() flushes Lua's buffered writes to the OS. NOTE (spec §10): on a
-- pmem-backed drive the guest MUST also get the bytes out of the kernel page
-- cache before finishing each input — fsync(2)/msync(2) — which pure Lua
-- cannot express: io flush only drains the C stdio buffer. A real guest
-- should follow sync() with os.execute("sync") (or fsync the device fd via
-- whatever facility its rootfs provides) so the drive bytes are current in
-- machine state at the yield.
function FileStore:sync()
  assert(self.f:flush())
end

function FileStore:close()
  self.f:close()
end

-- ======================================================================
-- Header (spec §5)
-- ======================================================================

local HEADER_SIZE = 4096
local MIN_REGISTRY_OFFSET = 0x100
local MAGIC = "ctsiacct"
local VERSION = 1

-- Header field byte offsets (0-based, as in the spec).
local OFF = {
  magic = 0x00,
  version = 0x08,
  flags = 0x0c,
  capacity = 0x10,
  table_offset = 0x18,
  load_limit = 0x20,
  live_count = 0x28,
  seed = 0x30,
  profile = 0x50,
  slot_size = 0x54,
  registry_offset = 0x58,
  registry_capacity = 0x60,
  token_count = 0x68,
  sparse_offset = 0x70,
  sparse_capacity = 0x78,
  sparse_load_limit = 0x80,
  sparse_live_count = 0x88,
  sparse_slot_size = 0x90,
}

local ZERO20 = ("\0"):rep(20)
local ZERO32 = ("\0"):rep(32)

local function is_all_zero(s)
  return not s:find("[^\0]")
end

-- Normalize a balance to exactly 32 bytes big-endian, or nil on overflow.
local function to32(v)
  if v == nil then return ZERO32 end
  local n = #v
  if n == 32 then return v end
  if n < 32 then return ("\0"):rep(32 - n) .. v end
  if v:sub(1, n - 32):find("[^\0]") then return nil end
  return v:sub(n - 31)
end

local function check_addr(addr)
  if type(addr) ~= "string" or #addr ~= 20 then
    error("address must be a 20-byte string", 3)
  end
  return addr ~= ZERO20
end

-- ----------------------------------------------------------------- config

-- The config table for format() — every field a deployment constant
-- (spec §5), snake_case:
--   drive_length, capacity, load_limit (0 -> 7/8 of capacity),
--   seed (32-byte string, default all-zero), profile, slot_size (0 -> 64),
--   table_offset (0 -> 4096), registry_offset (0 -> none, profile 0 only),
--   registry_capacity, sparse_offset, sparse_capacity,
--   sparse_load_limit (0 -> 7/8), sparse_slot_size (0 -> 64).

local function apply_defaults(c)
  c.drive_length = c.drive_length or 0
  c.capacity = c.capacity or 0
  c.load_limit = c.load_limit or 0
  c.profile = c.profile or 0
  c.slot_size = c.slot_size or 0
  c.table_offset = c.table_offset or 0
  c.registry_offset = c.registry_offset or 0
  c.registry_capacity = c.registry_capacity or 0
  c.sparse_offset = c.sparse_offset or 0
  c.sparse_capacity = c.sparse_capacity or 0
  c.sparse_load_limit = c.sparse_load_limit or 0
  c.sparse_slot_size = c.sparse_slot_size or 0
  c.seed = c.seed or ZERO32
  if type(c.seed) ~= "string" or #c.seed ~= 32 then
    error("seed must be a 32-byte string", 3)
  end
  if c.slot_size == 0 then c.slot_size = 64 end
  if c.table_offset == 0 then c.table_offset = HEADER_SIZE end
  if c.load_limit == 0 and c.capacity > 0 then
    c.load_limit = c.capacity - c.capacity // 8
  end
  if c.profile == M.PROFILE_SPARSE then
    if c.sparse_slot_size == 0 then c.sparse_slot_size = 64 end
    if c.sparse_load_limit == 0 and c.sparse_capacity > 0 then
      c.sparse_load_limit = c.sparse_capacity - c.sparse_capacity // 8
    end
  end
end

local function validate(c)
  local dl = c.drive_length
  if dl == 0 or (dl & (dl - 1)) ~= 0 then
    return nil, ("drive length %d is not a power of two"):format(dl)
  end
  if c.profile < 0 or c.profile > M.PROFILE_SPARSE then
    return nil, ("unknown profile %d"):format(c.profile)
  end
  local ss = c.slot_size
  if c.profile == M.PROFILE_WIDE then
    if ss < 128 or (ss & (ss - 1)) ~= 0 then
      return nil, ("wide slot size %d: want a power of two >= 128"):format(ss)
    end
  elseif c.profile == M.PROFILE_SINGLE_ASSET then
    if ss ~= 64 and ss ~= 32 then
      return nil, ("slot size %d: profile 0 takes 64 (standard record) or 32 (compact record)"):format(ss)
    end
  elseif ss ~= 64 then
    return nil, ("slot size %d: must be 64 in the sparse profile"):format(ss)
  end
  if c.capacity < 2 then
    return nil, ("capacity %d < 2"):format(c.capacity)
  end
  if c.load_limit < 1 or c.load_limit >= c.capacity then
    return nil, ("load limit %d outside [1, capacity-1]"):format(c.load_limit)
  end
  if c.table_offset < HEADER_SIZE or c.table_offset % ss ~= 0 then
    return nil, ("table offset %d: want >= %d and a multiple of the slot size")
      :format(c.table_offset, HEADER_SIZE)
  end
  local regions = {
    { name = "header", off = 0, len = HEADER_SIZE },
    { name = "accounts table", off = c.table_offset, len = c.capacity * ss },
  }
  if c.profile ~= M.PROFILE_SINGLE_ASSET and c.registry_offset == 0 then
    return nil, ("profile %d requires a token registry"):format(c.profile)
  end
  if c.registry_offset ~= 0 then
    if c.registry_capacity == 0 or c.registry_capacity > 65536 then
      return nil, ("registry capacity %d outside [1, 65536]"):format(c.registry_capacity)
    end
    if c.registry_offset % 32 ~= 0 then
      return nil, ("registry offset %d not a multiple of 32"):format(c.registry_offset)
    end
    local e = c.registry_offset + 32 * c.registry_capacity
    if c.registry_offset < HEADER_SIZE then -- in-header placement (spec §5)
      if c.registry_offset < MIN_REGISTRY_OFFSET or e > HEADER_SIZE then
        return nil, ("in-header registry [0x%x,0x%x) outside [0x%x,0x%x)")
          :format(c.registry_offset, e, MIN_REGISTRY_OFFSET, HEADER_SIZE)
      end
    else
      regions[#regions + 1] = { name = "registry", off = c.registry_offset, len = 32 * c.registry_capacity }
    end
  end
  if c.profile == M.PROFILE_SPARSE then
    local sss = c.sparse_slot_size
    if sss ~= 64 and sss ~= 32 then
      return nil, ("sparse slot size %d: want 64 or 32"):format(sss)
    end
    if c.sparse_capacity < 2 then
      return nil, ("sparse capacity %d < 2"):format(c.sparse_capacity)
    end
    if c.sparse_load_limit < 1 or c.sparse_load_limit >= c.sparse_capacity then
      return nil, ("sparse load limit %d outside [1, capacity-1]"):format(c.sparse_load_limit)
    end
    if c.sparse_offset < HEADER_SIZE or c.sparse_offset % sss ~= 0 then
      return nil, ("sparse offset %d: want >= %d and a multiple of the slot size")
        :format(c.sparse_offset, HEADER_SIZE)
    end
    regions[#regions + 1] = { name = "sparse table", off = c.sparse_offset, len = c.sparse_capacity * sss }
  end
  for _, r in ipairs(regions) do
    if r.off + r.len > dl then
      return nil, ("%s [%d,%d) outside drive of %d bytes"):format(r.name, r.off, r.off + r.len, dl)
    end
  end
  for i = 1, #regions do
    for j = i + 1, #regions do
      local a, b = regions[i], regions[j]
      if a.off < b.off + b.len and b.off < a.off + a.len then
        return nil, ("%s overlaps %s"):format(a.name, b.name)
      end
    end
  end
  return true
end

-- ======================================================================
-- Drive
-- ======================================================================

local Drive = {}
Drive.__index = Drive

-- format(store, config) writes a v1 header (and nothing else) describing
-- config onto a store that must be all-zero, then opens it. Returns
-- drive or nil, message.
function M.format(store, config)
  local c = {}
  for k, v in pairs(config) do c[k] = v end
  apply_defaults(c)
  local ok, msg = validate(c)
  if not ok then return nil, msg end
  local so, sc, sl, sss = 0, 0, 0, 0
  if c.profile == M.PROFILE_SPARSE then
    so, sc, sl, sss = c.sparse_offset, c.sparse_capacity, c.sparse_load_limit, c.sparse_slot_size
  end
  local h = string.pack("<c8I4I4I8I8I8I8", MAGIC, VERSION, 0,
      c.capacity, c.table_offset, c.load_limit, 0) -- liveCount = 0
    .. c.seed
    .. string.pack("<I4I4I8I8I8", c.profile, c.slot_size,
      c.registry_offset, c.registry_capacity, 0)   -- tokenCount = 0
    .. string.pack("<I8I8I8I8I4", so, sc, sl, 0, sss) -- sparseLiveCount = 0
  h = h .. ("\0"):rep(HEADER_SIZE - #h)
  store:write_at(0, h)
  return M.open(store)
end

-- open(store) reads and validates the header, caches the geometry (immutable
-- by spec) and the registry. Returns drive or nil, message.
function M.open(store)
  local h = store:read_at(0, HEADER_SIZE)
  if h:sub(1, 8) ~= MAGIC then
    return nil, "not an accounts drive (bad magic)"
  end
  local version = string.unpack("<I4", h, OFF.version + 1)
  if version ~= VERSION then
    return nil, ("unsupported accounts drive version %d"):format(version)
  end
  local flags = string.unpack("<I4", h, OFF.flags + 1)
  if flags ~= 0 then
    return nil, ("unsupported accounts drive: nonzero flags 0x%x"):format(flags)
  end
  local function u64(o) return string.unpack("<i8", h, o + 1) end
  local function u32(o) return string.unpack("<I4", h, o + 1) end
  local c = {
    capacity = u64(OFF.capacity),
    table_offset = u64(OFF.table_offset),
    load_limit = u64(OFF.load_limit),
    seed = h:sub(OFF.seed + 1, OFF.seed + 32),
    profile = u32(OFF.profile),
    slot_size = u32(OFF.slot_size),
    registry_offset = u64(OFF.registry_offset),
    registry_capacity = u64(OFF.registry_capacity),
    sparse_offset = u64(OFF.sparse_offset),
    sparse_capacity = u64(OFF.sparse_capacity),
    sparse_load_limit = u64(OFF.sparse_load_limit),
    sparse_slot_size = u32(OFF.sparse_slot_size),
  }
  if c.slot_size == 0 then c.slot_size = 64 end
  local d = setmetatable({ store = store, cfg = c, _tokens = {}, _col_off = {} }, Drive)
  local ok, msg = d:reload_registry()
  if not ok then return nil, msg end
  return d
end

-- config() returns the drive's cached geometry. Treat it as read-only.
function Drive:config()
  return self.cfg
end

-- ------------------------------------------------------------- counters

function Drive:_counter(off)
  return string.unpack("<i8", self.store:read_at(off, 8))
end

function Drive:_set_counter(off, v)
  self.store:write_at(off, string.pack("<i8", v))
end

-- live_count() returns the accounts table's occupied-slot count.
function Drive:live_count() return self:_counter(OFF.live_count) end

-- sparse_live_count() returns the sparse table's occupied-slot count.
function Drive:sparse_live_count() return self:_counter(OFF.sparse_live_count) end

-- token_count() returns the number of registered tokens (from the drive).
function Drive:token_count() return self:_counter(OFF.token_count) end

-- ------------------------------------------------------------- registry

-- reload_registry() re-reads the registry and the wide column offsets.
-- Hosts watching a drive someone else writes call this when the token
-- count moves. Returns true or nil, message.
function Drive:reload_registry()
  self._tokens, self._col_off = {}, {}
  if self.cfg.registry_offset == 0 then return true end
  local n = self:_counter(OFF.token_count)
  if n > self.cfg.registry_capacity then
    return nil, ("token count %d exceeds registry capacity %d")
      :format(n, self.cfg.registry_capacity)
  end
  local col = 64
  for i = 0, n - 1 do
    local e = self.store:read_at(self.cfg.registry_offset + 32 * i, 32)
    local w = e:byte(21)
    if w ~= 8 and w ~= 16 and w ~= 32 then
      return nil, ("registry entry %d: bad width %d"):format(i, w)
    end
    self._tokens[i + 1] = { address = e:sub(1, 20), width = w }
    self._col_off[i + 1] = col
    col = col + w
  end
  return true
end

-- tokens() returns the cached registry in id order: an array of
-- { address = <20-byte string>, width = 8|16|32 }. Treat it as read-only.
function Drive:tokens()
  return self._tokens
end

-- token_by_address(addr) resolves a token address to id, token — or nil.
function Drive:token_by_address(addr)
  for i, t in ipairs(self._tokens) do
    if t.address == addr then return i - 1, t end
  end
  return nil
end

-- register_token(addr, width) appends a token to the registry and returns
-- its id (0-based). In the wide profile the new column must fit the slot
-- (spec §7); everywhere the registry must have room (spec §8). Note the
-- zero address is a legal token address here: profile 0 declares the
-- native asset as registry entry 0 with the zero address.
function Drive:register_token(addr, width)
  if type(addr) ~= "string" or #addr ~= 20 then
    error("token address must be a 20-byte string", 2)
  end
  if self.cfg.registry_offset == 0 then return nil, "profile" end
  if width ~= 8 and width ~= 16 and width ~= 32 then return nil, "badWidth" end
  if self:token_by_address(addr) then return nil, "duplicateToken" end
  local n = #self._tokens
  if n >= self.cfg.registry_capacity then return nil, "registryFull" end
  if self.cfg.profile == M.PROFILE_WIDE then
    local sum = 64
    for _, t in ipairs(self._tokens) do sum = sum + t.width end
    if sum + width > self.cfg.slot_size then
      -- columns would need more bytes than the slot has: the wide budget is
      -- slot_size - 64 (spec §7). Same kind as a full table: reject input.
      return nil, "tableFull"
    end
  end
  if self.cfg.profile == M.PROFILE_SPARSE and self.cfg.sparse_slot_size == 32
      and width ~= 8 then
    return nil, "badWidth" -- 32-byte sparse slots require width 8 (spec §9)
  end
  if self:compact() and width ~= 8 then
    return nil, "badWidth" -- the compact account record requires width 8 (spec §7)
  end
  local e = addr .. string.char(width) .. ("\0"):rep(11)
  self.store:write_at(self.cfg.registry_offset + 32 * n, e)
  self:_set_counter(OFF.token_count, n + 1)
  local col = 64
  if n > 0 then col = self._col_off[n] + self._tokens[n].width end
  self._tokens[n + 1] = { address = addr, width = width }
  self._col_off[n + 1] = col
  return n
end

-- ------------------------------------------------- tables (spec §6)
--
-- Both hash tables use the same open-addressing scheme with robin-hood
-- linear probing; the probing code is written once against this
-- description. key(slot) extracts the record's key; a slot is live iff the
-- 20 address bytes at live_off are nonzero.

local function accounts_table(d)
  return {
    base = d.cfg.table_offset,
    capacity = d.cfg.capacity,
    slot_size = d.cfg.slot_size,
    count_off = OFF.live_count,
    limit = d.cfg.load_limit,
    key = function(s) return s:sub(1, 20) end,
    live_off = 0,
  }
end

local function sparse_table(d)
  return {
    base = d.cfg.sparse_offset,
    capacity = d.cfg.sparse_capacity,
    slot_size = d.cfg.sparse_slot_size,
    count_off = OFF.sparse_live_count,
    limit = d.cfg.sparse_load_limit,
    -- 2-byte LE token id followed by the 20-byte address (spec §6.1).
    key = function(s) return s:sub(1, 2) .. s:sub(5, 24) end,
    live_off = 4,
  }
end

local function read_slot(d, t, i)
  return d.store:read_at(t.base + t.slot_size * i, t.slot_size)
end

local function write_slot(d, t, i, s)
  d.store:write_at(t.base + t.slot_size * i, s)
end

-- A slot is live iff its record's address field is nonzero (the zero
-- address is invalid in every record type, spec §6).
local function live(t, s)
  return s:sub(t.live_off + 1, t.live_off + 20):find("[^\0]") ~= nil
end

local function displacement(d, t, s, at)
  return (at + t.capacity - M.home(d.cfg.seed, t.key(s), t.capacity)) % t.capacity
end

-- lookup implements spec §6.2: returns slot index, slot bytes when found;
-- nil when the key is absent.
local function lookup(d, t, key)
  local h = M.home(d.cfg.seed, key, t.capacity)
  for dist = 0, t.capacity - 1 do
    local i = (h + dist) % t.capacity
    local slot = read_slot(d, t, i)
    if not live(t, slot) then return nil end
    if t.key(slot) == key then return i, slot end
    if displacement(d, t, slot, i) < dist then return nil end
  end
  return nil -- unreachable while load limit < capacity
end

-- insert implements spec §6.3. The caller has checked the key is absent and
-- the load limit has room; insert bumps the live counter itself. The strict
-- `<` swap rule and the carry displacement reset are normative — MUST NOT
-- vary.
local function insert(d, t, rec)
  local carry = rec
  local h = M.home(d.cfg.seed, t.key(carry), t.capacity)
  local dist = 0
  while true do
    local i = (h + dist) % t.capacity
    local slot = read_slot(d, t, i)
    if not live(t, slot) then
      write_slot(d, t, i, carry)
      d:_set_counter(t.count_off, d:_counter(t.count_off) + 1)
      return
    end
    local disp = displacement(d, t, slot, i)
    if disp < dist then
      write_slot(d, t, i, carry)
      carry = slot
      h = M.home(d.cfg.seed, t.key(carry), t.capacity)
      dist = disp
    end
    dist = dist + 1
  end
end

-- remove implements spec §6.4 (backward shift) from a known slot index.
-- No tombstones: after deletion, empty slots are all-zero.
local function remove(d, t, i)
  while true do
    local j = (i + 1) % t.capacity
    local nxt = read_slot(d, t, j)
    if not live(t, nxt) or displacement(d, t, nxt, j) == 0 then
      write_slot(d, t, i, ("\0"):rep(t.slot_size))
      d:_set_counter(t.count_off, d:_counter(t.count_off) - 1)
      return
    end
    write_slot(d, t, i, nxt)
    i = j
  end
end

-- ------------------------------------------------------------- accounts

-- compact() reports whether the drive uses the 32-byte profile-0 record
-- (spec §7): profile 0 with slot_size 32.
function Drive:compact()
  return self.cfg.profile == M.PROFILE_SINGLE_ASSET and self.cfg.slot_size == 32
end

-- decode_account reads nonce and balance out of a live account slot. The
-- balance always comes back as 32 bytes big-endian, zero-extended from the
-- compact record's uint64.
local function decode_account(d, slot)
  if d:compact() then
    -- addr 1..20, nonce uint32 LE 21..24, balance uint64 BE 25..32.
    return string.unpack("<I4", slot, 21), ("\0"):rep(24) .. slot:sub(25, 32)
  end
  return string.unpack("<i8", slot, 25), slot:sub(33, 64)
end

-- encode_account builds a record from a 32-byte-normalized balance. In the
-- compact record a nonce past uint32 or a balance past uint64 is "overflow"
-- — the input must be rejected (spec §7). Returns record or nil, kind.
local function encode_account(d, addr, nonce, bal)
  if d:compact() then
    -- nonce is a signed Lua integer; a value >= 2^63 appears negative and
    -- its unsigned interpretation exceeds uint32 too, so nonce < 0 is the
    -- unsigned comparison's high half.
    if nonce < 0 or nonce > 0xffffffff then return nil, "overflow" end
    if bal:sub(1, 24):find("[^\0]") then return nil, "overflow" end
    return addr .. string.pack("<I4", nonce) .. bal:sub(25, 32)
  end
  return addr .. "\0\0\0\0" .. string.pack("<i8", nonce) .. bal
end

-- get_account(addr) returns { address, nonce, balance } when the address has
-- a record, nil when it has none (absence is a result, not an error — the
-- second value is nil), or nil, kind on error. balance is 32 bytes
-- big-endian; nonce is a Lua integer (see the signedness note up top; in the
-- compact record it is a uint32 and always non-negative).
function Drive:get_account(addr)
  if not check_addr(addr) then return nil, "zeroAddress" end
  local t = accounts_table(self)
  local i, slot = lookup(self, t, addr)
  if not i then return nil end
  local nonce, balance = decode_account(self, slot)
  return { address = addr, nonce = nonce, balance = balance }
end

-- set_account(addr, nonce, balance) writes an account's nonce and balance,
-- inserting the record if the address has none. "tableFull" — and, in the
-- compact record, "overflow" — means the input carrying this change must be
-- rejected (spec §6.3, §7). Returns true or nil, kind.
function Drive:set_account(addr, nonce, balance)
  if not check_addr(addr) then return nil, "zeroAddress" end
  local bal = to32(balance)
  if not bal then return nil, "overflow" end
  local rec, e = encode_account(self, addr, nonce or 0, bal)
  if not rec then return nil, e end
  local t = accounts_table(self)
  local i, slot = lookup(self, t, addr)
  if i then
    -- Update in place, touching only the record bytes: in the wide
    -- profile the rest of the slot is token columns.
    write_slot(self, t, i, rec .. slot:sub(#rec + 1))
    return true
  end
  if self:_counter(t.count_off) >= t.limit then return nil, "tableFull" end
  insert(self, t, rec .. ("\0"):rep(t.slot_size - #rec))
  return true
end

-- delete_account(addr, force) removes an account's record. A record with a
-- nonzero nonce is refused unless force is true: deleting it re-enables
-- replay of that account's past transactions (spec §7). In the wide profile
-- the record's token columns go with it. Returns true (deleted), false (no
-- record), or nil, kind.
function Drive:delete_account(addr, force)
  if not check_addr(addr) then return nil, "zeroAddress" end
  local t = accounts_table(self)
  local i, slot = lookup(self, t, addr)
  if not i then return false end
  local nonce = decode_account(self, slot)
  if not force and nonce ~= 0 then
    return nil, "nonceProtected"
  end
  remove(self, t, i)
  return true
end

-- ------------------------------------------------------- token balances

local function sparse_key(id, addr)
  return string.pack("<I2", id) .. addr
end

local function put_sparse_balance(d, slot, v32)
  if d.cfg.sparse_slot_size == 32 then
    return slot:sub(1, 24) .. v32:sub(25, 32)
  end
  return slot:sub(1, 32) .. v32
end

-- get_token_balance(addr, id) reads an account's balance of a registered
-- token. An absent record (or column of an absent account) is zero, not an
-- error. Returns a 32-byte big-endian string or nil, kind.
function Drive:get_token_balance(addr, id)
  if not check_addr(addr) then return nil, "zeroAddress" end
  local tok = self._tokens[(id or -1) + 1]
  if not tok then return nil, "unknownToken" end
  local p = self.cfg.profile
  if p == M.PROFILE_WIDE then
    local t = accounts_table(self)
    local _, slot = lookup(self, t, addr)
    if not slot then return ZERO32 end
    local off = self._col_off[id + 1]
    return ("\0"):rep(32 - tok.width) .. slot:sub(off + 1, off + tok.width)
  elseif p == M.PROFILE_SPARSE then
    local t = sparse_table(self)
    local _, slot = lookup(self, t, sparse_key(id, addr))
    if not slot then return ZERO32 end
    if self.cfg.sparse_slot_size == 32 then
      return ("\0"):rep(24) .. slot:sub(25, 32)
    end
    return slot:sub(33, 64)
  end
  return nil, "profile"
end

-- set_token_balance(addr, id, value) writes an account's balance of a
-- registered token. "overflow" (value exceeds the token's declared width)
-- and "tableFull" both mean rejecting the input. In the wide profile a
-- missing account record is auto-created with a zero nonce and native
-- balance, and writing zero keeps the record; in the sparse profile a
-- balance reaching zero deletes its record (spec §9). Returns true or
-- nil, kind.
function Drive:set_token_balance(addr, id, value)
  if not check_addr(addr) then return nil, "zeroAddress" end
  local tok = self._tokens[(id or -1) + 1]
  if not tok then return nil, "unknownToken" end
  local v32 = to32(value)
  if not v32 or (tok.width < 32 and v32:sub(1, 32 - tok.width):find("[^\0]")) then
    return nil, "overflow"
  end
  local zero = is_all_zero(v32)
  local p = self.cfg.profile
  if p == M.PROFILE_WIDE then
    local t = accounts_table(self)
    local off = self._col_off[id + 1]
    local col = v32:sub(33 - tok.width) -- low tok.width bytes, big-endian
    local i, slot = lookup(self, t, addr)
    if not i then
      if zero then return true end
      if self:_counter(t.count_off) >= t.limit then return nil, "tableFull" end
      local full = addr .. ("\0"):rep(off - 20) .. col
      insert(self, t, full .. ("\0"):rep(t.slot_size - #full))
      return true
    end
    write_slot(self, t, i, slot:sub(1, off) .. col .. slot:sub(off + tok.width + 1))
    return true
  elseif p == M.PROFILE_SPARSE then
    local t = sparse_table(self)
    local key = sparse_key(id, addr)
    local i, slot = lookup(self, t, key)
    if zero then
      if not i then return true end
      remove(self, t, i) -- a balance reaching zero deletes the record
      return true
    end
    if i then
      write_slot(self, t, i, put_sparse_balance(self, slot, v32))
      return true
    end
    if self:_counter(t.count_off) >= t.limit then return nil, "tableFull" end
    local rec
    if self.cfg.sparse_slot_size == 32 then
      rec = string.pack("<I2", id) .. "\0\0" .. addr .. v32:sub(25, 32)
    else
      rec = string.pack("<I2", id) .. "\0\0" .. addr .. ("\0"):rep(8) .. v32
    end
    insert(self, t, rec)
    return true
  end
  return nil, "profile"
end

return M
