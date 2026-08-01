#!/usr/bin/env bash
# Sends one L2 transaction carrying an arbitrary payload, which is how a user
# talks to the guest: the chain wraps the whole signed transaction in an
# EvmAdvance envelope and the guest reads the payload out of it.
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${SENDER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
PAYLOAD="${1:?usage: send-l2-tx.sh <0x payload>}"
SENDER=$(cast wallet address --private-key "$SENDER_KEY")
# The nonce comes from the chain: eth_getTransactionCount reads it out of the
# guest's accounts drive (docs/ACCOUNTS.md). Honesty note — the chain still
# does not *enforce* nonces, so any value would be accepted and this returns 0
# until the guest starts bumping them (roadmap v1); asking for the real one
# exercises the new RPC and stops mattering-by-accident the day enforcement
# lands. Gas stays explicit: there is still no fee market, so cast must not
# ask for eth_feeHistory.
NONCE=$(cast rpc eth_getTransactionCount "$SENDER" latest --rpc-url "$L2_RPC" | tr -d '"')
RAW=$(cast mktx --private-key "$SENDER_KEY" --legacy \
  --nonce "$((NONCE))" --gas-limit 1000000 --gas-price 1000000000 \
  --chain "$L2_CHAIN_ID" 0x0000000000000000000000000000000000000000 "$PAYLOAD")
cast rpc eth_sendRawTransaction "$RAW" --rpc-url "$L2_RPC" | tr -d '"'
