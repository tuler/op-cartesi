#!/usr/bin/env bash
# Shared configuration for the devnet scripts. Override any of these in the
# environment, e.g. L2_CHAIN_ID=42 ./devnet/start-shim.sh
#
# The CHAIN_FLAGS below are consensus-relevant: they determine the L2 genesis
# block hash, so `generate-config.sh` and `start-shim.sh` must both use them.

set -euo pipefail

DEVNET_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$DEVNET_DIR")"

# A .env at the repo root carries user overrides for both config surfaces:
# bun auto-loads it for the TypeScript scripts (devnet/lib/env.ts), and this
# loop replays it here with the same precedence — an already-exported
# variable wins over the file, the opposite of a plain `source`. Keys are
# validated before the eval so a stray line cannot execute anything.
if [ -f "$REPO_DIR/.env" ]; then
  while IFS='=' read -r k v; do
    case "$k" in ''|\#*) continue ;; esac
    if [[ "$k" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
      v="${v%\"}" ; v="${v#\"}"                # strip optional quotes, as bun does
      eval ": \"\${$k:=\$v}\""
    fi
  done < "$REPO_DIR/.env"
fi

# --- machine ---------------------------------------------------------------
# Empty runs the deterministic in-memory mock. Point this at a
# cartesi-jsonrpc-machine server to run a real Cartesi Machine.
: "${MACHINE_REMOTE:=}"
# Directory of a stored machine for the server to load. The devnet guest is
# ledger-app (the routed guest of docs/EVM-COMPAT.md), built by
# ./devnet/build-snapshot.sh into ledger-app/.cartesi/image.
: "${MACHINE_SNAPSHOT:=}"
# The address the guest accepts configuration from — portal registrations and
# the fee, through the config contract at GUEST_CONFIG_ADDRESS. It is baked
# into the snapshot (a Dockerfile ARG of ledger-app, this same default), so
# it is consensus-relevant, and it has to be known before anything is
# deployed, which is the point: the portals are not. Default is anvil
# account 3, the key the deploy scripts use.
: "${GUEST_OWNER:=0x90F79bf6EB2c4f870365E785982E1f101E93b906}"
# The routed guest's config contract (EVM-COMPAT §6): the system-namespace
# address owner configuration is sent to.
: "${GUEST_CONFIG_ADDRESS:=0xc751000000000000000000000000000000000002}"

# --- chain identity (consensus-relevant) -----------------------------------
: "${L1_CHAIN_ID:=900}"
: "${L2_CHAIN_ID:=901}"
: "${GENESIS_TIMESTAMP:=0}"
: "${GAS_LIMIT:=30000000}"
: "${MAX_CYCLES_PER_INPUT:=1000000000}"

# --- L1 anchor and contract addresses --------------------------------------
# devnet/deploy-l1.sh writes these after running op-deployer. Sourcing the file
# first lets everything below act as a fallback, so the scripts still run
# end-to-end with placeholders when no contracts have been deployed.
if [ -f "$DEVNET_DIR/l1-addresses.env" ]; then
  # shellcheck disable=SC1091
  source "$DEVNET_DIR/l1-addresses.env"
fi
# devnet/procs/genesis.sh writes this one: the L1 block the rollup is anchored
# at and the L2 genesis timestamp derived from it. It is how the engine — a
# different process, in a different pane — ends up on the same genesis the
# rollup config was generated with. Sourced after l1-addresses.env, because it
# is computed from it and therefore wins.
if [ -f "$DEVNET_DIR/chain-genesis.env" ]; then
  # shellcheck disable=SC1091
  source "$DEVNET_DIR/chain-genesis.env"
fi
if [ -f "$DEVNET_DIR/outputs-addresses.env" ]; then
  # shellcheck disable=SC1091
  source "$DEVNET_DIR/outputs-addresses.env"
fi
if [ -f "$DEVNET_DIR/token.env" ]; then
  # shellcheck disable=SC1091
  source "$DEVNET_DIR/token.env"
fi
: "${L1_GENESIS_HASH:=0x0000000000000000000000000000000000000000000000000000000000000000}"
: "${L1_GENESIS_NUMBER:=0}"
# The batcher's address must match the one in the rollup config's genesis
# system config, or op-node ignores its batches. Default is anvil account 0.
: "${BATCHER_ADDRESS:=0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266}"
: "${BATCHER_KEY:=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80}"
: "${BATCH_INBOX_ADDRESS:=0xff00000000000000000000000000000000000901}"
: "${DEPOSIT_CONTRACT_ADDRESS:=0x6900000000000000000000000000000000000001}"
: "${L1_SYSTEM_CONFIG_ADDRESS:=0x6900000000000000000000000000000000000002}"
: "${DISPUTE_GAME_FACTORY_ADDRESS:=}"
# op-deployer registers only the permissioned game (type 1), which is the
# right one here: there is no fault proof VM that can execute a Cartesi
# Machine, so proposals are made by a trusted proposer and never disputed.
# That is how OP chains launch; real disputes are the settlement track.
: "${DISPUTE_GAME_TYPE:=1}"
# Anvil account 2, matching the proposer role in the op-deployer intent — the
# permissioned game rejects proposals from anyone else.
: "${PROPOSER_ADDRESS:=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC}"
: "${PROPOSER_KEY:=0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a}"
# The L1 fee scalars in the genesis system config. Defaults match op-node's,
# but with contracts deployed these are read off the real SystemConfig.
: "${BASE_FEE_SCALAR:=1368}"
: "${BLOB_BASE_FEE_SCALAR:=810949}"
# Consensus-relevant on the L1 side: op-node reads these back from SystemConfig.
: "${EIP1559_DENOMINATOR:=50}"
: "${EIP1559_ELASTICITY:=6}"

# --- op-node / op-batcher --------------------------------------------------
# The OP monorepo ships no binaries: its releases carry source archives only,
# so docker is the official distribution and the only way to get these on a
# machine you would rather not compile Go on. `auto` uses whatever is on PATH
# and falls back to the published images; force either with OP_RUNTIME=docker
# or OP_RUNTIME=native.
#
# The two are versioned independently upstream, so the tags do not match.
: "${OP_RUNTIME:=auto}"
: "${OP_NODE_IMAGE:=us-docker.pkg.dev/oplabs-tools-artifacts/images/op-node:v1.19.3}"
: "${OP_BATCHER_IMAGE:=us-docker.pkg.dev/oplabs-tools-artifacts/images/op-batcher:v1.16.11}"
: "${OP_PROPOSER_IMAGE:=us-docker.pkg.dev/oplabs-tools-artifacts/images/op-proposer:v1.16.3}"
: "${OP_DEPLOYER_IMAGE:=us-docker.pkg.dev/oplabs-tools-artifacts/images/op-deployer:v0.7.1}"

# --- persistence ------------------------------------------------------------
# Directory for the chain store. Empty keeps the chain in memory, which is how
# it worked before and still fine for a throwaway run.
: "${DATA_DIR:=}"
: "${CHECKPOINT_INTERVAL:=25}"

# --- endpoints -------------------------------------------------------------
: "${ENGINE_ADDR:=127.0.0.1:8551}"
: "${HTTP_ADDR:=127.0.0.1:8545}"
: "${BLOCK_TIME:=2}"

: "${JWT_SECRET_FILE:=$DEVNET_DIR/jwt.hex}"
: "${ROLLUP_CONFIG_FILE:=$DEVNET_DIR/rollup.json}"

# --- the binary -------------------------------------------------------------
# `go run` is the default because it needs no build step, but it is a wrapper
# around the process that matters: a signal sent to it is not necessarily a
# signal to the engine, and a supervisor that stops the wrapper can leave the
# engine behind. start-devnet.sh compiles once and points this at the result,
# so each pane holds the engine itself.
: "${OP_CARTESI_BIN:=}"
if [ -n "$OP_CARTESI_BIN" ]; then
  OP_CARTESI=("$OP_CARTESI_BIN")
else
  OP_CARTESI=(go run ./cmd/op-cartesi)
fi

CHAIN_FLAGS=(
  -chain-id "$L2_CHAIN_ID"
  -checkpoint-interval "$CHECKPOINT_INTERVAL"
  -genesis.timestamp "$GENESIS_TIMESTAMP"
  -gas-limit "$GAS_LIMIT"
  -max-cycles-per-input "$MAX_CYCLES_PER_INPUT"
)
if [ -n "$MACHINE_REMOTE" ]; then
  CHAIN_FLAGS+=(-machine.remote "$MACHINE_REMOTE")
fi
if [ -n "$MACHINE_SNAPSHOT" ]; then
  CHAIN_FLAGS+=(-machine.snapshot "$MACHINE_SNAPSHOT")
fi

# ensure_jwt writes a fresh 32-byte hex secret if none exists yet.
ensure_jwt() {
  if [ ! -f "$JWT_SECRET_FILE" ]; then
    openssl rand -hex 32 > "$JWT_SECRET_FILE"
    echo "wrote a new JWT secret to $JWT_SECRET_FILE" >&2
  fi
}
