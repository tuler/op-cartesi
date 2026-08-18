#!/bin/sh
# op-cartesi, as compose starts it: the sequencer's engine and the verifier's
# are this same script with a different machine, a different data directory and
# a different set of published ports.
#
# The chain flags are not written here. `genesis` (compose/genesis.ts) writes
# them to /devnet/chain-flags — the same list it generated rollup.json with —
# because an engine and a rollup config that disagree on them are a chain
# op-node rejects for serving the wrong genesis block. One file, one source,
# two containers.
set -eu

flags="${CHAIN_FLAGS_FILE:-/devnet/chain-flags}"
if [ ! -f "$flags" ]; then
    echo "!!! no $flags — the genesis step has not run" >&2
    exit 1
fi

# Deliberately unquoted: the file is a flag list, and splitting it on
# whitespace is how it becomes one. Every value in it is a number or a hex
# string, so there is nothing in there to split wrongly.
# shellcheck disable=SC2046
exec op-cartesi run \
    $(cat "$flags") \
    -machine.remote "${MACHINE_REMOTE:?}" \
    -machine.snapshot "${MACHINE_SNAPSHOT:-/snapshot}" \
    ${DATA_DIR:+-datadir "$DATA_DIR"} \
    -engine.addr "${ENGINE_ADDR:-0.0.0.0:8551}" \
    -http.addr "${HTTP_ADDR:-0.0.0.0:8545}" \
    -engine.jwt-secret "${JWT_SECRET_FILE:-/devnet/jwt.hex}"
