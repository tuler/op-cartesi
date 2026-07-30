#!/usr/bin/env bash
# Brings up the whole stack: anvil as L1, a Cartesi Machine, op-cartesi as the
# execution engine, and op-node sequencing on top.
#
# This runs without any L1 contracts deployed. op-node still drives block
# production — every L2 block carries the L1-attributes deposit it injects —
# which is what exercises the engine end to end. Deposits from users and a
# safe chain need the contracts and op-batcher; see README.md.
#
# Prerequisites: anvil, cast, cartesi-machine, cartesi-jsonrpc-machine, op-node.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${MACHINE_PORT:=6300}"
: "${OPNODE_RPC_PORT:=9545}"
# Second node: its own engine, machine server and op-node, deriving purely
# from L1 rather than sequencing.
: "${VERIFIER_ENGINE_ADDR:=127.0.0.1:8571}"
: "${VERIFIER_HTTP_ADDR:=127.0.0.1:8565}"
: "${VERIFIER_MACHINE_PORT:=6400}"
: "${VERIFIER_OPNODE_RPC_PORT:=9555}"
# Set to 0 to run only the sequencer.
: "${BATCHER_RPC_PORT:=8548}"
: "${WITH_BATCHER:=1}"
: "${WITH_VERIFIER:=1}"
: "${SNAPSHOT_DIR:=$DEVNET_DIR/snapshot}"
: "${LOG_DIR:=$DEVNET_DIR/logs}"
: "${L1_CHAIN_CONFIG:=$DEVNET_DIR/l1-chain-config.json}"

mkdir -p "$LOG_DIR"
if [ ! -d "$SNAPSHOT_DIR" ]; then
  echo "no machine snapshot at $SNAPSHOT_DIR — run ./devnet/build-snapshot.sh first" >&2
  exit 1
fi

# The process name is truncated to 15 characters, which is shorter than
# "cartesi-jsonrpc-machine", so an exact-match kill has to use the truncation.
# pkill exits non-zero when nothing matched, which under `set -e` would abort
# the script before it starts anything.
cleanup() {
  pkill -x op-node 2>/dev/null || true
  pkill -x op-batcher 2>/dev/null || true
  pkill -x op-cartesi 2>/dev/null || true
  pkill -x cartesi-jsonrpc 2>/dev/null || true
  pkill -x anvil 2>/dev/null || true
}
trap cleanup EXIT
cleanup; sleep 1

echo "starting anvil (L1, chain $L1_CHAIN_ID) on :$L1_PORT" >&2
anvil --host 127.0.0.1 --port "$L1_PORT" --chain-id "$L1_CHAIN_ID" --block-time 2 --silent \
  > "$LOG_DIR/anvil.log" 2>&1 &
sleep 4

L1_GENESIS_HASH=$(cast block 0 --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["hash"])')
# The L2 genesis timestamp is anchored to the L1 block the rollup starts after,
# so op-node's derivation clock and the engine's genesis agree.
GENESIS_TIMESTAMP=$(cast block 0 --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["timestamp"],16))')
export L1_GENESIS_HASH GENESIS_TIMESTAMP
export MACHINE_REMOTE="http://127.0.0.1:$MACHINE_PORT" MACHINE_SNAPSHOT="$SNAPSHOT_DIR"

# The generator and the node each load the snapshot into a server, and a server
# holds one machine, so each gets a fresh one.
start_machine_server() {
  pkill -x cartesi-jsonrpc 2>/dev/null || true
  sleep 1
  cartesi-jsonrpc-machine --server-address="127.0.0.1:$MACHINE_PORT" > "$LOG_DIR/machine.log" 2>&1 &
  sleep 2
}

echo "generating rollup.json anchored to L1 block 0 ($L1_GENESIS_HASH)" >&2
start_machine_server
"$DEVNET_DIR/generate-config.sh"

echo "starting op-cartesi on a fresh machine server" >&2
start_machine_server
"$DEVNET_DIR/start-shim.sh" > "$LOG_DIR/op-cartesi.log" 2>&1 &

# Booting Linux inside the machine takes a while before the engine answers.
for _ in $(seq 1 60); do
  if grep -q "chain initialized" "$LOG_DIR/op-cartesi.log" 2>/dev/null; then break; fi
  sleep 2
done
grep "chain initialized" "$LOG_DIR/op-cartesi.log" >&2 || { echo "engine never initialized" >&2; exit 1; }

echo "starting op-node in sequencer mode" >&2
op-node \
  --l1="http://127.0.0.1:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
  --rollup.l1-chain-config="$L1_CHAIN_CONFIG" \
  --l2="http://$ENGINE_ADDR" --l2.jwt-secret="$JWT_SECRET_FILE" \
  --rollup.config="$ROLLUP_CONFIG_FILE" \
  --sequencer.enabled --sequencer.l1-confs=0 --verifier.l1-confs=0 \
  --p2p.disable --l1.beacon.ignore \
  --rpc.addr=127.0.0.1 --rpc.port="$OPNODE_RPC_PORT" > "$LOG_DIR/op-node.log" 2>&1 &

if [ "$WITH_BATCHER" = "1" ]; then
  # Batches go to L1 as calldata: blobs would need a beacon endpoint, and this
  # devnet deliberately runs without one.
  echo "starting op-batcher (calldata mode)" >&2
  op-batcher \
    --l1-eth-rpc="http://127.0.0.1:$L1_PORT" \
    --l2-eth-rpc="http://$HTTP_ADDR" \
    --rollup-rpc="http://127.0.0.1:$OPNODE_RPC_PORT" \
    --private-key="$BATCHER_KEY" \
    --data-availability-type=calldata \
    --sub-safety-margin=4 --poll-interval=2s --num-confirmations=1 \
    --max-channel-duration=2 \
    --rpc.addr=127.0.0.1 --rpc.port="$BATCHER_RPC_PORT" \
    > "$LOG_DIR/op-batcher.log" 2>&1 &
fi

if [ "$WITH_VERIFIER" = "1" ]; then
  # A second node that never sequences: it learns the chain only from what the
  # batcher posted to L1, which is the property that makes the chain a rollup
  # rather than a database with an RPC.
  echo "starting verifier: second machine server, engine and op-node" >&2
  cartesi-jsonrpc-machine --server-address="127.0.0.1:$VERIFIER_MACHINE_PORT" \
    > "$LOG_DIR/machine-verifier.log" 2>&1 &
  sleep 2
  MACHINE_REMOTE="http://127.0.0.1:$VERIFIER_MACHINE_PORT" \
  ENGINE_ADDR="$VERIFIER_ENGINE_ADDR" HTTP_ADDR="$VERIFIER_HTTP_ADDR" \
    "$DEVNET_DIR/start-shim.sh" > "$LOG_DIR/op-cartesi-verifier.log" 2>&1 &
  for _ in $(seq 1 60); do
    if grep -q "chain initialized" "$LOG_DIR/op-cartesi-verifier.log" 2>/dev/null; then break; fi
    sleep 2
  done
  op-node \
    --l1="http://127.0.0.1:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
    --rollup.l1-chain-config="$L1_CHAIN_CONFIG" \
    --l2="http://$VERIFIER_ENGINE_ADDR" --l2.jwt-secret="$JWT_SECRET_FILE" \
    --rollup.config="$ROLLUP_CONFIG_FILE" \
    --verifier.l1-confs=0 --p2p.disable --l1.beacon.ignore \
    --rpc.addr=127.0.0.1 --rpc.port="$VERIFIER_OPNODE_RPC_PORT" \
    > "$LOG_DIR/op-node-verifier.log" 2>&1 &
fi

echo >&2
echo "stack is up. logs in $LOG_DIR" >&2
echo "  L1     http://127.0.0.1:$L1_PORT" >&2
echo "  L2     http://$HTTP_ADDR" >&2
echo "  opnode http://127.0.0.1:$OPNODE_RPC_PORT" >&2
if [ "$WITH_VERIFIER" = "1" ]; then
  echo "  verifier L2     http://$VERIFIER_HTTP_ADDR" >&2
  echo "  verifier opnode http://127.0.0.1:$VERIFIER_OPNODE_RPC_PORT" >&2
fi
echo "watch blocks:  cast block-number --rpc-url http://$HTTP_ADDR" >&2
echo "ctrl-c to tear the stack down" >&2
wait
