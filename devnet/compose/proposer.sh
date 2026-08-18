#!/bin/sh
# op-proposer, with the one flag compose cannot write down in advance.
#
# Every other service's command line is fixed before anything starts: the
# addresses op-node and op-batcher need are inside rollup.json, which the
# genesis step writes. op-proposer takes its DisputeGameFactory as a flag, and
# that address does not exist until op-deployer has run — so it is read here,
# out of the file the deploy wrote, at the moment the container starts.
#
# Mounted into the published op-proposer image rather than baked into one of
# ours: the image has a shell, which is all this needs.
set -eu

addresses="${L1_ADDRESSES_FILE:-/devnet/l1-addresses.env}"
if [ ! -f "$addresses" ]; then
    echo "!!! no $addresses — the l1-contracts step has not run" >&2
    exit 1
fi
. "$addresses"

exec op-proposer --game-factory-address="${DISPUTE_GAME_FACTORY_ADDRESS:?}" "$@"
