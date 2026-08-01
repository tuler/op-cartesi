#!/bin/sh
# Bank app: the guest side of a deposit.
#
# It decodes each input as an Ethereum transaction and credits assets to a
# ledger that lives entirely inside the machine, so the balances are part of
# the state the machine's Merkle root commits to — which is the whole point: a
# deposit is credited by the execution layer, not by the shim around it.
#
# The ledger lives on a dedicated *accounts drive* (docs/ACCOUNTS-DRIVE-SPEC.md,
# maintained through the pure-Lua library in accounts-drive/lua): a profile-2
# (sparse) drive whose account records carry the ether balance and whose sparse
# table carries one record per nonzero ERC-20 holding. Because the drive layout
# is a published standard, the host reads balances and nonces straight out of
# machine memory (eth_getBalance, eth_getTransactionCount) with one
# machine.read_memory each — no inspect fork. See docs/ACCOUNTS.md §10, v0.
#
# Since roadmap v1 the drive's nonces are also *enforced* here: an ordinary L2
# transaction (anything that is not an OP deposit) must be a signed legacy
# Ethereum transaction whose recovered sender's account nonce matches, or the
# input is rejected. Acceptance bumps the nonce — this guest is the only
# writer of nonces — and debits a flat per-transaction fee (owner-settable,
# zero at genesis; see the fee section below and ACCOUNTS.md §5.7). Deposits
# are exempt: they are authenticated by their L1 origin, not a signature, and
# keep the paths described next.
#
# Three ways in, and the difference between them is who holds the asset on L1:
#
#   OptimismPortal.depositTransaction with a value   ether, held by OP's lockbox
#   OPEtherPortal.depositEther                       ether, held by the app
#   OPERC20Portal.depositERC20Tokens                 tokens, held by the app
#
# Only the last two are backed by something a voucher can move, since a voucher
# runs from the application contract. The first is OP's own path and its ether
# sits where only an OptimismPortal withdrawal proof could reach it — which on
# this chain nothing can produce. See DESIGN §7d.
#
# Balances are readable through eth_call, which the shim answers with the
# machine's inspect protocol: call the chain with a 20-byte address and the
# reply is that address's ether balance, or with token ‖ address for a token.
#
# Withdrawing emits a Cartesi *voucher*: a single-use permission to make one
# call from the application contract's context on L1. That is this chain's
# whole withdrawal path. The voucher enters the outputs tree, the tree root is
# the block's withdrawalsRoot, an op-proposer proposal on L1 commits to it, and
# an L1 contract proves the voucher against that proposal and runs the call.
# Nothing goes through OptimismPortal's withdrawal path, which wants an MPT
# proof of a storage slot against a state root that here is a Cartesi hash
# tree. See DESIGN §4.
#
# Written in Lua because the guest-tools rootfs ships lua5.4, and RLP wants
# real arithmetic rather than shell.

# Sender recovery is its own module, written next to the app the same way
# the accounts-drive library is: as guest bytes in the snapshot, pinned by
# the genesis state root. A separate file (rather than inlining it into
# bank.lua) lets devnet/test-guest.lua load and pin the ecrecover in
# isolation, against vectors generated with go-ethereum.
cat > /var/lib/secp256k1.lua <<'SECPEOF'
-- secp256k1.lua — ECDSA public-key recovery (ecrecover) in pure Lua 5.4.
--
-- The one consumer is the guest's transaction-sender recovery (bank.lua):
-- given a message hash and an Ethereum-style signature (r, s, recovery id),
-- return the signer's 20-byte address. Standard recovery: lift R from r
-- using the recovery id's parity, then Q = r⁻¹(sR − zG), and the address is
-- the low 20 bytes of keccak256 of Q's uncompressed coordinates.
--
-- Written against Lua 5.4's native 64-bit integers, standard library only —
-- the same deployment story as accounts_drive.lua, and it borrows that
-- module's keccak256. Field and scalar elements are 256-bit numbers held as
-- eight 32-bit limbs (tables, least-significant limb first), so every limb
-- product fits a 64-bit integer with room for a carry: a·b ≤ (2³²−1)² and
-- adding a limb plus a carry stays below 2⁶⁴. Lua's integers are signed, but
-- `&`, `|` and the logical `>>` operate on the raw two's-complement bits, so
-- values past 2⁶³ still shift and mask correctly.
--
-- Modular reduction uses the primes' sparseness: for m ∈ {p, n} the constant
-- k = 2²⁵⁶ − m is small (33 bits for p, 129 for n), so a wide value folds by
-- value = lo₂₅₆ + hi·k until it fits 256 bits, then subtracts m at most a
-- few times. Inverses and the square root are Fermat exponentiations (p ≡ 3
-- mod 4, so √a = a^((p+1)/4)). The group operation runs in Jacobian
-- coordinates — one inversion at the very end — and u₁G + u₂R is a shared
-- (Shamir) double-and-add ladder.
--
-- PERFORMANCE: this is real arithmetic in an interpreted language — one
-- recovery costs on the order of 10⁴ modular multiplications (tens of
-- milliseconds of host CPU; devnet/test-guest.lua measures and prints it).
-- Inside the machine the guest pays that again through an emulated RISC-V
-- CPU, so sender recovery dominates an L2 transaction's cycle cost. That is
-- the honest price of guest-side enforcement (ACCOUNTS.md §6.2) until a
-- native-code guest takes over; MaxCyclesPerInput bounds it per input.
--
-- Correctness is pinned by devnet/test-guest.lua's self-test vectors, which
-- were generated with go-ethereum and verified against crypto.Ecrecover
-- before being trusted.

local M = {}

local keccak256 = require("accounts_drive").keccak256

-- ====================================================================
-- 256-bit arithmetic on 32-bit limbs
-- ====================================================================

local function fromhex(s) -- 64 hex chars, big-endian → limbs
  assert(#s == 64, "fromhex wants exactly 64 hex chars")
  local a = {}
  for i = 1, 8 do
    a[i] = tonumber(s:sub(64 - 8 * i + 1, 64 - 8 * i + 8), 16)
  end
  return a
end

local function frombytes(b) -- 32-byte big-endian string → limbs
  local a = {}
  for i = 1, 8 do
    a[i] = string.unpack(">I4", b, 32 - 4 * i + 1)
  end
  return a
end

local function tobytes(a) -- limbs → 32-byte big-endian string
  local parts = {}
  for i = 8, 1, -1 do
    parts[#parts + 1] = string.pack(">I4", a[i])
  end
  return table.concat(parts)
end

local function iszero(a)
  for i = 1, 8 do
    if a[i] ~= 0 then return false end
  end
  return true
end

local function cmp(a, b)
  for i = 8, 1, -1 do
    if a[i] ~= b[i] then return a[i] < b[i] and -1 or 1 end
  end
  return 0
end

-- usub computes a − b mod 2²⁵⁶ (the wrap is exactly what the callers that
-- subtract from an implicit 2²⁵⁶ rely on).
local function usub(a, b)
  local r, borrow = {}, 0
  for i = 1, 8 do
    local d = a[i] - b[i] - borrow
    if d < 0 then d = d + 0x100000000; borrow = 1 else borrow = 0 end
    r[i] = d
  end
  return r
end

-- mulvar multiplies two little-endian limb arrays of any length
-- (schoolbook, carries propagated per product so nothing overflows 64 bits).
local function mulvar(a, b)
  local r = {}
  for i = 1, #a + #b do r[i] = 0 end
  for i = 1, #a do
    local ai = a[i]
    if ai ~= 0 then
      local carry = 0
      for j = 1, #b do
        local t = r[i + j - 1] + ai * b[j] + carry
        r[i + j - 1] = t & 0xffffffff
        carry = t >> 32
      end
      local k = i + #b
      while carry ~= 0 do
        local t = r[k] + carry
        r[k] = t & 0xffffffff
        carry = t >> 32
        k = k + 1
      end
    end
  end
  return r
end

-- A modulus context: m = the modulus (8 limbs), k = 2²⁵⁶ mod m, trimmed of
-- leading zero limbs so the fold multiplies by as little as possible.
local function context(m)
  local k = usub({ 0, 0, 0, 0, 0, 0, 0, 0 }, m) -- 2²⁵⁶ − m, via the wrap
  local n = 8
  while n > 1 and k[n] == 0 do n = n - 1 end
  local kt = {}
  for i = 1, n do kt[i] = k[i] end
  return { m = m, k = kt }
end

-- reduce folds a wide little-endian limb array below the modulus:
-- repeatedly value = lo₂₅⁶ + hi·(2²⁵⁶ mod m), then subtract m while needed.
local function reduce(t, ctx)
  while true do
    local wide = false
    for i = 9, #t do
      if t[i] ~= 0 then wide = true; break end
    end
    if not wide then break end
    local hi = {}
    for i = 9, #t do hi[i - 8] = t[i] end
    local folded = mulvar(hi, ctx.k)
    -- add the low 256 bits back in
    local carry = 0
    for i = 1, 8 do
      local s = (folded[i] or 0) + t[i] + carry
      folded[i] = s & 0xffffffff
      carry = s >> 32
    end
    local k = 9
    while carry ~= 0 do
      local s = (folded[k] or 0) + carry
      folded[k] = s & 0xffffffff
      carry = s >> 32
      k = k + 1
    end
    t = folded
  end
  local r = {}
  for i = 1, 8 do r[i] = t[i] or 0 end
  while cmp(r, ctx.m) >= 0 do r = usub(r, ctx.m) end
  return r
end

local function mulmod(a, b, ctx)
  return reduce(mulvar(a, b), ctx)
end

local function addmod(a, b, ctx)
  local r, carry = {}, 0
  for i = 1, 8 do
    local s = a[i] + b[i] + carry
    r[i] = s & 0xffffffff
    carry = s >> 32
  end
  if carry ~= 0 or cmp(r, ctx.m) >= 0 then r = usub(r, ctx.m) end
  return r
end

local function submod(a, b, ctx)
  if cmp(a, b) >= 0 then return usub(a, b) end
  -- a − b + m, computed mod 2²⁵⁶: the two wraps cancel.
  local t = usub(a, b)
  local r, carry = {}, 0
  for i = 1, 8 do
    local s = t[i] + ctx.m[i] + carry
    r[i] = s & 0xffffffff
    carry = s >> 32
  end
  return r
end

-- powmod is square-and-multiply over the exponent's 256 bits, MSB first.
local function powmod(a, e, ctx)
  local r, started = nil, false
  for i = 8, 1, -1 do
    local w = e[i]
    for bit = 31, 0, -1 do
      if started then r = mulmod(r, r, ctx) end
      if (w >> bit) & 1 == 1 then
        if started then r = mulmod(r, a, ctx) else r = a; started = true end
      end
    end
  end
  return r or { 1, 0, 0, 0, 0, 0, 0, 0 } -- a⁰ = 1
end

-- ====================================================================
-- The curve: y² = x³ + 7 over F_p, group order n, generator G
-- ====================================================================

local P = fromhex("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f")
local N = fromhex("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141")
local Pctx = context(P)
local Nctx = context(N)

local GX = fromhex("79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
local GY = fromhex("483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8")

local SEVEN = { 7, 0, 0, 0, 0, 0, 0, 0 }
local TWO = { 2, 0, 0, 0, 0, 0, 0, 0 }
local P_MINUS_2 = usub(P, TWO) -- Fermat inverse exponent mod p
local N_MINUS_2 = usub(N, TWO) -- Fermat inverse exponent mod n

-- (p+1)/4: p ≡ 3 (mod 4), so a^((p+1)/4) is a square root when one exists.
local SQRT_EXP = (function()
  local t = { P[1] + 1 } -- p is odd, no carry past the first limb
  for i = 2, 8 do t[i] = P[i] end
  local r = {}
  for i = 1, 8 do
    r[i] = ((t[i] >> 2) | ((t[i + 1] or 0) << 30)) & 0xffffffff
  end
  return r
end)()

-- Points are Jacobian {x, y, z} (affine x = x/z², y = y/z³); the point at
-- infinity is nil, which the ladder's accumulator starts as.

-- pdouble: dbl-2009-l, specialized to a = 0.
local function pdouble(pt)
  if not pt or iszero(pt.y) then return nil end
  local A = mulmod(pt.x, pt.x, Pctx)
  local B = mulmod(pt.y, pt.y, Pctx)
  local C = mulmod(B, B, Pctx)
  local t = addmod(pt.x, B, Pctx)
  t = mulmod(t, t, Pctx)
  t = submod(submod(t, A, Pctx), C, Pctx)
  local D = addmod(t, t, Pctx)
  local E = addmod(addmod(A, A, Pctx), A, Pctx)
  local F = mulmod(E, E, Pctx)
  local X3 = submod(F, addmod(D, D, Pctx), Pctx)
  local C8 = addmod(C, C, Pctx)
  C8 = addmod(C8, C8, Pctx)
  C8 = addmod(C8, C8, Pctx)
  local Y3 = submod(mulmod(E, submod(D, X3, Pctx), Pctx), C8, Pctx)
  local YZ = mulmod(pt.y, pt.z, Pctx)
  return { x = X3, y = Y3, z = addmod(YZ, YZ, Pctx) }
end

-- madd: madd-2007-bl, Jacobian + affine (x2, y2).
local function madd(pt, x2, y2)
  if not pt then
    return { x = x2, y = y2, z = { 1, 0, 0, 0, 0, 0, 0, 0 } }
  end
  local Z1Z1 = mulmod(pt.z, pt.z, Pctx)
  local U2 = mulmod(x2, Z1Z1, Pctx)
  local S2 = mulmod(y2, mulmod(pt.z, Z1Z1, Pctx), Pctx)
  local H = submod(U2, pt.x, Pctx)
  local rr = submod(S2, pt.y, Pctx)
  if iszero(H) then
    if iszero(rr) then return pdouble(pt) end
    return nil -- P + (−P) = ∞
  end
  rr = addmod(rr, rr, Pctx)
  local HH = mulmod(H, H, Pctx)
  local I = addmod(HH, HH, Pctx)
  I = addmod(I, I, Pctx)
  local J = mulmod(H, I, Pctx)
  local V = mulmod(pt.x, I, Pctx)
  local X3 = submod(submod(mulmod(rr, rr, Pctx), J, Pctx), addmod(V, V, Pctx), Pctx)
  local YJ = mulmod(pt.y, J, Pctx)
  local Y3 = submod(mulmod(rr, submod(V, X3, Pctx), Pctx), addmod(YJ, YJ, Pctx), Pctx)
  local Z3 = addmod(pt.z, H, Pctx) -- (Z1+H)² − Z1Z1 − HH = 2·Z1·H
  Z3 = mulmod(Z3, Z3, Pctx)
  Z3 = submod(submod(Z3, Z1Z1, Pctx), HH, Pctx)
  return { x = X3, y = Y3, z = Z3 }
end

-- shamir computes u1·G + u2·R with one shared double-and-add ladder.
local function shamir(u1, u2, rx, ry)
  local acc = nil
  for i = 8, 1, -1 do
    local w1, w2 = u1[i], u2[i]
    for bit = 31, 0, -1 do
      acc = pdouble(acc)
      if (w1 >> bit) & 1 == 1 then acc = madd(acc, GX, GY) end
      if (w2 >> bit) & 1 == 1 then acc = madd(acc, rx, ry) end
    end
  end
  return acc
end

-- ====================================================================
-- Recovery
-- ====================================================================

-- recover(hash, recid, r, s) returns the signer's 20-byte address, or nil
-- when the signature does not recover: r or s outside [1, n−1], a recovery
-- id other than 0 or 1, an r that is not the x-coordinate of a curve point,
-- or a recovery that lands on the point at infinity. hash, r and s are
-- 32-byte big-endian strings. Ethereum's recid is v − 27 for legacy
-- signatures (the EIP-155 normalization is the caller's, since v encodes
-- the chain id there); ids 2 and 3 — r taken from an x beyond n — are not
-- produced by Ethereum tooling and are rejected here.
--
-- The low-s rule (EIP-2) is deliberately NOT enforced here: it is Ethereum
-- transaction policy, not ECDSA, and the caller owns it.
function M.recover(hash, recid, r, s)
  if type(hash) ~= "string" or #hash ~= 32
      or type(r) ~= "string" or #r ~= 32
      or type(s) ~= "string" or #s ~= 32 then
    error("recover: hash, r and s must be 32-byte strings", 2)
  end
  if recid ~= 0 and recid ~= 1 then return nil end
  local rn = frombytes(r)
  local sn = frombytes(s)
  if iszero(rn) or cmp(rn, N) >= 0 then return nil end
  if iszero(sn) or cmp(sn, N) >= 0 then return nil end

  -- Lift R = (x, y): x = r (valid since r < n < p), y the root with the
  -- recovery id's parity.
  local x = rn
  local y2 = addmod(mulmod(mulmod(x, x, Pctx), x, Pctx), SEVEN, Pctx)
  local y = powmod(y2, SQRT_EXP, Pctx)
  if cmp(mulmod(y, y, Pctx), y2) ~= 0 then
    return nil -- x³ + 7 is not a square: r is not a curve x-coordinate
  end
  if y[1] & 1 ~= recid then
    if iszero(y) then return nil end -- no root of the requested parity
    y = usub(P, y)
  end

  -- z = the hash as an integer mod n (hash < 2²⁵⁶ < 2n, so one subtract).
  local z = frombytes(hash)
  if cmp(z, N) >= 0 then z = usub(z, N) end

  -- Q = r⁻¹(sR − zG) = u1·G + u2·R with u1 = −z·r⁻¹, u2 = s·r⁻¹ (mod n).
  local rinv = powmod(rn, N_MINUS_2, Nctx)
  local u1 = mulmod(z, rinv, Nctx)
  if not iszero(u1) then u1 = usub(N, u1) end
  local u2 = mulmod(sn, rinv, Nctx)

  local q = shamir(u1, u2, x, y)
  if not q then return nil end

  local zinv = powmod(q.z, P_MINUS_2, Pctx)
  local zinv2 = mulmod(zinv, zinv, Pctx)
  local qx = mulmod(q.x, zinv2, Pctx)
  local qy = mulmod(q.y, mulmod(zinv2, zinv, Pctx), Pctx)
  return keccak256(tobytes(qx) .. tobytes(qy)):sub(13, 32)
end

return M
SECPEOF

cat > /var/lib/bank.lua <<'LUAEOF'
-- ---------------------------------------------------------------- rollup I/O
-- The `rollup` tool reads its argument object on stdin and writes the request
-- to stdout, so each call is a round trip through two temporary files.
local function rollup(sub, body)
  local f = assert(io.open("/var/lib/bank.in", "w"))
  f:write(body)
  f:close()
  local p = assert(io.popen("rollup " .. sub .. " < /var/lib/bank.in 2>/dev/null", "r"))
  local out = p:read("a")
  p:close()
  return out
end

-- The request objects are small and fixed-shape, so matching the two fields
-- this app reads beats carrying a JSON parser into the guest.
local function field(s, name)
  return s:match('"' .. name .. '"%s*:%s*"([^"]*)"')
end

-- ------------------------------------------------------------------ 256-bit
-- Balances are wei, which overflows Lua's 64-bit integers, so they are carried
-- as 64-character hex and added 32 bits at a time.
local ZERO = string.rep("0", 64)

local function pad(hex)
  hex = hex:gsub("^0x", "")
  return string.rep("0", 64 - #hex) .. hex
end

-- sub returns a - b, or nil when b is larger; balances must never wrap.
local function sub(a, b)
  a, b = pad(a), pad(b)
  local limbs, borrow = {}, 0
  for i = 57, 1, -8 do
    local diff = tonumber(a:sub(i, i + 7), 16) - tonumber(b:sub(i, i + 7), 16) - borrow
    borrow = diff < 0 and 1 or 0
    if diff < 0 then diff = diff + 0x100000000 end
    table.insert(limbs, 1, string.format("%08x", diff))
  end
  if borrow ~= 0 then return nil end
  return table.concat(limbs)
end

local function add(a, b)
  a, b = pad(a), pad(b)
  local limbs, carry = {}, 0
  for i = 57, 1, -8 do
    local sum = tonumber(a:sub(i, i + 7), 16) + tonumber(b:sub(i, i + 7), 16) + carry
    carry = sum > 0xffffffff and 1 or 0
    table.insert(limbs, 1, string.format("%08x", sum & 0xffffffff))
  end
  return table.concat(limbs)
end

local function isZero(hex)
  return pad(hex) == ZERO
end

-- ---------------------------------------------------------------------- RLP
-- decode returns the payload span [from, to] of the item starting at i, the
-- index just past it, and whether it is a list. Spans are used rather than
-- copies so nested items can be walked without slicing the whole string.
--
-- Anything malformed comes back as nil, and every caller has to say what it
-- does about that. Inputs are whatever an L1 contract or an L2 sender chose to
-- send, so a length that runs off the end is not a rare case: it is the shape
-- of the first input written to stop the chain. An error here would halt the
-- machine, and a halted machine is a halted chain.
local function decode(b, i)
  local prefix = b:byte(i)
  if not prefix then return nil end
  local from, to, isList
  local function long(lenlen)
    if i + lenlen > #b then return nil end
    local n = 0
    for k = i + 1, i + lenlen do n = n * 256 + b:byte(k) end
    return i + 1 + lenlen, i + lenlen + n
  end
  if prefix < 0x80 then
    from, to, isList = i, i, false
  elseif prefix <= 0xb7 then
    from, to, isList = i + 1, i + prefix - 0x80, false
  elseif prefix <= 0xbf then
    from, to = long(prefix - 0xb7)
    isList = false
  elseif prefix <= 0xf7 then
    from, to, isList = i + 1, i + prefix - 0xc0, true
  else
    from, to = long(prefix - 0xf7)
    isList = true
  end
  if not from or to > #b then return nil end
  return from, to, to + 1, isList
end

local function hex(b, from, to)
  return (b:sub(from, to):gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

local function hexdecode(s)
  return (s:gsub("^0x", ""):gsub("%x%x", function(cc) return string.char(tonumber(cc, 16)) end))
end

-- ------------------------------------------------------------------ deposits
-- An OP deposit is 0x7e followed by
--   rlp[sourceHash, from, to, mint, value, gas, isSystemTx, data]
-- op-node builds it from the L1 TransactionDeposited log; here it is taken
-- apart again. `from` is the L1 caller, aliased if it was a contract, and it
-- is what the guest authenticates against — the same role msgSender plays in
-- a Cartesi Rollups input.
local function parseDeposit(raw)
  if #raw < 2 or raw:byte(1) ~= 0x7e then return nil end
  local from, _, _, isList = decode(raw, 2)
  if not from or not isList then return nil end
  local spans, i = {}, from
  for _ = 1, 8 do
    local f, t, nxt = decode(raw, i)
    if not f then return nil end
    spans[#spans + 1] = { f, t }
    i = nxt
  end
  local function at(n) return hex(raw, spans[n][1], spans[n][2]) end
  return {
    sourceHash = at(1),
    from = at(2),
    to = at(3),
    mint = at(4),
    value = at(5),
    -- Raw bytes rather than hex: the portal payloads below are read by offset.
    data = raw:sub(spans[8][1], spans[8][2]),
  }
end

-- ------------------------------------------------------------------- ledger
-- The ledger is the accounts drive. Ether lives in the account record's
-- uint256 balance; ERC-20 balances live in the sparse table, with tokens
-- registered first-seen on deposit (width 32: devnet tokens are arbitrary
-- ERC-20s, so they get the full uint256). The zero address still stands for
-- ether at this app's boundaries (inspect queries, portal payloads); it maps
-- to the account record rather than to a token id.
--
-- build-snapshot.sh writes the library to /var/lib next to this app.
package.path = "/var/lib/?.lua;" .. package.path
local ad = require("accounts_drive")
-- Pure-Lua ECDSA recovery (see the module's header for what it costs): the
-- guest must recover a transaction's sender itself — trusting anything the
-- host says about the sender would put the nonce check outside the proven
-- state transition, which is the whole reason enforcement lives here.
local secp = require("secp256k1")

local ETHER = string.rep("0", 40)

-- The accounts drive is the machine's second flash drive: the kernel numbers
-- pmem devices in --flash-drive order, the root filesystem is always first
-- (/dev/pmem0), and build-snapshot.sh declares the accounts drive right after
-- it — so it is /dev/pmem1.
local drive_fs = assert(ad.file_store("/dev/pmem1"))

-- Format the drive at boot if it is still blank (build-snapshot.sh ships it
-- zero-filled). This runs before the first yield, so the formatted header is
-- part of the stored snapshot and covered by the genesis state root like
-- every other chain parameter. The geometry below is therefore consensus:
-- header page, in-header token registry (offset 0x100, capacity 8), accounts
-- table at 4096 with 4096 64-byte slots, sparse table right after it at
-- 266240 (= 4096 + 4096*64) with 4096 64-byte slots; load limits are the
-- library's 7/8 default. The seed stays the library's all-zero default — a
-- devnet chain parameter, kept at zero so genesis is reproducible from this
-- file alone; a production chain would pick a random seed at genesis to make
-- probe-chain grinding (ACCOUNTS.md §5.3) start from nothing.
local drive
if drive_fs:read_at(0, 8) == "ctsiacct" then
  drive = assert(ad.open(drive_fs))
else
  drive = assert(ad.format(drive_fs, {
    drive_length = 1048576, -- 1 MiB (2^20), matches build-snapshot.sh
    profile = ad.PROFILE_SPARSE,
    capacity = 4096,
    registry_offset = 0x100,
    registry_capacity = 8,
    sparse_offset = 266240,
    sparse_capacity = 4096,
  }))
end

-- syncDrive gets the input's drive writes out of Lua's stdio buffer and the
-- kernel page cache and into the device — i.e. into machine state — before
-- the next yield (spec §10). Cartesi's kernel has no DAX, so a plain write()
-- can still be sitting in guest RAM at the yield, where the host could not
-- read it and the genesis root would not cover the format above.
local function syncDrive()
  drive_fs:sync()
  os.execute("sync")
end

-- Conversions between the app's 64-char-hex amounts and the library's raw
-- 20-byte addresses / 32-byte big-endian balances, using the hex helpers
-- above. hex32(bin) is hex() applied to a whole 32-byte string.
local function hex32(bin) return hex(bin, 1, 32) end

-- tokenId resolves a token to its registry id, optionally registering it
-- first-seen (register_token returns nil, kind when the registry is full).
-- Note id 0 is a valid id and Lua treats it as truthy, so `if id` works.
local function tokenId(token, register)
  local id = drive:token_by_address(hexdecode(token))
  if id ~= nil then return id end
  if not register then return nil end
  return drive:register_token(hexdecode(token), 32)
end

local function balanceOf(addr, token)
  -- Queries arrive with attacker-chosen lengths (inspect); anything that is
  -- not a 20-byte address has no balance rather than being an error.
  if #addr ~= 40 then return ZERO end
  token = token or ETHER
  if token == ETHER then
    local acct = drive:get_account(hexdecode(addr))
    if not acct then return ZERO end
    return hex32(acct.balance)
  end
  local id = tokenId(token, false)
  if id == nil then return ZERO end
  local bal = drive:get_token_balance(hexdecode(addr), id)
  if not bal then return ZERO end
  return hex32(bal)
end

-- setBalance writes a 64-char-hex balance to the drive; register says whether
-- an unseen token may be registered (credits yes, debits no — registration is
-- first-seen *on deposit*). Returns true, or nil when the drive refuses:
-- tableFull, registryFull and overflow are exactly the conditions the spec
-- requires to be answered by rejecting the input (spec §6.3, §8), and the
-- machine rollback that rejection triggers reverts the drive along with
-- everything else, so a refused write needs no undo.
local function setBalance(addr, token, value, register)
  if token == ETHER then
    local a = hexdecode(addr)
    local acct = drive:get_account(a)
    -- The nonce rides along unchanged: balance writes never touch it. The
    -- one writer of nonces is the enforcement commit in advance(), which
    -- bumps the sender's nonce exactly when their transaction is accepted.
    return drive:set_account(a, acct and acct.nonce or 0, hexdecode(pad(value)))
  end
  local id = tokenId(token, register)
  if id == nil then return nil end
  return drive:set_token_balance(hexdecode(addr), id, hexdecode(pad(value)))
end

-- credit returns the new balance, or nil when the drive refuses the write —
-- the caller must then reject the input (see setBalance).
local function credit(addr, amount, token)
  token = token or ETHER
  local updated = add(balanceOf(addr, token), amount)
  if not setBalance(addr, token, updated, true) then return nil end
  return updated
end

-- debit returns the new balance, or nil when the account is short (or the
-- drive refuses the write; either way the caller rejects).
local function debit(addr, amount, token)
  token = token or ETHER
  local left = sub(balanceOf(addr, token), amount)
  if not left then return nil end
  if token ~= ETHER and tokenId(token, false) == nil then
    -- A token nobody ever deposited can only get here with amount zero (its
    -- balance is zero); there is no record to update, and a withdrawal must
    -- not be a way to mint registry entries.
    return left
  end
  if not setBalance(addr, token, left, false) then return nil end
  return left
end

-- ------------------------------------------------------------------- portals
-- A Cartesi portal deposit is not an Ethereum transaction: the portal calls
-- OptimismPortal.depositTransaction with a packed payload, so what arrives is
-- those bytes in the deposit's data field, with `from` set to the aliased
-- portal. The layouts are Cartesi's own InputEncoding:
--
--   ether: sender(20) ‖ value(32) ‖ extra
--   erc20: token(20) ‖ sender(20) ‖ value(32) ‖ extra
--
-- Which one it is comes from *who sent it*, not from the bytes — the portals
-- are distinct L1 contracts, so their aliased addresses distinguish them. That
-- is what stops one portal from claiming to be another, and it is why the
-- guest cannot simply trust any sender: a contract that could pass off its own
-- bytes as an ERC-20 deposit would mint credit against tokens the application
-- really holds, and drain them through a voucher.
--
-- OWNER is a genesis parameter. It is the only address whose messages the
-- guest treats as configuration, and it is fixed before anything is deployed —
-- which is what breaks the circularity, since the portal addresses are not
-- known until L1 is deployed and the L1 deployment cannot precede a genesis
-- state that names them. devnet/build-snapshot.sh substitutes it.
local OWNER = "__OWNER__"

local REGISTER = 0x70 -- "p", followed by kind(1) ‖ portal(20)
local KIND_ETHER, KIND_ERC20 = 0x00, 0x01

-- OP aliases an L1 contract's address when it deposits: alias(a) = a + offset,
-- truncated to 160 bits. The guest is told the plain L1 address and does the
-- aliasing itself, so what is registered is what a block explorer shows.
local ALIAS_OFFSET = "1111000000000000000000000000000000001111"

local function alias(addr)
  return add(addr, ALIAS_OFFSET):sub(25)
end

-- Aliased portal address -> kind.
local portals = {}

local function register(data)
  local kind, portal = data:byte(2), hex(data, 3, 22)
  if kind ~= KIND_ETHER and kind ~= KIND_ERC20 then return nil end
  local aliased = alias(portal)
  portals[aliased] = kind
  return portal, aliased
end

local function parsePortalDeposit(data, from)
  local kind = portals[from]
  if kind == KIND_ERC20 and #data >= 72 then
    return { token = hex(data, 1, 20), sender = hex(data, 21, 40), amount = hex(data, 41, 72) }
  elseif kind == KIND_ETHER and #data >= 52 then
    return { token = ETHER, sender = hex(data, 1, 20), amount = hex(data, 21, 52) }
  end
  return nil
end

-- --------------------------------------------------------- L2 transactions
-- An ordinary L2 transaction reaches the guest as the whole signed legacy
-- transaction,
--   rlp[nonce, gasPrice, gasLimit, to, value, data, v, r, s]
-- and since ACCOUNTS.md roadmap v1 the guest takes all of it seriously, not
-- just the calldata: the sender is recovered from the signature, the nonce
-- must equal the sender's account record's nonce, and acceptance bumps that
-- nonce and debits the flat fee below. That check is what closes the README's
-- replay gap, and it has to live here — the drive is proven state, and a
-- nonce check outside the state transition would be a courtesy, not a rule
-- (the shim's mempool gate is exactly that courtesy).

-- parseLegacyTx takes the transaction apart, keeping the values enforcement
-- needs and the *encoded* bytes of items 1..6 — the sighash reuses them
-- byte-for-byte, so no re-encoding here can disagree with what was signed.
-- Anything structurally wrong comes back nil (same doctrine as decode: the
-- caller decides, and a malformed input must never halt the machine).
local function parseLegacyTx(raw)
  local from, last, _, isList = decode(raw, 1)
  if not from or not isList or last ~= #raw then return nil end
  local spans, encEnd, i = {}, {}, from
  for f = 1, 9 do
    if i > last then return nil end
    local a, b, nxt, nested = decode(raw, i)
    if not a or nested then return nil end
    spans[f] = { a, b }
    encEnd[f] = nxt - 1
    i = nxt
  end
  if i ~= last + 1 then return nil end
  local function int(f, maxlen)
    local a, b = spans[f][1], spans[f][2]
    if b - a + 1 > maxlen then return nil end
    local v = 0
    for k = a, b do v = v * 256 + raw:byte(k) end
    return v
  end
  local function word(f)
    local a, b = spans[f][1], spans[f][2]
    if b - a + 1 > 32 then return nil end
    return ("\0"):rep(32 - (b - a + 1)) .. raw:sub(a, b)
  end
  return {
    nonce = int(1, 8),
    data = raw:sub(spans[6][1], spans[6][2]),
    v = int(7, 8),
    r = word(8),
    s = word(9),
    sigItems = raw:sub(from, encEnd[6]),
  }
end

-- The two scraps of RLP *encoding* the sighash needs beyond the
-- transaction's own bytes: a list length prefix and a minimal integer.
local function rlpLen(n, offset)
  if n < 56 then return string.char(offset + n) end
  local bytes = ""
  while n > 0 do
    bytes = string.char(n % 256) .. bytes
    n = n // 256
  end
  return string.char(offset + 55 + #bytes) .. bytes
end

local function rlpInt(n)
  if n == 0 then return "\x80" end
  local bytes = ""
  while n > 0 do
    bytes = string.char(n % 256) .. bytes
    n = n // 256
  end
  if #bytes == 1 and bytes:byte(1) < 0x80 then return bytes end
  return string.char(0x80 + #bytes) .. bytes
end

-- EIP-2's upper bound on s (n ÷ 2): a signature with a higher s has a valid
-- twin with the same r, and admitting both would give every transaction two
-- hashes. 32-byte big-endian strings compare correctly as strings.
local HALF_N = hexdecode("7fffffffffffffffffffffffffffffff5d576e7357a4501ddfe92f46681b20a0")

-- recoverSender returns the 20-byte sender, or nil for anything that does
-- not verify. Both signature forms are accepted: pre-EIP-155 (v 27/28,
-- sighash over items 1..6) and EIP-155 (v = chainId·2 + 35 + parity,
-- sighash over items 1..6 ‖ chainId ‖ 0 ‖ 0). The chain id used is the one
-- the transaction itself names — recovery is only valid under that id, so a
-- signature can never be transplanted to another sender; pinning inputs to
-- *this* chain's id happens at ingress (the mempool's chain-id signer), and
-- a production guest would take the chain id as a genesis parameter exactly
-- like OWNER.
local function recoverSender(tx)
  if not (tx.nonce and tx.v and tx.r and tx.s) then return nil end
  if tx.s > HALF_N then return nil end
  local rid, payload
  if tx.v == 27 or tx.v == 28 then
    rid = tx.v - 27
    payload = tx.sigItems
  elseif tx.v >= 35 then
    rid = (tx.v - 35) % 2
    payload = tx.sigItems .. rlpInt((tx.v - 35) // 2) .. "\x80\x80"
  else
    return nil
  end
  return secp.recover(ad.keccak256(rlpLen(#payload, 0xc0) .. payload), rid, tx.r, tx.s)
end

-- ----------------------------------------------------------------- flat fee
-- The flat per-transaction charge of ACCOUNTS.md §5.7. Nonce records are
-- permanent — deleting one would re-arm replay of every past transaction —
-- so the first transaction from a fresh sender mints 64 bytes of drive
-- forever, and that must not be free. The fee is owner configuration, tag
-- "f" ‖ uint256 wei, the same trust root as the portal registrations.
--
-- THE GENESIS DEFAULT IS ZERO, DELIBERATELY AND LOUDLY (ACCOUNTS.md §5.7):
-- devnet senders hold no ether until someone deposits, so a nonzero fee at
-- genesis would reject every first transaction on a fresh devnet and break
-- every script that sends one. Zero keeps the §5.7 machinery wired but
-- unpriced — record-minting stays free until the owner sets a real fee,
-- which a production deployment must do as soon as it cares about table
-- exhaustion.
local FEE = ZERO
local SET_FEE = 0x66 -- "f", followed by fee(32)

-- -------------------------------------------------------------- withdrawals
-- A withdrawal request is carried in the calldata of an ordinary L2
-- transaction:
--
--   "w" ‖ recipient(20) ‖ amount(32)              ether
--   "t" ‖ token(20) ‖ recipient(20) ‖ amount(32)  tokens
--
-- The request still names its payee in the calldata and debits that account,
-- not the sender's: tying withdrawals to the authenticated sender is app
-- policy, deliberately unchanged in v1. What v1 adds is that the carrier
-- transaction must be validly signed, correctly nonced and fee-paid before
-- any of this runs.
local WITHDRAW, WITHDRAW_TOKEN = 0x77, 0x74

-- ERC20.transfer(address,uint256).
local TRANSFER = "a9059cbb"

-- A voucher is one call made from the application contract on L1. Paying ether
-- is a call to the recipient with a value and no payload; paying a token is a
-- call to the token with no value and a transfer payload. Both are the same
-- output type — the asset lives on L1, and the guest only says what to do
-- with it.
local function voucher(destination, value, payload)
  rollup("voucher", '{"destination":"0x' .. destination
    .. '","value":"0x' .. pad(value) .. '","payload":"0x' .. payload .. '"}')
end

-- ---------------------------------------------------------------- inputs
-- inspect answers a read-only query: a 20-byte address for ether, or
-- token ‖ address for a token. It is not a transaction and never reaches the
-- decoders above.
local function inspect(raw)
  if #raw == 40 then
    rollup("report", '{"payload":"0x' .. balanceOf(hex(raw, 21, 40), hex(raw, 1, 20)) .. '"}')
  else
    rollup("report", '{"payload":"0x' .. balanceOf(hex(raw, 1, #raw)) .. '"}')
  end
end

-- dispatch is the input's application semantics — deposits, withdrawals,
-- configuration — separated from the transaction-level enforcement that
-- advance() runs first. It returns the status the input should finish with.
local function dispatch(raw, deposit, data)
  local tag = data and #data > 0 and data:byte(1) or nil
  local portalDeposit = deposit and data and parsePortalDeposit(data, deposit.from)

  if deposit and deposit.from == OWNER and tag == REGISTER and #data == 22 then
    -- Configuration, and the only input the guest takes on anyone's word.
    local portal, aliased = register(data)
    if not portal then return "reject" end
    -- A notice, not a report: which contracts the guest will credit deposits
    -- from is consensus state, so it belongs in the outputs tree where it can
    -- be proven rather than in a diagnostic nobody commits to.
    rollup("notice", '{"payload":"0x' .. pad(portal) .. pad(aliased) .. '"}')
    rollup("report", '{"payload":"0x' .. pad(portal) .. pad(aliased) .. '"}')

  elseif deposit and deposit.from == OWNER and tag == SET_FEE and #data == 33 then
    -- The flat fee (see the fee section above): owner configuration, like
    -- the portal registrations. The fee is consensus state — every node
    -- must charge identically — so the change is a notice, provable in the
    -- outputs tree, not just a diagnostic.
    FEE = hex(data, 2, 33)
    rollup("notice", '{"payload":"0x' .. FEE .. '"}')
    rollup("report", '{"payload":"0x' .. FEE .. '"}')

  elseif portalDeposit then
    -- A Cartesi portal deposit. The asset is escrowed in the application
    -- contract on L1, so this credit is one a voucher can actually pay out.
    local d = portalDeposit
    local updated = credit(d.sender, d.amount, d.token)
    -- A refused credit (accounts table or registry at its load limit) must
    -- reject the input (spec §6.3, §8); rejection rolls the drive back with
    -- the rest of the machine. The L1 escrow has already happened — this is
    -- the deposit-stuck caveat of ACCOUNTS.md §5.3, answered by capacity
    -- headroom, not by cleverness here.
    if not updated then return "reject" end
    rollup("notice", '{"payload":"0x' .. pad(d.token) .. pad(d.sender) .. pad(d.amount) .. updated .. '"}')
    rollup("report", '{"payload":"0x' .. pad(d.sender) .. updated .. '"}')

  elseif deposit and not isZero(deposit.mint) and #deposit.to == 40 then
    -- OP's own ether deposit. Crediting the recipient rather than the sender
    -- collapses OP's two-step "mint to `from`, then transfer `value` to `to`"
    -- into the one step this guest can express: it has no transfers, only a
    -- ledger. For an ETH deposit the two agree, since mint equals value and
    -- `to` is the named recipient.
    --
    -- The ether itself stays in OP's lockbox, which no voucher can reach, so
    -- this credit is only as good as whatever else funds the application
    -- contract. OPEtherPortal is the path where the two directions agree.
    -- (The `to == 40` guard above: a deposit with an empty or malformed `to`
    -- has no 20-byte account to credit — the drive is keyed by real
    -- addresses — so it falls through to the record-and-accept branch.)
    local updated = credit(deposit.to, deposit.mint)
    if not updated then return "reject" end -- drive refused: see the portal branch
    -- The notice is the provable half: it enters the outputs tree and so the
    -- block's withdrawals root, which is what a verifier re-derives.
    rollup("notice", '{"payload":"0x' .. pad(deposit.to) .. pad(deposit.mint) .. updated .. '"}')
    rollup("report", '{"payload":"0x' .. pad(deposit.to) .. updated .. '"}')

  elseif tag == WITHDRAW and #data == 53 then
    local who, amount = hex(data, 2, 21), hex(data, 22, 53)
    local left = debit(who, amount)
    if not left then
      rollup("report", '{"payload":"0x' .. pad(who) .. ZERO .. '"}')
      return "reject"
    end
    voucher(who, amount, "")
    rollup("report", '{"payload":"0x' .. pad(who) .. left .. '"}')

  elseif tag == WITHDRAW_TOKEN and #data == 73 then
    local token, who, amount = hex(data, 2, 21), hex(data, 22, 41), hex(data, 42, 73)
    local left = debit(who, amount, token)
    if not left then
      rollup("report", '{"payload":"0x' .. pad(who) .. ZERO .. '"}')
      return "reject"
    end
    -- The voucher moves no ether: it tells the token to move itself, from the
    -- application contract that has held it since the deposit.
    voucher(token, "", TRANSFER .. pad(who) .. pad(amount))
    rollup("report", '{"payload":"0x' .. pad(who) .. left .. '"}')

  else
    -- Nothing this guest knows, including anything that failed to decode. It
    -- records the input and moves on rather than rejecting, which would roll
    -- the machine back over what may well be a well-formed message for a
    -- later version of this app.
    rollup("report", '{"payload":"0x' .. hex(raw, 1, #raw) .. '"}')
  end
  return "accept"
end

-- advance handles one input and returns the status to finish it with.
-- Rejecting rolls the machine back, so a rejected input leaves no balance
-- changed, no nonce bumped and no output behind.
--
-- The shape is enforcement around dispatch: for an ordinary L2 transaction
-- the guest first checks everything it can without writing — signature,
-- nonce, fee cover — and rejects on any failure; then runs the application
-- semantics; and only on acceptance commits the account effects (nonce bump
-- and fee debit). The two commit-time failures (balance dipped below the
-- fee mid-input, accounts table at its load limit) reject after dispatch
-- may have written, which is safe exactly because rejection reverts the
-- whole machine, drive included (spec §10).
local function advance(raw)
  local deposit = parseDeposit(raw)
  local tx, sender, current
  if not deposit then
    local first = #raw > 0 and raw:byte(1) or nil
    if first and first >= 0x01 and first <= 0x04 then
      -- A typed-transaction envelope (EIP-2718). It carries a nonce this
      -- guest cannot verify — only the legacy sighash is implemented — and
      -- recording it unverified would exempt a whole transaction class
      -- from replay protection. Rejection is the conservative answer.
      return "reject"
    end
    tx = parseLegacyTx(raw)
    if tx then
      sender = recoverSender(tx)
      if not sender then return "reject" end -- signature does not verify
      local acct = drive:get_account(sender)
      current = acct and acct.nonce or 0
      -- Exact match, exactly Ethereum's rule: below is a replay, above is
      -- a gap nothing here would ever fill.
      if tx.nonce ~= current then return "reject" end
      -- The sender must be able to pay for the transaction before it runs.
      if not sub(balanceOf(hex(sender, 1, 20)), FEE) then return "reject" end
    end
    -- Anything else — bytes that do not parse as a transaction at all —
    -- falls through unenforced to the record-and-accept arm: it carries no
    -- nonce to check and cannot be meaningfully replayed, and rejecting it
    -- would roll the machine back over what may be a well-formed message
    -- for a later version of this app.
  end

  local status = dispatch(raw, deposit, deposit and deposit.data or (tx and tx.data) or nil)

  if status == "accept" and sender then
    -- Commit the accepted transaction's account effects. This is the only
    -- place nonces are ever written. The balance is re-read because
    -- dispatch may have moved it (a withdrawal naming the sender as payee).
    local left = sub(balanceOf(hex(sender, 1, 20)), FEE)
    if not left or not drive:set_account(sender, current + 1, hexdecode(pad(left))) then
      return "reject" -- fee no longer covered, or table full (spec §6.3)
    end
  end
  return status
end

-- ---------------------------------------------------------------- main loop
local status = "accept"
while true do
  -- The drive bytes must be current in machine state at every yield (spec
  -- §10): finish parks the machine, and the host reads the drive out of the
  -- parked state. This covers the boot-time format too — the first finish is
  -- the yield the snapshot is stored at, so the genesis root covers the
  -- formatted header. Syncing before a rejecting finish is harmless: the
  -- rollback reverts the drive either way.
  syncDrive()
  local req = rollup("finish", '{"status":"' .. status .. '"}')
  if req == "" then os.exit(1) end
  local raw = hexdecode(field(req, "payload") or "0x")
  if field(req, "request_type") == "inspect_state" then
    inspect(raw)
    status = "accept"
  else
    status = advance(raw)
  end
end
LUAEOF

exec lua5.4 /var/lib/bank.lua
