#!/usr/bin/env lua5.4
-- test.lua — unit and conformance tests for accounts_drive.lua.
--
-- Run: lua5.4 test.lua   (from anywhere; paths resolve relative to the
-- script). Replays the spec's Appendix B vectors and the cross-language
-- golden vectors in ../testdata/golden.json, hashing every resulting drive
-- image with a pure-Lua SHA-256 defined below.

-- Resolve everything relative to this script so the test runs from any cwd.
local script_dir = (arg and arg[0] or "test.lua"):match("^(.*)[/\\][^/\\]*$") or "."
package.path = script_dir .. "/?.lua;" .. package.path
local ad = require("accounts_drive")

local GOLDEN_PATH = script_dir .. "/../testdata/golden.json"

-- ---------------------------------------------------------------- helpers

local checks_run = 0

local function check(cond, msg, ...)
  checks_run = checks_run + 1
  if not cond then
    error("FAIL: " .. string.format(msg, ...), 2)
  end
end

local function bin2hex(s)
  return (s:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

local function hex2bin(s)
  return (s:gsub("%x%x", function(h) return string.char(tonumber(h, 16)) end))
end

local function rep_addr(b)
  return string.char(b):rep(20)
end

-- ---------------------------------------------------------------- SHA-256
-- Pure Lua 5.4, on native integers with 32-bit masking. Used to hash whole
-- drive images against the golden vectors' sha256 field.

local SHA_K = {
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
  0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

local function ror32(x, n)
  return ((x >> n) | (x << (32 - n))) & 0xffffffff
end

local function sha256_hex(msg)
  local len = #msg
  local padzeros = (64 - ((len + 9) % 64)) % 64
  msg = msg .. "\x80" .. ("\0"):rep(padzeros) .. string.pack(">I8", len * 8)
  local h0, h1, h2, h3 = 0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a
  local h4, h5, h6, h7 = 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19
  local w = {}
  for chunk = 1, #msg, 64 do
    w[1], w[2], w[3], w[4], w[5], w[6], w[7], w[8],
    w[9], w[10], w[11], w[12], w[13], w[14], w[15], w[16] =
      string.unpack(">I4I4I4I4I4I4I4I4I4I4I4I4I4I4I4I4", msg, chunk)
    for i = 17, 64 do
      local x, y = w[i - 15], w[i - 2]
      local s0 = ror32(x, 7) ~ ror32(x, 18) ~ (x >> 3)
      local s1 = ror32(y, 17) ~ ror32(y, 19) ~ (y >> 10)
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) & 0xffffffff
    end
    local a, b, c, d, e, f, g, h = h0, h1, h2, h3, h4, h5, h6, h7
    for i = 1, 64 do
      local S1 = ror32(e, 6) ~ ror32(e, 11) ~ ror32(e, 25)
      local ch = (e & f) ~ (~e & g)
      local t1 = (h + S1 + ch + SHA_K[i] + w[i]) & 0xffffffff
      local S0 = ror32(a, 2) ~ ror32(a, 13) ~ ror32(a, 22)
      local maj = (a & b) ~ (a & c) ~ (b & c)
      local t2 = (S0 + maj) & 0xffffffff
      h = g; g = f; f = e; e = (d + t1) & 0xffffffff
      d = c; c = b; b = a; a = (t1 + t2) & 0xffffffff
    end
    h0 = (h0 + a) & 0xffffffff; h1 = (h1 + b) & 0xffffffff
    h2 = (h2 + c) & 0xffffffff; h3 = (h3 + d) & 0xffffffff
    h4 = (h4 + e) & 0xffffffff; h5 = (h5 + f) & 0xffffffff
    h6 = (h6 + g) & 0xffffffff; h7 = (h7 + h) & 0xffffffff
  end
  return string.format("%08x%08x%08x%08x%08x%08x%08x%08x",
    h0, h1, h2, h3, h4, h5, h6, h7)
end

-- ------------------------------------------------------------ JSON parser
-- Minimal pure-Lua JSON decoder, sufficient for golden.json: objects,
-- arrays, strings (basic escapes and \uXXXX), numbers, booleans, null.
-- Numbers that are integral come back as Lua integers.

local function json_decode(s)
  local pos = 1

  local function fail(msg)
    error(("json: %s at byte %d"):format(msg, pos))
  end

  local function skip_ws()
    pos = s:find("[^ \t\r\n]", pos) or #s + 1
  end

  local ESC = {
    ['"'] = '"', ["\\"] = "\\", ["/"] = "/",
    b = "\b", f = "\f", n = "\n", r = "\r", t = "\t",
  }

  local function parse_string()
    local out, i = {}, pos + 1
    while true do
      local j = s:find('["\\]', i)
      if not j then fail("unterminated string") end
      out[#out + 1] = s:sub(i, j - 1)
      if s:sub(j, j) == '"' then
        pos = j + 1
        return table.concat(out)
      end
      local e = s:sub(j + 1, j + 1)
      if ESC[e] then
        out[#out + 1] = ESC[e]
        i = j + 2
      elseif e == "u" then
        local cp = tonumber(s:sub(j + 2, j + 5), 16) or fail("bad \\u escape")
        out[#out + 1] = utf8.char(cp)
        i = j + 6
      else
        fail("bad escape \\" .. e)
      end
    end
  end

  local function parse_number()
    local j = s:find("[^-+0-9.eE]", pos) or #s + 1
    local n = tonumber(s:sub(pos, j - 1))
    if not n then fail("bad number") end
    pos = j
    return math.tointeger(n) or n
  end

  local parse_value

  local function parse_array()
    pos = pos + 1 -- '['
    local out = {}
    skip_ws()
    if s:sub(pos, pos) == "]" then pos = pos + 1; return out end
    while true do
      out[#out + 1] = parse_value()
      skip_ws()
      local c = s:sub(pos, pos)
      pos = pos + 1
      if c == "]" then return out end
      if c ~= "," then fail("expected , or ] in array") end
      skip_ws()
    end
  end

  local function parse_object()
    pos = pos + 1 -- '{'
    local out = {}
    skip_ws()
    if s:sub(pos, pos) == "}" then pos = pos + 1; return out end
    while true do
      skip_ws()
      if s:sub(pos, pos) ~= '"' then fail("expected object key") end
      local k = parse_string()
      skip_ws()
      if s:sub(pos, pos) ~= ":" then fail("expected : after key") end
      pos = pos + 1
      skip_ws()
      out[k] = parse_value()
      skip_ws()
      local c = s:sub(pos, pos)
      pos = pos + 1
      if c == "}" then return out end
      if c ~= "," then fail("expected , or } in object") end
    end
  end

  parse_value = function()
    skip_ws()
    local c = s:sub(pos, pos)
    if c == '"' then return parse_string() end
    if c == "{" then return parse_object() end
    if c == "[" then return parse_array() end
    if c == "t" and s:sub(pos, pos + 3) == "true" then pos = pos + 4; return true end
    if c == "f" and s:sub(pos, pos + 4) == "false" then pos = pos + 5; return false end
    if c == "n" and s:sub(pos, pos + 3) == "null" then pos = pos + 4; return nil end
    if c == "-" or c:match("%d") then return parse_number() end
    fail("unexpected character " .. (c == "" and "<eof>" or c))
  end

  local v = parse_value()
  skip_ws()
  if pos <= #s then fail("trailing garbage") end
  return v
end

-- ============================================================ unit tests

local function test_keccak_vectors()
  check(bin2hex(ad.keccak256("")) ==
    "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470",
    'keccak256("") mismatch')
  local seed = ("\0"):rep(32)
  local bb = rep_addr(0xbb)
  check(bin2hex(ad.keccak256(seed, bb)) ==
    "693865af36f54054f08a9e83ec52e6df2001c39673abba1e01d450211dff1a92",
    "keccak256(seed .. bb..) mismatch")
  -- sparse key: token id 3 (uint16 LE) .. bb.., 22 bytes (spec Appendix B)
  local sparse_key = "\x03\x00" .. bb
  check(bin2hex(ad.keccak256(seed, sparse_key)) ==
    "e5cad18719cbc3bfeb1ff8587f27c3cfd5f9da3867df2634691bd24e26d147d3",
    "keccak256(seed .. sparse key) mismatch")
  -- Exercise the multi-block absorb path and the rate-1 padding edge case
  -- (135 bytes: the 0x01 and 0x80 padding bytes coincide in one byte).
  -- Values cross-checked against the Go reference implementation.
  check(bin2hex(ad.keccak256(("\xa3"):rep(200))) ==
    "3a57666b048777f2c953dc4456f45a2588e1cb6f2da760122d530ac2ce607d4a",
    "keccak256(0xa3 x200) mismatch")
  check(bin2hex(ad.keccak256(("\xa3"):rep(135))) ==
    "3d28d08c3dacab77392064a939f3e7f8d03f2e02e2c664ac08a05f63ac652626",
    "keccak256(0xa3 x135) mismatch")
  print("ok  keccak-256 vectors")
end

local function test_home_slots()
  local seed = ("\0"):rep(32)
  local want = { [0xaa] = 5, [0xbb] = 1, [0xcc] = 7, [0xdd] = 1, [0xee] = 1 }
  for b, w in pairs(want) do
    check(ad.home(seed, rep_addr(b), 8) == w,
      "home(%02x..) mod 8 != %d", b, w)
  end
  check(ad.home(seed, rep_addr(0xbb), 1 << 19) == 342121,
    "home(bb..) mod 2^19 != 342121")
  check(ad.home(seed, "\x03\x00" .. rep_addr(0xbb), 1 << 19) == 117477,
    "sparse home mod 2^19 != 117477")
  -- Non-power-of-two capacity takes the Horner path; cross-check it against
  -- the LE64 value of keccak256(seed .. bb..): 0x5440f536af653869 % 7 == 5.
  check(ad.home(seed, rep_addr(0xbb), 7) == 0x5440f536af653869 % 7,
    "home(bb..) mod 7 (Horner path) mismatch")
  print("ok  home slots")
end

local function test_account_record_encoding()
  local s = ad.mem_store(1 << 16)
  local d = assert(ad.format(s, { drive_length = 1 << 16, capacity = 8 }))
  assert(d:set_account(rep_addr(0xbb), 7, "\x05"))
  -- home(bb..) mod 8 = 1
  local got = s:read_at(4096 + 64 * 1, 64)
  local want = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb000000000700000000000000"
    .. "0000000000000000000000000000000000000000000000000000000000000005"
  check(bin2hex(got) == want, "account record encoding:\n got %s\nwant %s",
    bin2hex(got), want)
  local a = assert(d:get_account(rep_addr(0xbb)))
  check(a.nonce == 7 and bin2hex(a.balance) ==
    "0000000000000000000000000000000000000000000000000000000000000005",
    "account record decode mismatch")
  print("ok  account record encoding")
end

local function test_compact_record_encoding()
  -- The 32-byte profile-0 record (spec §7, Appendix B): encoding, the uint32
  -- nonce and uint64 balance bounds, and the registry width rule.
  local s = ad.mem_store(1 << 16)
  local d = assert(ad.format(s, {
    drive_length = 1 << 16, capacity = 8, slot_size = 32,
    registry_offset = 0x100, registry_capacity = 2,
  }))
  assert(d:set_account(rep_addr(0xbb), 7, "\x05"))
  -- home(bb..) mod 8 = 1
  local got = s:read_at(4096 + 32 * 1, 32)
  local want = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb070000000000000000000005"
  check(bin2hex(got) == want, "compact record encoding:\n got %s\nwant %s",
    bin2hex(got), want)
  local a = assert(d:get_account(rep_addr(0xbb)))
  check(a.nonce == 7 and bin2hex(a.balance) ==
    "0000000000000000000000000000000000000000000000000000000000000005",
    "compact record decode mismatch")
  -- nonce past uint32 (2^32) must overflow
  local _, e = d:set_account(rep_addr(0xbb), 1 << 32, "\x05")
  check(e == "overflow", "compact nonce past uint32 kind %s", tostring(e))
  -- balance past uint64 (2^64: a nonzero byte above the low 8) must overflow
  _, e = d:set_account(rep_addr(0xbb), 7, "\x01" .. ("\0"):rep(8))
  check(e == "overflow", "compact balance past uint64 kind %s", tostring(e))
  -- the registry, if declared, only takes width 8 (checked last, spec §7)
  _, e = d:register_token(rep_addr(1), 32)
  check(e == "badWidth", "compact registry width kind %s", tostring(e))
  check(d:register_token(("\0"):rep(20), 8) == 0, "compact width-8 registration")
  -- nonce protection reads the uint32
  _, e = d:delete_account(rep_addr(0xbb), false)
  check(e == "nonceProtected", "compact delete kind %s", tostring(e))
  -- 32-byte account slots outside profile 0 must not format
  local d2 = ad.format(ad.mem_store(1 << 16), {
    drive_length = 1 << 16, capacity = 8, slot_size = 32,
    profile = ad.PROFILE_SPARSE,
    registry_offset = 0x100, registry_capacity = 2,
    sparse_offset = 8192, sparse_capacity = 8,
  })
  check(d2 == nil, "32-byte account slots outside profile 0 must not format")
  print("ok  compact record encoding")
end

local function test_sparse32_record_encoding()
  local s = ad.mem_store(1 << 16)
  local d = assert(ad.format(s, {
    drive_length = 1 << 16, capacity = 8, profile = ad.PROFILE_SPARSE,
    registry_offset = 0x100, registry_capacity = 8,
    sparse_offset = 8192, sparse_capacity = 8, sparse_slot_size = 32,
  }))
  for i = 1, 4 do
    assert(d:register_token(rep_addr(i), 8))
  end
  assert(d:set_token_balance(rep_addr(0xbb), 3, string.pack(">I8", 1000000)))
  -- sparse home(id 3, bb..) mod 8: LE64(e5cad187..) = 0xbfc3cb1987d1cae5 -> 5
  local got = s:read_at(8192 + 32 * 5, 32)
  local want = "03000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000f4240"
  check(bin2hex(got) == want, "sparse32 record:\n got %s\nwant %s",
    bin2hex(got), want)
  print("ok  sparse 32-byte record encoding")
end

local function test_error_kinds()
  -- Profile 0 drive with a single-asset registry entry (spec §7: zero
  -- address stands for the native asset).
  local s = ad.mem_store(1 << 16)
  local d = assert(ad.format(s, {
    drive_length = 1 << 16, capacity = 8, load_limit = 2,
    registry_offset = 0x100, registry_capacity = 1,
  }))
  local zero20 = ("\0"):rep(20)
  local _, e = d:get_account(zero20)
  check(e == "zeroAddress", "get_account(zero) kind %s", tostring(e))
  _, e = d:set_account(zero20, 0, "\1")
  check(e == "zeroAddress", "set_account(zero) kind %s", tostring(e))
  check(d:register_token(zero20, 32) == 0, "single-asset registry entry 0")
  _, e = d:register_token(rep_addr(1), 8)
  check(e == "registryFull", "register beyond capacity kind %s", tostring(e))
  _, e = d:get_token_balance(rep_addr(1), 0)
  check(e == "profile", "token balance in profile 0 kind %s", tostring(e))
  _, e = d:set_token_balance(rep_addr(1), 0, "\1")
  check(e == "profile", "set token balance in profile 0 kind %s", tostring(e))
  _, e = d:get_token_balance(rep_addr(1), 1)
  check(e == "unknownToken", "unregistered id kind %s", tostring(e))
  assert(d:set_account(rep_addr(1), 1, "\1"))
  assert(d:set_account(rep_addr(2), 0, "\1"))
  _, e = d:set_account(rep_addr(3), 0, "\1")
  check(e == "tableFull", "insert at load limit kind %s", tostring(e))
  _, e = d:delete_account(rep_addr(1), false)
  check(e == "nonceProtected", "nonzero-nonce delete kind %s", tostring(e))
  check(d:delete_account(rep_addr(1), true) == true, "forced delete")
  check(d:delete_account(rep_addr(3), false) == false,
    "absent delete is a result, not an error")
  _, e = d:set_account(rep_addr(2), 0, "\1" .. ("\0"):rep(32))
  check(e == "overflow", ">32-byte nonzero balance kind %s", tostring(e))

  -- Registry kinds on a sparse drive without a registry op in golden.
  local s2 = ad.mem_store(1 << 16)
  local d2 = assert(ad.format(s2, {
    drive_length = 1 << 16, capacity = 8, profile = ad.PROFILE_SPARSE,
    registry_offset = 0x100, registry_capacity = 2,
    sparse_offset = 8192, sparse_capacity = 8,
  }))
  _, e = d2:register_token(rep_addr(1), 20)
  check(e == "badWidth", "width 20 kind %s", tostring(e))
  check(d2:register_token(rep_addr(1), 8) == 0, "first sparse token id")
  _, e = d2:register_token(rep_addr(1), 8)
  check(e == "duplicateToken", "duplicate register kind %s", tostring(e))
  check(d2:register_token(rep_addr(2), 32) == 1, "second sparse token id")
  _, e = d2:register_token(rep_addr(3), 8)
  check(e == "registryFull", "sparse register full kind %s", tostring(e))
  _, e = d2:set_token_balance(rep_addr(9), 0, ("\1"):rep(9))
  check(e == "overflow", "value beyond width 8 kind %s", tostring(e))
  -- Reopen: the registry cache is rebuilt from the drive bytes.
  local d3 = assert(ad.open(s2))
  check(#d3:tokens() == 2 and d3:token_count() == 2
    and d3:tokens()[1].width == 8 and d3:token_by_address(rep_addr(2)) == 1,
    "registry survives reopen")
  print("ok  error kinds")
end

local function test_file_store()
  local path = os.tmpname()
  local okwrap, err = pcall(function()
    local f = assert(io.open(path, "wb"))
    f:write(("\0"):rep(1 << 16))
    f:close()
    local fs = assert(ad.file_store(path))
    local d = assert(ad.format(fs, { drive_length = 1 << 16, capacity = 8 }))
    assert(d:set_account(rep_addr(0xbb), 7, "\x05"))
    fs:sync()
    fs:close()
    -- reopen and read back
    local fs2 = assert(ad.file_store(path))
    local d2 = assert(ad.open(fs2))
    local a = assert(d2:get_account(rep_addr(0xbb)))
    check(a.nonce == 7 and a.balance:byte(32) == 5, "file_store round trip")
    fs2:close()
  end)
  os.remove(path)
  if not okwrap then error(err, 0) end
  print("ok  file store round trip")
end

-- ========================================================== golden replay

local function run_op(d, op)
  local kind
  if op.op == "setAccount" then
    local _, e = d:set_account(hex2bin(op.addr), op.nonce or 0,
      hex2bin(op.value or ""))
    kind = e or "ok"
  elseif op.op == "deleteAccount" then
    local _, e = d:delete_account(hex2bin(op.addr), op.force or false)
    kind = e or "ok"
  elseif op.op == "registerToken" then
    local _, e = d:register_token(hex2bin(op.token), op.width)
    kind = e or "ok"
  elseif op.op == "setTokenBalance" then
    local _, e = d:set_token_balance(hex2bin(op.addr), op.id or 0,
      hex2bin(op.value or ""))
    kind = e or "ok"
  else
    error(("unknown op %q"):format(tostring(op.op)))
  end
  return kind
end

local function run_check(d, store, cfg, c, name, idx)
  if c.check == "account" then
    local a, e = d:get_account(hex2bin(c.addr))
    check(not e, "%s check %d: get_account error %s", name, idx, tostring(e))
    local found = a ~= nil
    check(found == (c.found or false),
      "%s check %d (account %s): found=%s, want %s",
      name, idx, c.addr, tostring(found), tostring(c.found or false))
    if found then
      check(a.nonce == (c.nonce or 0),
        "%s check %d (account %s): nonce %d, want %d",
        name, idx, c.addr, a.nonce, c.nonce or 0)
      check(bin2hex(a.balance) == c.value,
        "%s check %d (account %s): balance %s, want %s",
        name, idx, c.addr, bin2hex(a.balance), c.value)
    end
  elseif c.check == "tokenBalance" then
    local v, e = d:get_token_balance(hex2bin(c.addr), c.id or 0)
    check(v ~= nil, "%s check %d: get_token_balance error %s", name, idx, tostring(e))
    check(bin2hex(v) == c.value,
      "%s check %d (token %d of %s): %s, want %s",
      name, idx, c.id or 0, c.addr, bin2hex(v), c.value)
  elseif c.check == "liveCount" then
    local n
    if c.table == "sparse" then n = d:sparse_live_count() else n = d:live_count() end
    check(n == (c.count or 0), "%s check %d: %s liveCount %d, want %d",
      name, idx, tostring(c.table), n, c.count or 0)
  elseif c.check == "slot" then
    local off, len
    if c.table == "sparse" then
      off = cfg.sparseOffset + cfg.sparseSlotSize * (c.index or 0)
      len = cfg.sparseSlotSize
    else
      off = cfg.tableOffset + cfg.slotSize * (c.index or 0)
      len = cfg.slotSize
    end
    local slot = store:read_at(off, len)
    check(bin2hex(slot) == c.hex, "%s check %d: slot %d = %s, want %s",
      name, idx, c.index or 0, bin2hex(slot), c.hex)
  else
    error(("%s check %d: unknown check %q"):format(name, idx, tostring(c.check)))
  end
end

local function replay_scenario(sc)
  local cfg = sc.config
  local store = ad.mem_store(cfg.driveLength)
  local d, err = ad.format(store, {
    drive_length = cfg.driveLength,
    capacity = cfg.capacity,
    load_limit = cfg.loadLimit,
    seed = hex2bin(cfg.seed),
    profile = cfg.profile,
    slot_size = cfg.slotSize,
    table_offset = cfg.tableOffset,
    registry_offset = cfg.registryOffset,
    registry_capacity = cfg.registryCapacity,
    sparse_offset = cfg.sparseOffset,
    sparse_capacity = cfg.sparseCapacity,
    sparse_load_limit = cfg.sparseLoadLimit,
    sparse_slot_size = cfg.sparseSlotSize,
  })
  check(d ~= nil, "%s: format failed: %s", sc.name, tostring(err))
  for i, op in ipairs(sc.ops) do
    local kind = run_op(d, op)
    check(kind == op.expect, "%s op %d (%s): outcome %q, want %q",
      sc.name, i, op.op, kind, op.expect)
  end
  for i, c in ipairs(sc.checks) do
    run_check(d, store, cfg, c, sc.name, i)
  end
  local img = store:snapshot()
  check(#img == cfg.driveLength, "%s: snapshot length %d != driveLength %d",
    sc.name, #img, cfg.driveLength)
  local sum = sha256_hex(img)
  check(sum == sc.sha256, "%s: drive sha256 = %s, want %s", sc.name, sum, sc.sha256)
  print(("ok  golden %-12s (%d ops, %d checks, sha256 %s...)")
    :format(sc.name, #sc.ops, #sc.checks, sum:sub(1, 12)))
end

local function test_golden()
  local f = io.open(GOLDEN_PATH, "rb")
  check(f ~= nil, "cannot open golden vectors at %s", GOLDEN_PATH)
  local data = f:read("a")
  f:close()
  local golden = json_decode(data)
  check(type(golden) == "table" and type(golden.scenarios) == "table"
    and #golden.scenarios > 0, "golden file has no scenarios")
  for _, sc in ipairs(golden.scenarios) do
    replay_scenario(sc)
  end
  return #golden.scenarios
end

-- =================================================================== main

test_keccak_vectors()
test_home_slots()
test_account_record_encoding()
test_compact_record_encoding()
test_sparse32_record_encoding()
test_error_kinds()
test_file_store()
local nscenarios = test_golden()

print(("PASS: all tests passed (%d golden scenarios, %d assertions)")
  :format(nscenarios, checks_run))
