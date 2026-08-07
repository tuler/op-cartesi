#!/usr/bin/env bash
# anvil, the devnet L1.
#
# Deliberately not --silent: this pane is where you watch L1 accept the
# batcher's calldata and the proposer's games.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init l1

devnet_say "anvil (L1, chain $L1_CHAIN_ID) on $L1_BIND_ADDR:$L1_PORT"
exec anvil \
  --host "$L1_BIND_ADDR" \
  --port "$L1_PORT" \
  --chain-id "$L1_CHAIN_ID" \
  --block-time "$L1_BLOCK_TIME"
