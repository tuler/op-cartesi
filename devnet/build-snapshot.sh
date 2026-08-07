#!/usr/bin/env bash
# Builds the Cartesi Machine snapshot that becomes the chain's genesis state:
# the routed guest of ledger-app (docs/EVM-COMPAT.md), built with
# `cartesi build` (@cartesi/cli 2.0 alpha) into ledger-app/.cartesi/image —
# a stored machine, booted and parked at its first input yield, which is what
# makes genesis reproducible: the chain's genesis state root is the stored
# machine's own root hash.
#
# Genesis parameters travel differently than they did for the Lua guest:
# CHAIN_ID and OWNER are Dockerfile build ARGs whose defaults match env.sh
# (chain 901, owner anvil account 3). To deviate, set build_args on
# [drives.root] in ledger-app/cartesi.toml — they are consensus parameters,
# baked into the image environment and covered by the genesis state root.
# The accounts drive geometry lives in the same cartesi.toml.
#
# Requires: @cartesi/cli (npm i -g @cartesi/cli@alpha), docker with riscv64
# support, and `bun install` run once at the repo root (the app is a bun
# workspace member; the build's docker context is the repo root).

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

if ! command -v cartesi > /dev/null 2>&1; then
  echo "cartesi is not on PATH; install it with: npm i -g @cartesi/cli@alpha" >&2
  exit 1
fi

if [ "$GUEST_OWNER" != "0x90F79bf6EB2c4f870365E785982E1f101E93b906" ] || [ "$L2_CHAIN_ID" != "901" ]; then
  echo "note: GUEST_OWNER/L2_CHAIN_ID differ from the image defaults; set matching" >&2
  echo "build_args on [drives.root] in ledger-app/cartesi.toml or the snapshot will" >&2
  echo "disagree with the chain flags." >&2
fi

cd "$REPO_DIR/ledger-app"
cartesi build

echo "stored machine snapshot in $REPO_DIR/ledger-app/.cartesi/image" >&2
echo "run it with: cartesi-jsonrpc-machine --server-address=127.0.0.1:6000" >&2
echo "then: ./devnet/start-shim.sh with MACHINE_REMOTE=http://127.0.0.1:6000 MACHINE_SNAPSHOT=$REPO_DIR/ledger-app/.cartesi/image" >&2
