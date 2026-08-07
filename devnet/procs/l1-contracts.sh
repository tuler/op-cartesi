#!/usr/bin/env bash
# The OP Stack L1 contract suite, deployed with op-deployer.
#
# Optional (WITH_CONTRACTS=0 leaves this pane out), but it runs before the
# rollup is anchored rather than alongside it: op-node reads the SystemConfig
# at the anchor block to start derivation, and before the deployment there is
# nothing there to read. So genesis waits for this to finish, and everything
# downstream waits for genesis.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init l1-contracts

clear_ready l1-contracts
wait_for_port "$L1_PORT" "anvil"

# A fresh anvil forgets every deployment, so anything recorded by a previous
# run is stale the moment it boots.
rm -f "$DEVNET_DIR/l1-addresses.env"
"$DEVNET_DIR/deploy-l1.sh"
mark_ready l1-contracts

devnet_say "L1 contracts deployed; the pane stays down from here"
