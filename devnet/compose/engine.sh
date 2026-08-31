#!/bin/sh
# op-cartesi, as compose starts it: the sequencer's engine and the verifier's
# are this same script with a different machine, a different data directory and
# a different set of published ports.
#
# The chain is not described here. `genesis` (compose/genesis.ts) writes the
# configuration document to /devnet/chain-config.json — the same parameters it
# generated rollup.json from — because an engine and a rollup config that
# disagree are a chain op-node rejects for serving the wrong genesis block, and
# a disagreement about the parameters below genesis is worse: op-node's
# handshake cannot see it, so it surfaces as a state root divergence later.
# One file, one source, both containers.
#
# Everything a stand-in engine needs is in the environment, so an
# implementation in another language can take this slot without reading any
# Go — see devnet/README.md.
set -eu

config="${CHAIN_CONFIG_FILE:-/devnet/chain-config.json}"
if [ ! -f "$config" ]; then
    echo "!!! no $config — the genesis step has not run" >&2
    exit 1
fi

exec op-cartesi run \
    -chain-config "$config" \
    -machine.remote "${MACHINE_REMOTE:?}" \
    -machine.snapshot "${MACHINE_SNAPSHOT:-/snapshot}" \
    ${DATA_DIR:+-datadir "$DATA_DIR"} \
    -engine.addr "${ENGINE_ADDR:-0.0.0.0:8551}" \
    -http.addr "${HTTP_ADDR:-0.0.0.0:8545}" \
    -engine.jwt-secret "${JWT_SECRET_FILE:-/devnet/jwt.hex}"
