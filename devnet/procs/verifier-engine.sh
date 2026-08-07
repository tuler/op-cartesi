#!/usr/bin/env bash
# The verifying node's engine: a second op-cartesi that sequences nothing and
# learns the chain only from what the batcher posted to L1. That it agrees
# block for block is the property that makes this a rollup rather than a
# database with an RPC.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init verifier-engine

wait_ready genesis "the rollup config"
wait_for_port "$VERIFIER_MACHINE_PORT" "the verifier's machine server"
reload_addresses

export MACHINE_REMOTE="http://127.0.0.1:$VERIFIER_MACHINE_PORT"
export MACHINE_SNAPSHOT="$SNAPSHOT_DIR"
export ENGINE_ADDR="$VERIFIER_ENGINE_ADDR"
export HTTP_ADDR="$VERIFIER_HTTP_ADDR"
# Its own store: a pebble database is held by one process, and pointing two
# nodes at one directory fails with a message about resources rather than
# about sharing.
export DATA_DIR="${DATA_DIR:+$DATA_DIR-verifier}"

devnet_say "verifier engine on :${VERIFIER_ENGINE_ADDR##*:} (eth RPC :${VERIFIER_HTTP_ADDR##*:})"
exec "$DEVNET_DIR/start-shim.sh"
