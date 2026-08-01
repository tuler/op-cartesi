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
-- raw device. The Lua sources are taken out of the shell script they are
-- embedded in, so this tests the files that are actually appended to the
-- snapshot — including secp256k1.lua, whose ecrecover is pinned below
-- against vectors generated with go-ethereum (and verified against its
-- crypto.Ecrecover before being trusted; generator in the fixtures section).
--
-- One fidelity gap, deliberate: a real machine reverts the drive when an
-- input is rejected, and this harness does not. The guest decides every
-- rejection it can before writing (signature, nonce and fee cover are
-- checked up front; debits check the balance first), so the temp image still
-- ends byte-exact for every case below. Two commit-time rejections do exist
-- that would write first — the sender's balance dipping below the fee
-- mid-input, and the accounts table hitting its load limit — and they are
-- correct only because the real machine rolls back; the harness deliberately
-- does not construct them, and the drive checks at the bottom would catch a
-- branch that starts writing before rejecting in any case exercised here.

local script_dir = arg[0]:match("^(.*)/[^/]*$") or "."
local path = arg[1] or script_dir .. "/bank-app.sh"

-- The accounts-drive library, from the repo. The guest prepends
-- /var/lib/?.lua (where build-snapshot.sh installs it); that path does not
-- exist here, so require falls through to this one.
package.path = script_dir .. "/../accounts-drive/lua/?.lua;" .. package.path
local ad = require("accounts_drive")

-- ------------------------------------------------------- the guest sources
-- Both embedded files are extracted up front: secp256k1.lua is loaded
-- directly for the self-tests below, and preloaded so the bank app's own
-- require finds the exact same chunk.
local f = assert(io.open(path, "r"))
local shell = f:read("a")
f:close()
local src = shell:match("cat > /var/lib/bank%.lua <<'LUAEOF'\n(.-)\nLUAEOF")
assert(src, "could not find the guest's Lua in " .. path)
assert(src:find("__OWNER__", 1, true), "the guest has no __OWNER__ placeholder to substitute")
local secpSrc = shell:match("cat > /var/lib/secp256k1%.lua <<'SECPEOF'\n(.-)\nSECPEOF")
assert(secpSrc, "could not find the guest's secp256k1.lua in " .. path)
package.preload["secp256k1"] = assert(load(secpSrc, "@secp256k1.lua"))
local secp = require("secp256k1")

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

-- ---------------------------------------------------- ecrecover self-tests
-- The vectors were generated with go-ethereum (crypto.Sign over
-- keccak256 of a fixed message, keys 0x..01 and 0x..02) and verified
-- against crypto.Ecrecover before being hardcoded:
--
--   key := crypto.ToECDSA(common.LeftPadBytes([]byte{1}, 32))
--   sig, _ := crypto.Sign(hash, key)          // r ‖ s ‖ recid
--   pub, _ := crypto.Ecrecover(hash, sig)     // must reproduce the address
--
-- Pinning them here pins the whole pure-Lua stack — limb arithmetic, the
-- fold reduction, the Jacobian ladder — against the reference
-- implementation the rest of the chain uses.
local SENDER1 = "7e5f4552091a69125d5dfcb7b8c2659029395bdf" -- key 0x..01
local SENDER2 = "2b5ad5c4795c026514f8317c7a215e218dccd6cf" -- key 0x..02

local EC1_HASH = bin("579e07bd1251cddbd46e5a8e6f7b0e275d79867338dbdfc32840c7bc132760c2")
local EC1_R = bin("aac8c5e815a6519e4e167062d9a936732b3c8b26ffe8ec17f3d0b41b85cb9208")
local EC1_S = bin("4d0cf48a3f1d2c2d96cb86baf587ca52a00509c67f2a4cbb986f85a7ba0330ba")
local EC2_HASH = bin("fad4c625f3097b714bd9cf78161008da6c39f7ea5096ca0ca8cd8f5944286e49")
local EC2_R = bin("f32dbb74859c68ed76c2a2483ba6c0ebb6f6f6c3247617b6ea0aad7d378c2231")
local EC2_S = bin("28e4696913416bb0bc8f78e73527d4a1a60a7f1625fec3aa5ee23291e0ac8008")
local CURVE_N = bin("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141")

local r1 = secp.recover(EC1_HASH, 1, EC1_R, EC1_S)
check("ecrecover reproduces go-ethereum vector 1", r1 and tohex(r1) or nil, SENDER1)
local r2 = secp.recover(EC2_HASH, 0, EC2_R, EC2_S)
check("ecrecover reproduces go-ethereum vector 2", r2 and tohex(r2) or nil, SENDER2)
local flipped = secp.recover(EC1_HASH, 0, EC1_R, EC1_S)
check("a flipped recovery id yields a different signer",
  flipped ~= nil and tohex(flipped) ~= SENDER1, true)
check("ecrecover rejects r = 0",
  secp.recover(EC1_HASH, 1, ("\0"):rep(32), EC1_S), nil)
check("ecrecover rejects s = n",
  secp.recover(EC1_HASH, 1, EC1_R, CURVE_N), nil)
check("ecrecover rejects recovery ids outside {0,1}",
  secp.recover(EC1_HASH, 2, EC1_R, EC1_S), nil)

-- Pure-Lua ecrecover is the expensive part of guest-side enforcement:
-- measure it here so a cost regression is at least visible. The guest pays
-- this again through an emulated RISC-V CPU — expect one to two orders of
-- magnitude more inside the machine, which is real cycle cost per L2
-- transaction (MaxCyclesPerInput bounds it; see secp256k1.lua's header).
do
  local reps = 3
  local t0 = os.clock()
  for _ = 1, reps do secp.recover(EC1_HASH, 1, EC1_R, EC1_S) end
  print(string.format("info ecrecover: ~%.0f ms per recovery on this host (pure Lua)",
    (os.clock() - t0) / reps * 1000))
end

-- The temp file standing in for /dev/pmem1: 1 MiB of zeros, exactly what
-- build-snapshot.sh declares, so the guest's boot-time format runs here too.
local DRIVE_LENGTH = 1048576
local drivePath = os.tmpname()
do
  local df = assert(io.open(drivePath, "wb"))
  df:write(("\0"):rep(DRIVE_LENGTH))
  df:close()
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

-- Signed legacy transactions, generated with go-ethereum (chain id 901,
-- EIP-155 except where noted; gas price 1 gwei, gas 1e6, to = 0x0, value 0)
-- and checked against types.Sender before being hardcoded:
--
--   tx, _ := types.SignNewTx(key, types.NewEIP155Signer(big.NewInt(901)),
--     &types.LegacyTx{Nonce: n, GasPrice: ..., Gas: ..., To: &zero, Data: d})
--   raw, _ := tx.MarshalBinary()
--
-- SENDER1 (key 0x..01) carries the withdrawal plan at nonces 0,1,1,2,2,3;
-- SENDER2 (key 0x..02) signs one pre-EIP-155 (homestead, v 28) transaction
-- at nonce 0 and an EIP-155 one at nonce 1. The calldata bytes are the same
-- withdrawal payloads the pre-enforcement tests used.
local TX_WITHDRAW_TOKEN = bin( -- key 1, nonce 0: "t" ‖ TOKEN ‖ USER ‖ 0x40
  "f8b080843b9aca00830f424094000000000000000000000000000000000000000080b84974dddddddddddddddddddddddddddddddddddddddd"
  .. "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb0000000000000000000000000000000000000000000000000000000000000040"
  .. "82072da09885fcfbb28438c59e4b5aceade8a5d32b67835fe661152372e77bf4ffd4088ba025a370041d62af30e8cbd27756f27ce70825267ba72e38ab5ce418e54a63f3ce")
local TX_WITHDRAW_TOKEN_OVER = bin( -- key 1, nonce 1: amount 0xff
  "f8b001843b9aca00830f424094000000000000000000000000000000000000000080b84974dddddddddddddddddddddddddddddddddddddddd"
  .. "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb00000000000000000000000000000000000000000000000000000000000000ff"
  .. "82072da0e12c6d280c1aed814c1e04d1041c8e9fdeacd1e3ae465e579c23d09328ca4a66a055d8a77d73b47f559870595b159be664a84578bdb7cc52dc4a56e96e583da61d")
local TX_WITHDRAW_ETHER = bin( -- key 1, nonce 1: "w" ‖ USER ‖ 0x04
  "f89b01843b9aca00830f424094000000000000000000000000000000000000000080b577bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  .. "0000000000000000000000000000000000000000000000000000000000000004"
  .. "82072da082698c552bf812ab4550a91394448f2fd3f7a7204e4bf95c37181fd98635fcf7a00873e8ddaa32fbd81ac10a6bfa34851b46357296b0ba44aea141a3b9c2c7b071")
local TX_WITHDRAW_ETHER_OVER = bin( -- key 1, nonce 2: "w" ‖ USER ‖ 0x09
  "f89b02843b9aca00830f424094000000000000000000000000000000000000000080b577bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  .. "0000000000000000000000000000000000000000000000000000000000000009"
  .. "82072da03470845e8ef81c3d0b33e453fe1149fe5aad9bfb5795958021d57755f0d4fad9a05f77cd72b43b3be43760f4f97e2f136e7386ee1ccbcae0c2234725c0328d8b13")
local TX_UNKNOWN = bin( -- key 1, nonce 2: unknown tag 0x99
  "f86702843b9aca00830f42409400000000000000000000000000000000000000008081"
  .. "9982072ea0e85404ef040f9163914b80ddda931066a8c5922f09bbfd786366de978236d761a0282c750903b92b2a4d08cd2a810b35121f595824bd28a93651456f828011f54d")
local TX_WRONG_NONCE = bin( -- key 1, nonce 99: the guest's account says 3
  "f86763843b9aca00830f42409400000000000000000000000000000000000000008081"
  .. "9982072ea082e16edc56e2bd87bcf4f22799df7d36a41f7676c1668eb4066de7b8d8eb8b10a01e1c05b13d34eecf43cd82707f05dc804de0db57d6208965a293e57c5e789e75")
local TX_PRE155 = bin( -- key 2, nonce 0, homestead signature (v 28)
  "f86580843b9aca00830f42409400000000000000000000000000000000000000008081"
  .. "981ca0ada1ac1ea99e3b164c571278d125d8689fc4ca9bfa63029bc369a190e2246c20a05008bb88ac061f25b3380fda32216b32e534c06c3d3292a0be7c4108cb7d4464")
local TX_FEE = bin( -- key 1, nonce 3: sent after the owner sets a fee
  "f86703843b9aca00830f42409400000000000000000000000000000000000000008081"
  .. "9982072ea019f05450e4f4c9affda492c32b7b0c92ea77b8ac1112ea3d55fd5e8f5e5c697ea048310f081b136ff2b9b3ee3449bd15a75140c3f5b357d122da0abd96e38c87fe")
local TX_SENDER2_FEE = bin( -- key 2, nonce 1: sender2 holds no ether
  "f86701843b9aca00830f42409400000000000000000000000000000000000000008081"
  .. "9782072da09cd4803d14a557fa3d1d9e3c4491657093497254b6aa3d78563821d26db125cda04f98698610de189ad149e298cca65de6bbe8489583e4da5ce4eb019111a923d9")

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

-- A legacy-shaped transaction with a junk signature (r and s are repeated
-- bytes, not a signature of anything). Before enforcement this was how all
-- test payloads travelled; now it exists to pin that such a transaction is
-- rejected rather than executed.
local function junkSigned(data)
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

local function run(source)
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

  local chunk = assert(load(source, "@bank.lua"))
  local ok, err = pcall(chunk)

  io.open, io.popen, os.exit, os.execute = realOpen, realPopen, realExit, realExecute
  if not ok and err ~= "guest exited" then error(err, 0) end
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

-- The withdrawal plan, now as properly signed transactions from SENDER1:
-- enforcement recovers the signer and walks its nonce 0 → 3 across the four
-- requests (rejected inputs do not bump it).
local WITHDRAW_TOKEN = step()
advance(TX_WITHDRAW_TOKEN)
local WITHDRAW_TOKEN_OVER = step()
advance(TX_WITHDRAW_TOKEN_OVER)
local WITHDRAW_ETHER = step()
advance(TX_WITHDRAW_ETHER)
local WITHDRAW_ETHER_OVER = step()
advance(TX_WITHDRAW_ETHER_OVER)

local UNKNOWN = step()
advance(TX_UNKNOWN)

-- The replay-protection headline: the exact raw bytes of an already-included
-- transaction, resubmitted. Its nonce (1) is behind the account (3) now, so
-- the guest must reject it — before enforcement this input would have
-- withdrawn USER's ether a second time.
local REPLAY = step()
advance(TX_WITHDRAW_ETHER)

-- A nonce ahead of the account is no better than one behind it.
local WRONG_NONCE = step()
advance(TX_WRONG_NONCE)

-- The pre-EIP-155 signature form (v = 28, sighash over six items) must
-- recover too: SENDER2's first transaction, minting its nonce record while
-- the fee is still zero.
local PRE155 = step()
advance(TX_PRE155)

-- A structurally valid transaction whose r and s sign nothing. Before
-- enforcement this shape executed withdrawals; now it must not execute
-- anything on anyone's word.
local BAD_SIG = step()
advance(junkSigned(bin("99")))

-- A typed-transaction envelope (EIP-2718). The guest cannot verify its
-- nonce (only the legacy sighash is implemented), so recording it would
-- exempt a whole transaction class from replay protection: reject.
local TYPED = step()
advance(bin("02c0"))

-- Fund SENDER1 through the ether portal (deposits are exempt from
-- enforcement and fees), so the fee debit below has something to debit.
local FUND_SENDER1 = step()
advance(deposit(ETHER_PORTAL_ALIAS, ETHER, "00", bin(SENDER1 .. string.rep("0", 62) .. "0a")))

-- The owner prices transactions: "f" ‖ uint256, here 3 wei flat.
local FEE_SET = step()
advance(deposit(OWNER, ETHER, "00", bin("66" .. string.rep("0", 63) .. "3")))

-- The same bytes from anyone else must not reprice anything. (If this did
-- configure, the fee would jump to 0x63 and TX_FEE below — whose sender
-- holds 10 wei — would be rejected.)
local FEE_IMPOSTOR = step()
advance(deposit(NOT_OWNER, ETHER, "00", bin("66" .. string.rep("0", 62) .. "63")))

-- SENDER1 pays the fee: nonce 3 → 4, balance 10 → 7.
local FEE_PAID = step()
advance(TX_FEE)

-- SENDER2 cannot: correct nonce, valid signature, zero balance. Rejected
-- before anything runs — the fee is what §5.7 charges for the record.
local FEE_SHORT = step()
advance(TX_SENDER2_FEE)

-- A transaction whose RLP claims more bytes than it has. Nothing here should
-- credit anything, and — the point of the case — nothing should throw: an
-- error in the guest halts the machine, and a halted machine is a halted
-- chain, so a malformed input would be a denial of service anyone could send.
local TRUNCATED = step()
advance(bin("f8ff01"))
local TRUNCATED_DEPOSIT = step()
advance(bin("7ef8ff01"))

-- ---------------------------------------------------------------------- run
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

check("an unknown payload from a valid sender is recorded, not rejected",
  statusOf(UNKNOWN), "accept")
check("an unknown payload is recorded whole", payloadAt(UNKNOWN, "report"), tohex(TX_UNKNOWN))
check("an unknown payload emits nothing provable", payloadAt(UNKNOWN, "notice"), nil)

check("a replayed transaction is rejected", statusOf(REPLAY), "reject")
check("a replayed transaction emits nothing", outputsAt(REPLAY)[1], nil)
check("a future nonce is rejected too", statusOf(WRONG_NONCE), "reject")
check("a pre-EIP-155 signature recovers and is accepted", statusOf(PRE155), "accept")
check("a junk signature is rejected, not executed", statusOf(BAD_SIG), "reject")
check("a junk signature emits nothing", outputsAt(BAD_SIG)[1], nil)
check("a typed transaction the guest cannot verify is rejected",
  statusOf(TYPED), "reject")

check("the owner setting the fee is provable",
  payloadAt(FEE_SET, "notice"), string.rep("0", 63) .. "3")
check("a fee setting from anyone else is not one",
  payloadAt(FEE_IMPOSTOR, "notice"), nil)
check("a funded sender pays the fee and is accepted", statusOf(FEE_PAID), "accept")
check("a sender who cannot cover the fee is rejected", statusOf(FEE_SHORT), "reject")
check("a fee-rejected transaction emits nothing", outputsAt(FEE_SHORT)[1], nil)

check("a truncated transaction does not halt the machine", statusOf(TRUNCATED), "accept")
check("a truncated deposit does not halt the machine", statusOf(TRUNCATED_DEPOSIT), "accept")

-- ------------------------------------------------------- the drive itself
-- The reports above show what the guest *said*; the point of the accounts
-- drive is what an outside reader *finds*. Open the image with the library
-- directly — the same read path the host's eth_getBalance takes — and check
-- the balances and nonces really are drive bytes, not Lua state that died
-- with the run. Expected: USER holds ether 2 (portal) + 3 (OP deposit) − 4
-- (withdrawal) = 1 at nonce 0 — withdrawals name their payee, and nonces
-- belong to senders; SENDER1 walked nonce 0 → 4 over five accepted
-- transactions and paid one 3-wei fee out of 10 deposited; SENDER2 minted
-- its record while the fee was still zero (the §5.7 freebie this devnet's
-- zero default deliberately allows) and was refused once the fee was real.
local dfs = assert(ad.file_store(drivePath))
local d = assert(ad.open(dfs), "the guest never formatted the drive")

local acct = d:get_account(bin(USER))
check("the ether balance is on the drive",
  acct and tohex(acct.balance) or nil, string.rep("0", 63) .. "1")
check("a payee's nonce is untouched",
  acct and acct.nonce or nil, 0)

local s1 = d:get_account(bin(SENDER1))
check("the sender's nonce is enforced and bumped on the drive",
  s1 and s1.nonce or nil, 4)
check("the fee was debited from the sender's drive balance",
  s1 and tohex(s1.balance) or nil, string.rep("0", 63) .. "7")
local s2 = d:get_account(bin(SENDER2))
check("a pre-EIP-155 sender's nonce is on the drive too",
  s2 and s2.nonce or nil, 1)
check("the fee-refused sender's balance never went negative",
  s2 and tohex(s2.balance) or nil, string.rep("0", 64))

local tokenID = d:token_by_address(bin(TOKEN))
check("the token was registered first-seen with id 0", tokenID, 0)
check("the token registered at width 32",
  tokenID and d:tokens()[tokenID + 1].width or nil, 32)
check("the token balance is on the drive",
  tokenID and tohex(d:get_token_balance(bin(USER), tokenID)) or nil,
  string.rep("0", 62) .. "24")

check("no other account records exist", d:live_count(), 3)
check("one sparse holding exists", d:sparse_live_count(), 1)
check("an absent account is absent, not zero-filled", d:get_account(bin(NOT_OWNER)), nil)

dfs:close()
os.remove(drivePath)

if failures > 0 then
  print(failures .. " failed")
  os.exit(1)
end
print("all " .. #statuses - 1 .. " requests handled")
