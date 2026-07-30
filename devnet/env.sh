#!/usr/bin/env bash
# Shared configuration for the devnet scripts. Override any of these in the
# environment, e.g. L2_CHAIN_ID=42 ./devnet/start-shim.sh
#
# The CHAIN_FLAGS below are consensus-relevant: they determine the L2 genesis
# block hash, so `generate-config.sh` and `start-shim.sh` must both use them.

set -euo pipefail

DEVNET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$DEVNET_DIR")"

# --- machine ---------------------------------------------------------------
# Empty runs the deterministic in-memory mock. Point this at a
# cartesi-jsonrpc-machine server to run a real Cartesi Machine.
: "${MACHINE_REMOTE:=}"

# --- chain identity (consensus-relevant) -----------------------------------
: "${L1_CHAIN_ID:=900}"
: "${L2_CHAIN_ID:=901}"
: "${GENESIS_TIMESTAMP:=0}"
: "${GAS_LIMIT:=30000000}"
: "${MAX_CYCLES_PER_INPUT:=1000000000}"

# --- L1 anchor and contract addresses --------------------------------------
# These come from the L1 chain and the op-deployer output. The placeholder
# addresses below let the scripts run end-to-end without a deployment; replace
# them before connecting a real op-node.
: "${L1_GENESIS_HASH:=0x0000000000000000000000000000000000000000000000000000000000000000}"
: "${L1_GENESIS_NUMBER:=0}"
: "${BATCHER_ADDRESS:=0x42000000000000000000000000000000000000f0}"
: "${BATCH_INBOX_ADDRESS:=0xff00000000000000000000000000000000000901}"
: "${DEPOSIT_CONTRACT_ADDRESS:=0x6900000000000000000000000000000000000001}"
: "${L1_SYSTEM_CONFIG_ADDRESS:=0x6900000000000000000000000000000000000002}"

# --- endpoints -------------------------------------------------------------
: "${ENGINE_ADDR:=127.0.0.1:8551}"
: "${HTTP_ADDR:=127.0.0.1:8545}"
: "${BLOCK_TIME:=2}"

: "${JWT_SECRET_FILE:=$DEVNET_DIR/jwt.hex}"
: "${ROLLUP_CONFIG_FILE:=$DEVNET_DIR/rollup.json}"

CHAIN_FLAGS=(
  -chain-id "$L2_CHAIN_ID"
  -genesis.timestamp "$GENESIS_TIMESTAMP"
  -gas-limit "$GAS_LIMIT"
  -max-cycles-per-input "$MAX_CYCLES_PER_INPUT"
)
if [ -n "$MACHINE_REMOTE" ]; then
  CHAIN_FLAGS+=(-machine.remote "$MACHINE_REMOTE")
fi

# ensure_jwt writes a fresh 32-byte hex secret if none exists yet.
ensure_jwt() {
  if [ ! -f "$JWT_SECRET_FILE" ]; then
    openssl rand -hex 32 > "$JWT_SECRET_FILE"
    echo "wrote a new JWT secret to $JWT_SECRET_FILE" >&2
  fi
}
