-- Tests for the guest in bank-app.sh, run outside the machine.
--
--   lua5.4 devnet/test-guest.lua [devnet/bank-app.sh]
--
-- The guest is consensus code: what it credits and what it refuses to credit
-- is the chain's state transition, and getting it wrong is not a bug that
-- shows up as a failed request — it is a bug that shows up as a fork, or as
-- an unbacked withdrawal. It is also the one part of this repo that never
-- runs on the host, which is exactly why it is worth being able to run here.
--
-- The guest talks to the machine through `io.open` on a scratch file,
-- `io.popen` on the `rollup` tool, and — since the ledger moved onto the
-- accounts drive — `io.open` on /dev/pmem1 plus `os.execute("sync")`.
-- Stubbing those is enough to drive it with whatever inputs a test wants and
-- read back what it emitted: the drive device is redirected to a temp file,
-- which the accounts-drive library reads and writes exactly as it would the
-- raw device. The Lua source is taken out of the shell script it is embedded
-- in, so this tests the file that is actually appended to the snapshot.
--
-- One fidelity gap, deliberate: a real machine reverts the drive when an
-- input is rejected, and this harness does not. The guest never writes the
-- drive before deciding to reject (debits check the balance first), so the
-- temp image still ends byte-exact — and the drive checks at the bottom
-- would catch a change that starts writing before rejecting.

local script_dir = arg[0]:match("^(.*)/[^/]*$") or "."
local path = arg[1] or script_dir .. "/bank-app.sh"

-- The accounts-drive library, from the repo. The guest prepends
-- /var/lib/?.lua (where build-snapshot.sh installs it); that path does not
-- exist here, so require falls through to this one.
package.path = script_dir .. "/../accounts-drive/lua/?.lua;" .. package.path
local ad = require("accounts_drive")

-- The temp file standing in for /dev/pmem1: 1 MiB of zeros, exactly what
-- build-snapshot.sh declares, so the guest's boot-time format runs here too.
local DRIVE_LENGTH = 1048576
local drivePath = os.tmpname()
do
  local f = assert(io.open(drivePath, "wb"))
  f:write(("\0"):rep(DRIVE_LENGTH))
  f:close()
end

-- ------------------------------------------------------------------ fixtures
-- The addresses are arbitrary except for the aliases, which come from OP's
-- AddressAliasHelper: alias(a) = a + 0x1111000000000000000000000000000000001111
-- truncated to 160 bits. They are written out rather than computed so that the
-- guest's own aliasing is checked against an independent answer — including
-- the wrap, which is what the ERC-20 portal below is chosen to exercise.
local OWNER = string.rep("aa", 20)
local NOT_OWNER = string.rep("cc", 20)
local ETHER_PORTAL = "1234567890123456789012345678901234567890"
local ETHER_PORTAL_ALIAS = "23455678901234567890123456789012345689a1"
local ERC20_PORTAL = string.rep("ff", 20)
local ERC20_PORTAL_ALIAS = "1111000000000000000000000000000000001110"
local IMPOSTOR = string.rep("ee", 20)
local TOKEN = string.rep("dd", 20)
local USER = string.rep("bb", 20)
local ETHER = string.rep("00", 20)

local ONE = string.rep("0", 63) .. "1"

-- ----------------------------------------------------------------- encoding
local function bin(hex)
  return (hex:gsub("%x%x", function(c) return string.char(tonumber(c, 16)) end))
end

local function tohex(s)
  return (s:gsub(".", function(c) return string.format("%02x", c:byte()) end))
end

local function trim(s)
  return (s:gsub("^%z+", ""))
end

local function rlpLen(n, offset)
  if n < 56 then return string.char(offset + n) end
  local bytes = ""
  while n > 0 do
    bytes = string.char(n % 256) .. bytes
    n = n // 256
  end
  return string.char(offset + 55 + #bytes) .. bytes
end

local function rlpStr(s)
  if #s == 1 and s:byte(1) < 0x80 then return s end
  return rlpLen(#s, 0x80) .. s
end

local function rlpList(items)
  local body = table.concat(items)
  return rlpLen(#body, 0xc0) .. body
end

-- An OP deposit, as op-node builds it from a TransactionDeposited log.
local function deposit(from, to, mint, data)
  return "\x7e" .. rlpList {
    rlpStr(bin(string.rep("00", 32))),
    rlpStr(bin(from)),
    rlpStr(bin(to)),
    rlpStr(trim(bin(mint))),
    rlpStr(trim(bin(mint))),
    rlpStr(bin("0186a0")),
    rlpStr(""),
    rlpStr(data),
  }
end

-- A signed legacy transaction, which is how a user's payload reaches the
-- guest: the envelope carries the whole transaction, not its calldata.
local function legacy(data)
  return rlpList {
    rlpStr(bin("01")), rlpStr(bin("3b9aca00")), rlpStr(bin("0f4240")),
    rlpStr(bin(string.rep("00", 20))), rlpStr(""), rlpStr(data),
    rlpStr(bin("25")), rlpStr(bin(string.rep("11", 32))), rlpStr(bin(string.rep("22", 32))),
  }
end

-- -------------------------------------------------------------- the harness
-- Requests the guest will be handed, in order, and everything it emits.
local pending, emitted, statuses = {}, {}, {}

local function advance(raw)
  pending[#pending + 1] = '{"request_type":"advance_state","payload":"0x' .. tohex(raw) .. '"}'
end

local function inspect(raw)
  pending[#pending + 1] = '{"request_type":"inspect_state","payload":"0x' .. tohex(raw) .. '"}'
end

local cursor = 1

local function handle(sub, body)
  if sub == "finish" then
    statuses[#statuses + 1] = body:match('"status":"([^"]*)"')
    local req = pending[cursor]
    cursor = cursor + 1
    return req or ""
  end
  emitted[#emitted + 1] = { kind = sub, body = body, at = cursor - 1 }
  return ""
end

local function run(src)
  local written
  local realOpen, realPopen, realExit, realExecute = io.open, io.popen, os.exit, os.execute
  io.open = function(p, mode)
    if p == "/var/lib/bank.in" then
      return { write = function(_, s) written = s end, close = function() end }
    end
    if p == "/dev/pmem1" then
      -- The accounts drive: a plain file here, the raw device in the guest.
      return realOpen(drivePath, mode)
    end
    return realOpen(p, mode)
  end
  io.popen = function(cmd)
    local out = handle(cmd:match("^rollup (%S+)"), written)
    return { read = function() return out end, close = function() end }
  end
  -- os.execute("sync") flushes the guest kernel's page cache; there is no
  -- guest kernel here, and the library's own sync() already flushed to the
  -- temp file, so it is a no-op.
  os.execute = function() return true end
  os.exit = function() error("guest exited", 0) end

  local chunk = assert(load(src, "@bank.lua"))
  local ok, err = pcall(chunk)

  io.open, io.popen, os.exit, os.execute = realOpen, realPopen, realExit, realExecute
  if not ok and err ~= "guest exited" then error(err, 0) end
end

-- ------------------------------------------------------------------ asserts
local failures = 0

local function check(name, got, want)
  if got == want then
    print("ok   " .. name)
  else
    failures = failures + 1
    print("FAIL " .. name)
    print("       got  " .. tostring(got))
    print("       want " .. tostring(want))
  end
end

-- outputsAt returns what the guest emitted while handling request n.
local function outputsAt(n)
  local out = {}
  for _, e in ipairs(emitted) do
    if e.at == n then out[#out + 1] = e end
  end
  return out
end

local function payloadAt(n, kind)
  for _, e in ipairs(outputsAt(n)) do
    if e.kind == kind then return e.body:match('"payload":"0x([^"]*)"') end
  end
  return nil
end

-- statusOf is the status the guest reported for request n, which it sends on
-- the *next* finish — an accepted input and a rejected one are told apart only
-- by what comes after them.
local function statusOf(n)
  return statuses[n + 1]
end

-- --------------------------------------------------------------------- plan
local n = 0
local function step() n = n + 1 return n end

local REGISTER_ETHER = step()
advance(deposit(OWNER, ETHER, "00", bin("70" .. "00" .. ETHER_PORTAL)))
local REGISTER_ERC20 = step()
advance(deposit(OWNER, ETHER, "00", bin("70" .. "01" .. ERC20_PORTAL)))

-- Not the owner: the same bytes from anyone else must not configure anything.
local REGISTER_IMPOSTOR = step()
advance(deposit(NOT_OWNER, ETHER, "00", bin("70" .. "01" .. IMPOSTOR)))

-- token(20) ‖ sender(20) ‖ value(32), which is Cartesi's InputEncoding.
local ERC20_DEPOSIT = step()
advance(deposit(ERC20_PORTAL_ALIAS, ETHER, "00", bin(TOKEN .. USER .. string.rep("0", 62) .. "64")))

-- The same payload from a contract that is not the portal. This is the case
-- that matters: crediting it would mint claims against tokens the application
-- really holds, and a voucher would pay them out.
local ERC20_IMPOSTOR = step()
advance(deposit(IMPOSTOR, ETHER, "00", bin(TOKEN .. USER .. string.rep("0", 62) .. "64")))

-- sender(20) ‖ value(32).
local ETHER_DEPOSIT = step()
advance(deposit(ETHER_PORTAL_ALIAS, ETHER, "00", bin(USER .. string.rep("0", 63) .. "2")))

-- OP's own ether deposit: no data, a mint, credited to `to`.
local OP_DEPOSIT = step()
advance(deposit(NOT_OWNER, USER, string.rep("0", 63) .. "3", ""))

local BALANCE_TOKEN = step()
inspect(bin(TOKEN .. USER))
local BALANCE_ETHER = step()
inspect(bin(USER))

-- "t" ‖ token ‖ recipient ‖ amount.
local WITHDRAW_TOKEN = step()
advance(legacy(bin("74" .. TOKEN .. USER .. string.rep("0", 62) .. "40")))
local WITHDRAW_TOKEN_OVER = step()
advance(legacy(bin("74" .. TOKEN .. USER .. string.rep("0", 62) .. "ff")))

-- "w" ‖ recipient ‖ amount.
local WITHDRAW_ETHER = step()
advance(legacy(bin("77" .. USER .. string.rep("0", 63) .. "4")))
local WITHDRAW_ETHER_OVER = step()
advance(legacy(bin("77" .. USER .. string.rep("0", 63) .. "9")))

local UNKNOWN = step()
local unknownTx = legacy(bin("99"))
advance(unknownTx)

-- A transaction whose RLP claims more bytes than it has. Nothing here should
-- credit anything, and — the point of the case — nothing should throw: an
-- error in the guest halts the machine, and a halted machine is a halted
-- chain, so a malformed input would be a denial of service anyone could send.
local TRUNCATED = step()
advance(bin("f8ff01"))
local TRUNCATED_DEPOSIT = step()
advance(bin("7ef8ff01"))

-- ---------------------------------------------------------------------- run
local f = assert(io.open(path, "r"))
local shell = f:read("a")
f:close()
local src = shell:match("cat > /var/lib/bank%.lua <<'LUAEOF'\n(.-)\nLUAEOF")
assert(src, "could not find the guest's Lua in " .. path)
assert(src:find("__OWNER__", 1, true), "the guest has no __OWNER__ placeholder to substitute")
run((src:gsub("__OWNER__", OWNER)))

-- ------------------------------------------------------------------- checks
check("registering the ether portal is provable",
  payloadAt(REGISTER_ETHER, "notice"),
  string.rep("0", 24) .. ETHER_PORTAL .. string.rep("0", 24) .. ETHER_PORTAL_ALIAS)
check("registering the erc20 portal aliases with a wrap",
  payloadAt(REGISTER_ERC20, "notice"),
  string.rep("0", 24) .. ERC20_PORTAL .. string.rep("0", 24) .. ERC20_PORTAL_ALIAS)
check("a registration from anyone else is not one",
  payloadAt(REGISTER_IMPOSTOR, "notice"), nil)

check("an erc20 deposit credits the L1 sender",
  payloadAt(ERC20_DEPOSIT, "report"), string.rep("0", 24) .. USER .. string.rep("0", 62) .. "64")
check("an erc20 deposit is provable",
  payloadAt(ERC20_DEPOSIT, "notice"),
  string.rep("0", 24) .. TOKEN .. string.rep("0", 24) .. USER
    .. string.rep("0", 62) .. "64" .. string.rep("0", 62) .. "64")
check("an unregistered sender credits nothing",
  payloadAt(ERC20_IMPOSTOR, "notice"), nil)

check("an ether portal deposit credits the L1 sender",
  payloadAt(ETHER_DEPOSIT, "report"), string.rep("0", 24) .. USER .. string.rep("0", 63) .. "2")
check("an OP deposit credits the recipient",
  payloadAt(OP_DEPOSIT, "report"), string.rep("0", 24) .. USER .. string.rep("0", 63) .. "5")

check("token balances are read by token ‖ address",
  payloadAt(BALANCE_TOKEN, "report"), string.rep("0", 62) .. "64")
check("ether balances are read by address alone",
  payloadAt(BALANCE_ETHER, "report"), string.rep("0", 63) .. "5")

check("a token withdrawal is a transfer voucher on the token",
  outputsAt(WITHDRAW_TOKEN)[1].body,
  '{"destination":"0x' .. TOKEN .. '","value":"0x' .. string.rep("0", 64)
    .. '","payload":"0xa9059cbb' .. string.rep("0", 24) .. USER
    .. string.rep("0", 62) .. '40"}')
check("a token withdrawal debits the ledger",
  payloadAt(WITHDRAW_TOKEN, "report"), string.rep("0", 24) .. USER .. string.rep("0", 62) .. "24")
check("an overdrawn token withdrawal is rejected",
  statusOf(WITHDRAW_TOKEN_OVER), "reject")
check("an overdrawn token withdrawal emits no voucher",
  outputsAt(WITHDRAW_TOKEN_OVER)[1].kind, "report")

check("an ether withdrawal pays the recipient directly",
  outputsAt(WITHDRAW_ETHER)[1].body,
  '{"destination":"0x' .. USER .. '","value":"0x' .. string.rep("0", 63) .. '4","payload":"0x"}')
check("an ether withdrawal debits the ledger",
  payloadAt(WITHDRAW_ETHER, "report"), string.rep("0", 24) .. USER .. string.rep("0", 63) .. "1")
check("an overdrawn ether withdrawal is rejected",
  statusOf(WITHDRAW_ETHER_OVER), "reject")

check("an unknown payload is recorded, not rejected", statusOf(UNKNOWN), "accept")
check("an unknown payload is recorded whole", payloadAt(UNKNOWN, "report"), tohex(unknownTx))
check("an unknown payload emits nothing provable", payloadAt(UNKNOWN, "notice"), nil)

check("a truncated transaction does not halt the machine", statusOf(TRUNCATED), "accept")
check("a truncated deposit does not halt the machine", statusOf(TRUNCATED_DEPOSIT), "accept")

-- ------------------------------------------------------- the drive itself
-- The reports above show what the guest *said*; the point of the accounts
-- drive is what an outside reader *finds*. Open the image with the library
-- directly — the same read path the host's eth_getBalance takes — and check
-- the credited balances really are drive bytes, not Lua state that died with
-- the run. Expected: ether 2 (portal) + 3 (OP deposit) − 4 (withdrawal) = 1,
-- with the overdraw rejected before any write; token 0x64 − 0x40 = 0x24.
local dfs = assert(ad.file_store(drivePath))
local d = assert(ad.open(dfs), "the guest never formatted the drive")

local acct = d:get_account(bin(USER))
check("the ether balance is on the drive",
  acct and tohex(acct.balance) or nil, string.rep("0", 63) .. "1")
check("the account nonce is untouched (no enforcement yet)",
  acct and acct.nonce or nil, 0)

local tokenID = d:token_by_address(bin(TOKEN))
check("the token was registered first-seen with id 0", tokenID, 0)
check("the token registered at width 32",
  tokenID and d:tokens()[tokenID + 1].width or nil, 32)
check("the token balance is on the drive",
  tokenID and tohex(d:get_token_balance(bin(USER), tokenID)) or nil,
  string.rep("0", 62) .. "24")

check("no other account records exist", d:live_count(), 1)
check("one sparse holding exists", d:sparse_live_count(), 1)
check("an absent account is absent, not zero-filled", d:get_account(bin(NOT_OWNER)), nil)

dfs:close()
os.remove(drivePath)

if failures > 0 then
  print(failures .. " failed")
  os.exit(1)
end
print("all " .. #statuses - 1 .. " requests handled")
