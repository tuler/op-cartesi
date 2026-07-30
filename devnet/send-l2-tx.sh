#!/usr/bin/env bash
# Sends one L2 transaction carrying an arbitrary payload, which is how a user
# talks to the guest: the chain wraps the whole signed transaction in an
# EvmAdvance envelope and the guest reads the payload out of it.
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${SENDER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
PAYLOAD="${1:?usage: send-l2-tx.sh <0x payload>}"
# The chain has no account model, so nonce and gas are formalities; they only
# have to make a well-formed signed transaction.
# Legacy, and every field given explicitly: cast otherwise asks for
# eth_feeHistory and eth_getTransactionCount, and this chain serves neither —
# it has no fee market and no account model to take a nonce from.
RAW=$(cast mktx --private-key "$SENDER_KEY" --legacy \
  --nonce "$(date +%s | tail -c 6)" --gas-limit 1000000 --gas-price 1000000000 \
  --chain "$L2_CHAIN_ID" 0x0000000000000000000000000000000000000000 "$PAYLOAD")
cast rpc eth_sendRawTransaction "$RAW" --rpc-url "$L2_RPC" | tr -d '"'
