#!/usr/bin/env bash
# Sends an L1 deposit that op-node derives into an L2 deposit transaction.
#
# A real chain gets these from OptimismPortal.depositTransaction. This devnet
# has no contract suite, so it installs a minimal emitter at the rollup
# config's deposit contract address: op-node's derivation reads the
# TransactionDeposited log, not the contract that produced it, so an emitter
# that logs the canonical event is indistinguishable from the portal as far as
# the chain is concerned.
#
#   ./devnet/deposit.sh <recipient> [wei]
#
# Requires the devnet to be running (./devnet/start-devnet.sh).

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
# Anvil account 1, kept off account 0 so deposits never race the batcher's
# nonce.
: "${DEPOSITOR_KEY:=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d}"
: "${DEPOSIT_GAS_LIMIT:=1000000}"

TO="${1:?usage: deposit.sh <recipient address> [wei]}"
AMOUNT="${2:-1000000000000000000}"

# Runtime bytecode for the emitter. It logs TransactionDeposited with
# from = msg.sender, to = calldata word 0, version = 0, and data = the rest of
# the calldata, which is the caller's already-ABI-encoded opaqueData. Keeping
# the encoding on this side is what keeps the bytecode small enough to audit
# by eye:
#
#   CALLDATASIZE PUSH1 20 SWAP1 SUB PUSH1 20 PUSH1 00 CALLDATACOPY
#       -- memory[0:] = calldata[32:]
#   PUSH1 00                     -- topic4: version 0
#   PUSH1 00 CALLDATALOAD        -- topic3: to
#   CALLER                       -- topic2: from
#   PUSH32 <TransactionDeposited>-- topic1: event signature
#   CALLDATASIZE PUSH1 20 SWAP1 SUB PUSH1 00 LOG4 STOP
TOPIC0=b3813568d9991fc951961fcb4c784893574240a28925604d09fc577c55bb7c32
EMITTER_CODE="0x366020900360206000376000600035337f${TOPIC0}36602090036000a400"

# With the contract suite deployed, the deposit goes through the real
# OptimismPortal, which is the path a user would take. The emitter below is
# only for the contract-less devnet (WITH_CONTRACTS=0), where there is no
# portal to call.
if [ "$(cast code "$DEPOSIT_CONTRACT_ADDRESS" --rpc-url "$L1_RPC")" != "0x" ]; then
  echo "depositing $AMOUNT wei to $TO via OptimismPortal at $DEPOSIT_CONTRACT_ADDRESS" >&2
  cast send "$DEPOSIT_CONTRACT_ADDRESS" \
    "depositTransaction(address,uint256,uint64,bool,bytes)" \
    "$TO" "$AMOUNT" "$DEPOSIT_GAS_LIMIT" false 0x \
    --value "$AMOUNT" --private-key "$DEPOSITOR_KEY" --rpc-url "$L1_RPC" --json \
    | python3 -c 'import sys,json;r=json.load(sys.stdin);print("L1 tx",r["transactionHash"],"in block",int(r["blockNumber"],16),"status",r["status"])'
  exit 0
fi

echo "no contract at $DEPOSIT_CONTRACT_ADDRESS; installing the minimal emitter" >&2
cast rpc anvil_setCode "$DEPOSIT_CONTRACT_ADDRESS" "$EMITTER_CODE" --rpc-url "$L1_RPC" > /dev/null

# opaqueData for version 0, as OptimismPortal packs it:
#   mint (uint256) ++ value (uint256) ++ gasLimit (uint64) ++ isCreation (bool)
#   ++ data
MINT=$(cast to-uint256 "$AMOUNT")
VALUE=$MINT
GAS=$(printf '%016x' "$DEPOSIT_GAS_LIMIT")
OPAQUE="${MINT#0x}${VALUE#0x}${GAS}00"

# The log's data field is the ABI encoding of that bytes value, since
# opaqueData is a non-indexed event parameter.
CALLDATA="$(cast to-uint256 "$TO")$(cast abi-encode 'f(bytes)' "0x$OPAQUE" | sed 's/^0x//')"

echo "depositing $AMOUNT wei to $TO" >&2
cast send "$DEPOSIT_CONTRACT_ADDRESS" "$CALLDATA" \
  --private-key "$DEPOSITOR_KEY" --rpc-url "$L1_RPC" --json \
  | python3 -c 'import sys,json;r=json.load(sys.stdin);print("L1 tx",r["transactionHash"],"in block",int(r["blockNumber"],16),"status",r["status"])'
