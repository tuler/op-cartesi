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
    -- The nonce rides along unchanged. This guest does not bump nonces yet:
    -- enforcement (and with it sender recovery) is ACCOUNTS.md roadmap v1.
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

-- -------------------------------------------------------------- withdrawals
-- A withdrawal request is carried in the calldata of an ordinary L2
-- transaction:
--
--   "w" ‖ recipient(20) ‖ amount(32)              ether
--   "t" ‖ token(20) ‖ recipient(20) ‖ amount(32)  tokens
--
-- It is not an Ethereum call: this guest has no account model, so there is
-- nothing to authorise against beyond the balance itself, which is enough to
-- exercise the path end to end.
local WITHDRAW, WITHDRAW_TOKEN = 0x77, 0x74

-- ERC20.transfer(address,uint256).
local TRANSFER = "a9059cbb"

-- callData pulls the data field out of a legacy transaction, which is
--   rlp[nonce, gasPrice, gasLimit, to, value, data, v, r, s]
-- The envelope hands the guest the whole signed transaction, not its calldata:
-- deciding what the bytes mean is the execution layer's job, and here that is
-- the guest.
local function callData(raw)
  local from, _, _, isList = decode(raw, 1)
  if not from or not isList then return nil end
  local i = from
  for f = 1, 6 do
    local a, b, nxt = decode(raw, i)
    if not a then return nil end
    if f == 6 then return raw:sub(a, b) end
    i = nxt
  end
  return nil
end

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

-- advance handles one input and returns the status to finish it with.
-- Rejecting rolls the machine back, so a rejected input leaves no balance
-- changed and no output behind.
local function advance(raw)
  local deposit = parseDeposit(raw)
  local data = deposit and deposit.data or callData(raw)
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
