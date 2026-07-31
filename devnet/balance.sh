#!/usr/bin/env bash
# Reads a balance out of the guest's ledger.
#
#   ./devnet/balance.sh <address> [token]
#
# `cast rpc eth_call` rather than `cast call`: the latter reads the caller's
# nonce first, and this chain serves no `eth_getTransactionCount` because its
# guest has no account model to take a nonce from.
#
# The call data is the query, not an ABI encoding: 20 bytes of address for
# ether, or token ‖ address for a token. The shim answers by running the
# machine's inspect protocol on a fork it then discards, so what comes back is
# the guest's own state, not the shim's idea of it.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"

WHO="${1:?usage: balance.sh <address> [token]}"
TOKEN="${2:-}"
QUERY="0x${TOKEN#0x}${WHO#0x}"

cast rpc eth_call \
  "{\"to\":\"0x0000000000000000000000000000000000000000\",\"data\":\"$QUERY\"}" latest \
  --rpc-url "$L2_RPC" | tr -d '"'
