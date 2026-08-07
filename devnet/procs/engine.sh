#!/usr/bin/env bash
# op-cartesi, the sequencer's execution engine: the authenticated Engine API
# op-node drives, and the public eth_* RPC everything else uses.
#
# It waits for genesis because its chain flags have to match the ones
# rollup.json was generated with — GENESIS_TIMESTAMP among them, which
# procs/genesis.sh writes to chain-genesis.env for exactly this reason.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init engine

wait_ready genesis "the rollup config"
wait_for_port "$MACHINE_PORT" "the machine server"
reload_addresses

export MACHINE_REMOTE="http://127.0.0.1:$MACHINE_PORT"
export MACHINE_SNAPSHOT="$SNAPSHOT_DIR"

devnet_say "booting the machine; the engine binds :${ENGINE_ADDR##*:} and :${HTTP_ADDR##*:} once the chain is open"
exec "$DEVNET_DIR/start-shim.sh"
