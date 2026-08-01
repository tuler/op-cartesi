#!/usr/bin/env bash
# Sends one L2 transaction carrying an arbitrary payload, which is how a user
# talks to the guest: the chain wraps the whole signed transaction in an
# EvmAdvance envelope and the guest reads the payload out of it.
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${SENDER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
PAYLOAD="${1:?usage: send-l2-tx.sh <0x payload>}"
# Plain `cast send` works: cast fetches the nonce itself through
# eth_getTransactionCount, which reads the guest's accounts drive
# (docs/ACCOUNTS.md), and waits for the receipt the shim already serves.
# Since roadmap v1 that nonce is *enforced* — the guest recovers the sender,
# rejects any other nonce, and bumps the record on acceptance, so a replayed
# transaction no longer executes twice. Gas price and limit stay explicit
# flags because eth_gasPrice/eth_feeHistory and eth_estimateGas are still
# unserved, and the flags keep cast from asking.
cast send --rpc-url "$L2_RPC" --private-key "$SENDER_KEY" --legacy \
  --gas-price 1000000000 --gas-limit 1000000 \
  0x0000000000000000000000000000000000000000 "$PAYLOAD"
