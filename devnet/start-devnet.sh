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
# Generating the rollup config loads the snapshot too, and a machine server
# holds exactly one machine, so the generator gets its own server rather than
# recycling the node's. Recycling meant killing a server mid-run and racing the
# port back open, which is a race the script kept losing.
: "${GENESIS_MACHINE_PORT:=6301}"
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

# Teardown works from the pids this script started, not from process names.
# Matching by name was both too narrow and too wide: `pkill -x cartesi-jsonrpc`
# only matches on Linux, where the process name is truncated to 15 characters,
# so on macOS the machine servers survived and the next run inherited a server
# that already held a machine — while on Linux it happily killed emulators and
# anvils belonging to whatever else the developer was running.
PIDS=()
track() { PIDS+=("$1"); }

# Killing a machine server does not kill the servers it forked — op-cartesi
# forks one per block for snapshots — so teardown walks the whole tree.
kill_tree() {
  local pid=$1 kids
  kids=$(pgrep -P "$pid" 2>/dev/null || true)
  kill "$pid" 2>/dev/null || true
  local kid
  for kid in $kids; do kill_tree "$kid"; done
}

cleanup() {
  local pid
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill_tree "$pid"
  done
}
trap cleanup EXIT

port_free() {
  ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# A port still held from a previous run is reported rather than cleared: the
# process holding it may not be ours. This is also the failure this check
# exists to make legible — a machine server left behind answers just well
# enough for op-cartesi to connect and then fail with "machine exists".
require_free_ports() {
  local port busy=()
  for port in "$@"; do
    port_free "$port" || busy+=("$port")
  done
  if [ ${#busy[@]} -gt 0 ]; then
    echo "ports already in use: ${busy[*]}" >&2
    for port in "${busy[@]}"; do
      if command -v lsof > /dev/null 2>&1; then
        echo "  :$port held by pid(s) $(lsof -ti "tcp:$port" | tr '\n' ' ')" >&2
      fi
    done
    echo "stop them (or set the matching *_PORT variables) and try again" >&2
    exit 1
  fi
}

wait_for_port() {
  local port=$1
  for _ in $(seq 1 40); do
    port_free "$port" || return 0
    sleep 0.5
  done
  echo "nothing came up on port $port" >&2
  return 1
}

REQUIRED_PORTS=("$L1_PORT" "$MACHINE_PORT" "$GENESIS_MACHINE_PORT" "$OPNODE_RPC_PORT" "${ENGINE_ADDR##*:}" "${HTTP_ADDR##*:}")
[ "$WITH_BATCHER" = "1" ] && REQUIRED_PORTS+=("$BATCHER_RPC_PORT")
[ "$WITH_VERIFIER" = "1" ] && REQUIRED_PORTS+=("$VERIFIER_MACHINE_PORT" "$VERIFIER_OPNODE_RPC_PORT" "${VERIFIER_ENGINE_ADDR##*:}" "${VERIFIER_HTTP_ADDR##*:}")
require_free_ports "${REQUIRED_PORTS[@]}"

echo "starting anvil (L1, chain $L1_CHAIN_ID) on :$L1_PORT" >&2
anvil --host 127.0.0.1 --port "$L1_PORT" --chain-id "$L1_CHAIN_ID" --block-time 2 --silent \
  > "$LOG_DIR/anvil.log" 2>&1 &
track $!
wait_for_port "$L1_PORT"

L1_GENESIS_HASH=$(cast block 0 --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["hash"])')
# The L2 genesis timestamp is anchored to the L1 block the rollup starts after,
# so op-node's derivation clock and the engine's genesis agree.
GENESIS_TIMESTAMP=$(cast block 0 --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["timestamp"],16))')
export L1_GENESIS_HASH GENESIS_TIMESTAMP
export MACHINE_SNAPSHOT="$SNAPSHOT_DIR"

# Sets MACHINE_SERVER_PID so a caller that only needs the server briefly can
# stop it again.
start_machine_server() {
  local port=$1 log=$2
  cartesi-jsonrpc-machine --server-address="127.0.0.1:$port" > "$log" 2>&1 &
  MACHINE_SERVER_PID=$!
  track "$MACHINE_SERVER_PID"
  wait_for_port "$port" || { echo "--- $log ---" >&2; tail -5 "$log" >&2; exit 1; }
}

echo "generating rollup.json anchored to L1 block 0 ($L1_GENESIS_HASH)" >&2
start_machine_server "$GENESIS_MACHINE_PORT" "$LOG_DIR/machine-genesis.log"
MACHINE_REMOTE="http://127.0.0.1:$GENESIS_MACHINE_PORT" "$DEVNET_DIR/generate-config.sh"
# The generator is done with it, and a booted machine is not cheap to leave
# sitting around.
kill_tree "$MACHINE_SERVER_PID"

echo "starting op-cartesi" >&2
export MACHINE_REMOTE="http://127.0.0.1:$MACHINE_PORT"
start_machine_server "$MACHINE_PORT" "$LOG_DIR/machine.log"
"$DEVNET_DIR/start-shim.sh" > "$LOG_DIR/op-cartesi.log" 2>&1 &
track $!

# Booting Linux inside the machine takes a while before the engine answers.
for _ in $(seq 1 60); do
  if grep -q "chain initialized" "$LOG_DIR/op-cartesi.log" 2>/dev/null; then break; fi
  sleep 2
done
grep "chain initialized" "$LOG_DIR/op-cartesi.log" >&2 || {
  echo "engine never initialized; last lines of $LOG_DIR/op-cartesi.log:" >&2
  tail -10 "$LOG_DIR/op-cartesi.log" >&2
  exit 1
}

echo "starting op-node in sequencer mode" >&2
op-node \
  --l1="http://127.0.0.1:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
  --rollup.l1-chain-config="$L1_CHAIN_CONFIG" \
  --l2="http://$ENGINE_ADDR" --l2.jwt-secret="$JWT_SECRET_FILE" \
  --rollup.config="$ROLLUP_CONFIG_FILE" \
  --sequencer.enabled --sequencer.l1-confs=0 --verifier.l1-confs=0 \
  --p2p.disable --l1.beacon.ignore \
  --rpc.addr=127.0.0.1 --rpc.port="$OPNODE_RPC_PORT" > "$LOG_DIR/op-node.log" 2>&1 &
track $!

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
  track $!
fi

if [ "$WITH_VERIFIER" = "1" ]; then
  # A second node that never sequences: it learns the chain only from what the
  # batcher posted to L1, which is the property that makes the chain a rollup
  # rather than a database with an RPC.
  echo "starting verifier: second machine server, engine and op-node" >&2
  start_machine_server "$VERIFIER_MACHINE_PORT" "$LOG_DIR/machine-verifier.log"
  MACHINE_REMOTE="http://127.0.0.1:$VERIFIER_MACHINE_PORT" \
  ENGINE_ADDR="$VERIFIER_ENGINE_ADDR" HTTP_ADDR="$VERIFIER_HTTP_ADDR" \
    "$DEVNET_DIR/start-shim.sh" > "$LOG_DIR/op-cartesi-verifier.log" 2>&1 &
  track $!
  for _ in $(seq 1 60); do
    if grep -q "chain initialized" "$LOG_DIR/op-cartesi-verifier.log" 2>/dev/null; then break; fi
    sleep 2
  done
  grep -q "chain initialized" "$LOG_DIR/op-cartesi-verifier.log" || {
    echo "the verifier engine never initialized; last lines of $LOG_DIR/op-cartesi-verifier.log:" >&2
    tail -10 "$LOG_DIR/op-cartesi-verifier.log" >&2
    exit 1
  }
  op-node \
    --l1="http://127.0.0.1:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
    --rollup.l1-chain-config="$L1_CHAIN_CONFIG" \
    --l2="http://$VERIFIER_ENGINE_ADDR" --l2.jwt-secret="$JWT_SECRET_FILE" \
    --rollup.config="$ROLLUP_CONFIG_FILE" \
    --verifier.l1-confs=0 --p2p.disable --l1.beacon.ignore \
    --rpc.addr=127.0.0.1 --rpc.port="$VERIFIER_OPNODE_RPC_PORT" \
    > "$LOG_DIR/op-node-verifier.log" 2>&1 &
  track $!
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
