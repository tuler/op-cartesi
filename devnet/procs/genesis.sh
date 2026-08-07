#!/usr/bin/env bash
# Anchors the rollup to L1 and writes the config every other piece reads.
#
# Two outputs, both consensus-relevant:
#
#   devnet/chain-genesis.env  the L1 block the rollup starts after and the L2
#                             genesis timestamp derived from it. env.sh sources
#                             it, which is how the engine — a different
#                             process, started from a different pane — ends up
#                             on the same genesis as op-node.
#   devnet/rollup.json         what op-node derives the chain from.
#
# Generating rollup.json loads the snapshot, so this needs a machine server of
# its own; it is started and stopped here rather than shared with the node's,
# because a server holds exactly one machine.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init genesis

clear_ready genesis
wait_for_port "$L1_PORT" "anvil"

L1_RPC_URL="http://127.0.0.1:$L1_PORT"

if [ "$WITH_CONTRACTS" = "1" ]; then
  wait_ready l1-contracts "the L1 contract deployment"
  # The deploy wrote l1-addresses.env after this script sourced env.sh, so the
  # addresses and the anchor block have to be read again here.
  reload_addresses
else
  # No contracts, no deployment block: the rollup starts at L1 genesis, on the
  # placeholder addresses env.sh falls back to.
  L1_GENESIS_NUMBER=0
  L1_GENESIS_HASH=$(cast block 0 --rpc-url "$L1_RPC_URL" --json |
    python3 -c 'import sys,json;print(json.load(sys.stdin)["hash"])')
fi

# The L2 genesis timestamp is anchored to the L1 block the rollup starts after,
# so op-node's derivation clock and the engine's genesis agree.
GENESIS_TIMESTAMP=$(cast block "$L1_GENESIS_NUMBER" --rpc-url "$L1_RPC_URL" --json |
  python3 -c 'import sys,json;print(int(json.load(sys.stdin)["timestamp"],16))')

{
  echo "# Written by devnet/procs/genesis.sh. Sourced by env.sh."
  echo "# The L1 anchor of this rollup, and the L2 genesis timestamp derived"
  echo "# from it. Both are consensus-relevant: op-node and the engine must"
  echo "# agree on them or the engine serves a genesis op-node rejects."
  echo "L1_GENESIS_HASH=$L1_GENESIS_HASH"
  echo "L1_GENESIS_NUMBER=$L1_GENESIS_NUMBER"
  echo "GENESIS_TIMESTAMP=$GENESIS_TIMESTAMP"
} > "$CHAIN_GENESIS_FILE"
devnet_say "anchored at L1 block $L1_GENESIS_NUMBER ($L1_GENESIS_HASH), genesis timestamp $GENESIS_TIMESTAMP"

export MACHINE_SNAPSHOT="$SNAPSHOT_DIR"
devnet_say "booting a machine to read the genesis state root off the snapshot"
start_machine_server "$GENESIS_MACHINE_PORT"
trap 'kill_tree "$MACHINE_SERVER_PID"' EXIT

MACHINE_REMOTE="http://127.0.0.1:$GENESIS_MACHINE_PORT" "$DEVNET_DIR/generate-config.sh"

# The generator is done with it, and a booted machine is not cheap to leave
# sitting around.
kill_tree "$MACHINE_SERVER_PID"
trap - EXIT

mark_ready genesis
devnet_say "wrote $ROLLUP_CONFIG_FILE; the pane stays down from here"
