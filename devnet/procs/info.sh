#!/usr/bin/env bash
# The front page: what this devnet is serving and how to poke at it.
#
# It prints and exits — the pane keeps the text, which is the point. mprocs
# marks it DOWN, as it does every step that finishes rather than runs
# (genesis, l1-contracts, outputs).

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init info

L2_RPC_URL="http://127.0.0.1:${HTTP_ADDR##*:}"

cat <<INFO
op-cartesi devnet
=================

  L1 (anvil)        http://127.0.0.1:$L1_PORT        chain $L1_CHAIN_ID
  L2 (op-cartesi)   $L2_RPC_URL        chain $L2_CHAIN_ID
  op-node           http://127.0.0.1:$OPNODE_RPC_PORT
INFO
[ "$WITH_BATCHER" = "1" ] && echo "  op-batcher        http://127.0.0.1:$BATCHER_RPC_PORT"
[ "$WITH_PROPOSER" = "1" ] && [ "$WITH_CONTRACTS" = "1" ] &&
  echo "  op-proposer       http://127.0.0.1:$PROPOSER_RPC_PORT"
if [ "$WITH_VERIFIER" = "1" ]; then
  cat <<INFO
  verifier L2       http://127.0.0.1:${VERIFIER_HTTP_ADDR##*:}
  verifier op-node  http://127.0.0.1:$VERIFIER_OPNODE_RPC_PORT
INFO
fi

cat <<INFO

panes
-----
  machine   the emulator's console: Linux booting, then guest stdout
  guest     what the guest says about each transaction (its reports)
  genesis   runs once and stays down; so do l1-contracts and outputs
  outputs   does not autostart — press \`s\` to deploy the outputs suite

  up/down pick a pane, \`s\` start, \`x\` stop, \`r\` restart, \`q\` quit.
  Logs are also written to $LOG_DIR/<pane>.log.

from another terminal
---------------------
  cast block-number --rpc-url $L2_RPC_URL
  cast rpc cartesi_getOutputsRoot latest --rpc-url $L2_RPC_URL
  cast rpc optimism_syncStatus --rpc-url http://127.0.0.1:$OPNODE_RPC_PORT
INFO
if [ "$WITH_VERIFIER" = "1" ]; then
  cat <<INFO

  # the verifier sequences nothing, so it must agree block for block
  cast block 10 --rpc-url $L2_RPC_URL | grep -E 'hash|stateRoot'
  cast block 10 --rpc-url http://127.0.0.1:${VERIFIER_HTTP_ADDR##*:} | grep -E 'hash|stateRoot'
INFO
fi
if [ "$WITH_CONTRACTS" = "1" ]; then
  cat <<INFO

  bun devnet/deposit.ts <address> <wei>   # deposit through OptimismPortal
  bun devnet/balance.ts <address>         # what the guest's ledger says
INFO
fi
