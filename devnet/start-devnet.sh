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

echo >&2
echo "stack is up. logs in $LOG_DIR" >&2
echo "  L1     http://127.0.0.1:$L1_PORT" >&2
echo "  L2     http://$HTTP_ADDR" >&2
echo "  opnode http://127.0.0.1:$OPNODE_RPC_PORT" >&2
echo "watch blocks:  cast block-number --rpc-url http://$HTTP_ADDR" >&2
echo "ctrl-c to tear the stack down" >&2
wait
