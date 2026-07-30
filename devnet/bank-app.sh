#!/bin/sh
# Bank app: the guest side of a deposit.
#
# It decodes each input as an Ethereum transaction, and for an OP deposit
# (type 0x7e) credits the minted amount to the recipient. The ledger it keeps
# lives entirely inside the machine, so the balances are part of the state the
# machine's Merkle root commits to — which is the whole point: a deposit is
# credited by the execution layer, not by the shim around it.
#
# Balances are readable through eth_call, which the shim answers with the
# machine's inspect protocol: call the chain with a 20-byte address and the
# reply is that address's balance as a uint256.
#
# Written in Lua because the guest-tools rootfs ships lua5.4, and RLP wants
# real arithmetic rather than shell.

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

-- ---------------------------------------------------------------------- RLP
-- decode returns the payload span [from, to] of the item starting at i, the
-- index just past it, and whether it is a list. Spans are used rather than
-- copies so nested items can be walked without slicing the whole string.
local function decode(b, i)
  local prefix = b:byte(i)
  local function long(lenlen, base)
    local n = 0
    for k = i + 1, i + lenlen do n = n * 256 + b:byte(k) end
    return i + 1 + lenlen, i + lenlen + n, i + 1 + lenlen + n, base
  end
  if prefix < 0x80 then
    return i, i, i + 1, false
  elseif prefix <= 0xb7 then
    local n = prefix - 0x80
    return i + 1, i + n, i + 1 + n, false
  elseif prefix <= 0xbf then
    return long(prefix - 0xb7, false)
  elseif prefix <= 0xf7 then
    local n = prefix - 0xc0
    return i + 1, i + n, i + 1 + n, true
  else
    return long(prefix - 0xf7, true)
  end
end

local function hex(b, from, to)
  return (b:sub(from, to):gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

-- ------------------------------------------------------------------ deposits
-- An OP deposit is 0x7e followed by
--   rlp[sourceHash, from, to, mint, value, gas, isSystemTx, data]
-- op-node builds it from the L1 TransactionDeposited log; here it is taken
-- apart again.
local function parseDeposit(raw)
  if #raw < 2 or raw:byte(1) ~= 0x7e then return nil end
  local from, _, _, isList = decode(raw, 2)
  if not isList then return nil end
  local fields, i = {}, from
  for _ = 1, 8 do
    local f, t, nxt = decode(raw, i)
    fields[#fields + 1] = hex(raw, f, t)
    i = nxt
  end
  return { sourceHash = fields[1], from = fields[2], to = fields[3], mint = fields[4] }
end

-- ------------------------------------------------------------------- ledger
local balances = {}

local function balanceOf(addr)
  return balances[addr] or ZERO
end

-- Crediting the recipient rather than the sender collapses OP's two-step
-- "mint to `from`, then transfer `value` to `to`" into the one step this guest
-- can express: it has no transfers, only a ledger. For an ETH deposit the two
-- agree, since mint equals value and `to` is the named recipient.
local function credit(addr, amount)
  local updated = add(balanceOf(addr), amount)
  balances[addr] = updated
  return updated
end

local function hexdecode(s)
  return (s:gsub("^0x", ""):gsub("%x%x", function(cc) return string.char(tonumber(cc, 16)) end))
end

-- ---------------------------------------------------------------- main loop
local status = "accept"
while true do
  local req = rollup("finish", '{"status":"' .. status .. '"}')
  if req == "" then os.exit(1) end
  local raw = hexdecode(field(req, "payload") or "0x")
  status = "accept"

  if field(req, "request_type") == "inspect_state" then
    -- eth_call: a 20-byte address in, that address's balance out.
    rollup("report", '{"payload":"0x' .. balanceOf(hex(raw, 1, #raw)) .. '"}')
  else
    local d = parseDeposit(raw)
    if d then
      local updated = credit(d.to, d.mint)
      -- The notice is the provable half: it enters the outputs tree and so
      -- the block's withdrawals root, which is what a verifier re-derives.
      rollup("notice", '{"payload":"0x' .. pad(d.to) .. pad(d.mint) .. updated .. '"}')
      rollup("report", '{"payload":"0x' .. pad(d.to) .. updated .. '"}')
    else
      -- Not a deposit. This guest has no account model for signed
      -- transactions yet, so it records the input and moves on rather than
      -- rejecting, which would roll the machine back.
      rollup("report", '{"payload":"0x' .. hex(raw, 1, #raw) .. '"}')
    end
  end
end
LUAEOF

exec lua5.4 /var/lib/bank.lua
