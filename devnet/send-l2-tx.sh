#!/usr/bin/env bash
# Sends one L2 transaction carrying an arbitrary payload, which is how a user
# talks to the guest: the chain wraps the whole signed transaction in an
# EvmAdvance envelope and the guest reads the payload out of it.
source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${SENDER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
PAYLOAD="${1:?usage: send-l2-tx.sh <0x payload>}"
# Plain `cast send` works with no gas flags: cast fetches the nonce through
# eth_getTransactionCount, which reads the guest's accounts drive
# (docs/ACCOUNTS.md), the gas price through eth_gasPrice (the constant header
# base fee — there is no fee market), and the gas limit through
# eth_estimateGas (the per-input cycle budget expressed as gas — an upper
# bound the chain accepts, not a measurement), then waits for the receipt the
# shim serves. Since roadmap v1 the nonce is *enforced* — the guest recovers
# the sender, rejects any other nonce, and bumps the record on acceptance, so
# a replayed transaction no longer executes twice.
# --legacy stays: without it cast builds an EIP-1559 typed transaction, and
# the guest parses only legacy ones (its signature check implements only the
# legacy sighash); 1559 transaction support is part of the fee-market work.
cast send --rpc-url "$L2_RPC" --private-key "$SENDER_KEY" --legacy \
  0x0000000000000000000000000000000000000000 "$PAYLOAD"
