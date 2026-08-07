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
# Deploy the OP L1 contract suite with op-deployer. Set to 0 for the older,
# faster bring-up with placeholder addresses and no contracts.
: "${WITH_CONTRACTS:=1}"
: "${WITH_PROPOSER:=1}"
: "${PROPOSER_RPC_PORT:=8560}"
# The routed guest's snapshot, as `cartesi build` stores it
# (./devnet/build-snapshot.sh wraps that build).
: "${SNAPSHOT_DIR:=$REPO_DIR/ledger-app/.cartesi/image}"
: "${LOG_DIR:=$DEVNET_DIR/logs}"
: "${L1_CHAIN_CONFIG:=$DEVNET_DIR/l1-chain-config.json}"

mkdir -p "$LOG_DIR"
# op-node needs the secret before op-cartesi would otherwise create it, and in
# docker it is a bind mount, which docker would silently create as a directory
# if the file were missing.
ensure_jwt
if [ ! -d "$SNAPSHOT_DIR" ]; then
  echo "no machine snapshot at $SNAPSHOT_DIR — run ./devnet/build-snapshot.sh first" >&2
  exit 1
fi

# Teardown works from what this script started, never from process names.
# Matching by name was both too narrow and too wide: `pkill -x cartesi-jsonrpc`
# only matches on Linux, where the process name is truncated to 15 characters,
# so on macOS the machine servers survived and the next run inherited a server
# that already held a machine — while on Linux it happily killed emulators and
# anvils belonging to whatever else the developer was running.
PIDS=()
track() { PIDS+=("$1"); }

# Walking parent to child is the fallback, not the mechanism: op-cartesi forks
# a machine server per block and shuts down the ones it no longer needs, which
# reparents their own forks to init and puts them out of reach of any walk.
kill_tree() {
  local pid=$1 kids
  kids=$(pgrep -P "$pid" 2>/dev/null || true)
  kill "$pid" 2>/dev/null || true
  local kid
  for kid in $kids; do kill_tree "$kid"; done
}

# Containers are not children of this script, so they need removing by name.
# The names are prefixed so this only ever touches containers it started.
CONTAINER_PREFIX="op-cartesi-devnet"
DOCKER_NETWORK="op-cartesi-devnet"

# Teardown goes by process group. Everything this script starts inherits it,
# including the emulator's forks, and — unlike a parent — a process group
# survives its members being reparented to init, which is what happens to every
# fork whose snapshot op-cartesi has pruned. A short run leaks a handful that
# way; a long one leaked 67 of 205.
#
# The group covers this script's descendants and nothing else, so this is not
# the blunt instrument that killing by process name was. It only holds when the
# script leads its own group, which it does when run normally; if it does not,
# the pid walk is the fallback.
cleanup() {
  # Containers are not in the process group, so they go first.
  if [ "${OP_RUNTIME:-native}" = "docker" ]; then
    local names
    names=$(docker ps -aq --filter "name=^${CONTAINER_PREFIX}-" 2>/dev/null || true)
    [ -n "$names" ] && docker rm -f $names > /dev/null 2>&1 || true
    docker network rm "$DOCKER_NETWORK" > /dev/null 2>&1 || true
  fi

  local pgid
  pgid=$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ')
  if [ "$pgid" = "$$" ]; then
    # This kills the script too, so nothing may follow it — and the trap is
    # cleared first so the signal does not re-enter here.
    trap - EXIT
    kill -TERM -- "-$pgid" 2>/dev/null || true
    sleep 1
    kill -KILL -- "-$pgid" 2>/dev/null || true
    return
  fi

  local i
  for (( i=${#PIDS[@]}-1 ; i>=0 ; i-- )); do
    kill_tree "${PIDS[i]}"
  done
}
trap cleanup EXIT

# The OP tools run either as binaries on PATH or as the images the monorepo
# publishes, since it publishes no binaries at all.
#
# The choice is all-or-nothing rather than per-tool, because it also decides
# how everything addresses everything else: a container reaches the host over
# the gateway, a binary over loopback. Mixing the two would mean two addressing
# schemes in one stack. So `auto` only picks native when every tool this run
# will actually start is present — having op-node and op-batcher but not
# op-proposer used to select native and then fail on `op-proposer: command not
# found`, several minutes in.
OP_TOOLS=(op-node)
[ "$WITH_BATCHER" = "1" ] && OP_TOOLS+=(op-batcher)
[ "$WITH_PROPOSER" = "1" ] && [ "$WITH_CONTRACTS" = "1" ] && OP_TOOLS+=(op-proposer)
if [ "$OP_RUNTIME" = "auto" ]; then
  OP_RUNTIME=native
  for tool in "${OP_TOOLS[@]}"; do
    command -v "$tool" > /dev/null 2>&1 || OP_RUNTIME=docker
  done
  if [ "$OP_RUNTIME" = "docker" ] && ! command -v docker > /dev/null 2>&1; then
    echo "need docker, or all of: ${OP_TOOLS[*]} on PATH" >&2
    exit 1
  fi
elif [ "$OP_RUNTIME" = "native" ]; then
  for tool in "${OP_TOOLS[@]}"; do
    command -v "$tool" > /dev/null 2>&1 || { echo "OP_RUNTIME=native but $tool is not on PATH" >&2; exit 1; }
  done
fi

# op-deployer has no binaries at all, so deploying contracts means docker even
# when op-node does not — and that alone forces anvil off loopback.
DEPLOYER_IN_DOCKER=0
if [ "$WITH_CONTRACTS" = "1" ] && ! command -v op-deployer > /dev/null 2>&1; then
  DEPLOYER_IN_DOCKER=1
fi
if [ "$OP_RUNTIME" = "docker" ] || [ "$DEPLOYER_IN_DOCKER" = "1" ]; then
  L1_BIND_ADDR="0.0.0.0"
else
  L1_BIND_ADDR="127.0.0.1"
fi

if [ "$OP_RUNTIME" = "docker" ]; then
  # A container reaches anvil and op-cartesi over the host gateway rather than
  # its own loopback, which in turn means those two have to listen on more
  # than loopback. That is a devnet-only trade: on a shared network it exposes
  # them, which is why it is not the default when running natively.
  HOST_ADDR="host.docker.internal"
  BIND_ADDR="0.0.0.0"
  ROLLUP_CONFIG_ARG=/config/rollup.json
  JWT_ARG=/config/jwt.hex
  L1_CHAIN_CONFIG_ARG=/config/l1-chain-config.json
  # Exported, because start-shim.sh reads these from the environment and would
  # otherwise fall back to env.sh's loopback defaults — which the containers
  # cannot reach.
  export ENGINE_ADDR="$BIND_ADDR:${ENGINE_ADDR##*:}"
  export HTTP_ADDR="$BIND_ADDR:${HTTP_ADDR##*:}"
  VERIFIER_ENGINE_ADDR="$BIND_ADDR:${VERIFIER_ENGINE_ADDR##*:}"
  VERIFIER_HTTP_ADDR="$BIND_ADDR:${VERIFIER_HTTP_ADDR##*:}"
  # op-batcher talks to op-node, and both are containers, so they share a
  # user-defined network and address each other by name. Going back out
  # through the host would mean publishing op-node's RPC on every interface
  # rather than just loopback — a port bound to the host's loopback is not
  # reachable from the bridge gateway, which is exactly how this first failed.
  docker network create "$DOCKER_NETWORK" > /dev/null 2>&1 || true
  SEQUENCER_RPC="http://$CONTAINER_PREFIX-sequencer:$OPNODE_RPC_PORT"
  echo "running op-node and op-batcher from docker images" >&2
else
  HOST_ADDR="127.0.0.1"
  BIND_ADDR="127.0.0.1"
  ROLLUP_CONFIG_ARG="$ROLLUP_CONFIG_FILE"
  JWT_ARG="$JWT_SECRET_FILE"
  L1_CHAIN_CONFIG_ARG="$L1_CHAIN_CONFIG"
  SEQUENCER_RPC="http://127.0.0.1:$OPNODE_RPC_PORT"
fi

# run_op starts one of the two, either way, and streams its output to a log.
# In docker the foreground CLI is what gets tracked; the container itself is
# torn down by name, since killing the client would leave it running.
run_op() {
  local kind=$1 name=$2 rpc_port=$3 log=$4
  shift 4
  if [ "$OP_RUNTIME" = "docker" ]; then
    local image=$OP_NODE_IMAGE
    [ "$kind" = "op-batcher" ] && image=$OP_BATCHER_IMAGE
    [ "$kind" = "op-proposer" ] && image=$OP_PROPOSER_IMAGE
    docker rm -f "$CONTAINER_PREFIX-$name" > /dev/null 2>&1 || true
    docker run --rm --name "$CONTAINER_PREFIX-$name" \
      --network "$DOCKER_NETWORK" \
      --add-host="host.docker.internal:host-gateway" \
      -p "127.0.0.1:$rpc_port:$rpc_port" \
      -v "$ROLLUP_CONFIG_FILE:$ROLLUP_CONFIG_ARG:ro" \
      -v "$JWT_SECRET_FILE:$JWT_ARG:ro" \
      -v "$L1_CHAIN_CONFIG:$L1_CHAIN_CONFIG_ARG:ro" \
      "$image" "$kind" "$@" > "$log" 2>&1 &
  else
    "$kind" "$@" > "$log" 2>&1 &
  fi
  track $!
}

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
[ "$WITH_PROPOSER" = "1" ] && REQUIRED_PORTS+=("$PROPOSER_RPC_PORT")
[ "$WITH_VERIFIER" = "1" ] && REQUIRED_PORTS+=("$VERIFIER_MACHINE_PORT" "$VERIFIER_OPNODE_RPC_PORT" "${VERIFIER_ENGINE_ADDR##*:}" "${VERIFIER_HTTP_ADDR##*:}")
require_free_ports "${REQUIRED_PORTS[@]}"

echo "starting anvil (L1, chain $L1_CHAIN_ID) on :$L1_PORT" >&2
anvil --host "$L1_BIND_ADDR" --port "$L1_PORT" --chain-id "$L1_CHAIN_ID" --block-time 2 --silent \
  > "$LOG_DIR/anvil.log" 2>&1 &
track $!
wait_for_port "$L1_PORT"

if [ "$WITH_CONTRACTS" = "1" ]; then
  # Deploying first, because the rollup has to be anchored at a block where
  # the SystemConfig already exists — op-node reads it there to start
  # derivation, and before the deployment there is nothing to read.
  rm -f "$DEVNET_DIR/l1-addresses.env"
  "$DEVNET_DIR/deploy-l1.sh" > "$LOG_DIR/deploy-l1.log" 2>&1 || {
    echo "deploying the L1 contracts failed; last lines of $LOG_DIR/deploy-l1.log:" >&2
    tail -20 "$LOG_DIR/deploy-l1.log" >&2
    exit 1
  }
  # shellcheck disable=SC1091
  source "$DEVNET_DIR/l1-addresses.env"
  echo "L1 contracts deployed; rollup anchored at L1 block $L1_GENESIS_NUMBER" >&2
else
  L1_GENESIS_NUMBER=0
  L1_GENESIS_HASH=$(cast block 0 --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["hash"])')
fi

# The L2 genesis timestamp is anchored to the L1 block the rollup starts after,
# so op-node's derivation clock and the engine's genesis agree.
GENESIS_TIMESTAMP=$(cast block "$L1_GENESIS_NUMBER" --rpc-url "http://127.0.0.1:$L1_PORT" --json | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["timestamp"],16))')
export L1_GENESIS_HASH L1_GENESIS_NUMBER GENESIS_TIMESTAMP
export BATCH_INBOX_ADDRESS DEPOSIT_CONTRACT_ADDRESS L1_SYSTEM_CONFIG_ADDRESS
export BASE_FEE_SCALAR BLOB_BASE_FEE_SCALAR
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

echo "generating rollup.json anchored to L1 block $L1_GENESIS_NUMBER ($L1_GENESIS_HASH)" >&2
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
run_op op-node sequencer "$OPNODE_RPC_PORT" "$LOG_DIR/op-node.log" \
  --l1="http://$HOST_ADDR:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
  --rollup.l1-chain-config="$L1_CHAIN_CONFIG_ARG" \
  --l2="http://$HOST_ADDR:${ENGINE_ADDR##*:}" --l2.jwt-secret="$JWT_ARG" \
  --rollup.config="$ROLLUP_CONFIG_ARG" \
  --sequencer.enabled --sequencer.l1-confs=0 --verifier.l1-confs=0 \
  --p2p.disable --l1.beacon.ignore \
  --rpc.addr="$BIND_ADDR" --rpc.port="$OPNODE_RPC_PORT"

if [ "$WITH_BATCHER" = "1" ]; then
  # Batches go to L1 as calldata: blobs would need a beacon endpoint, and this
  # devnet deliberately runs without one.
  echo "starting op-batcher (calldata mode)" >&2
  run_op op-batcher batcher "$BATCHER_RPC_PORT" "$LOG_DIR/op-batcher.log" \
    --l1-eth-rpc="http://$HOST_ADDR:$L1_PORT" \
    --l2-eth-rpc="http://$HOST_ADDR:${HTTP_ADDR##*:}" \
    --rollup-rpc="$SEQUENCER_RPC" \
    --private-key="$BATCHER_KEY" \
    --data-availability-type=calldata \
    --sub-safety-margin=4 --poll-interval=2s --num-confirmations=1 \
    --max-channel-duration=2 \
    --rpc.addr="$BIND_ADDR" --rpc.port="$BATCHER_RPC_PORT"
fi

if [ "$WITH_PROPOSER" = "1" ] && [ -n "$DISPUTE_GAME_FACTORY_ADDRESS" ]; then
  # op-proposer reads optimism_outputAtBlock from op-node and creates a game
  # per proposal. Nothing about it is op-cartesi-specific: the output root it
  # submits already commits to the Cartesi outputs tree, because op-node builds
  # it from the header's withdrawalsRoot.
  #
  # --allow-non-finalized because anvil has no beacon chain to finalize
  # anything, so a proposer that waited for finality would never propose.
  echo "starting op-proposer (game type $DISPUTE_GAME_TYPE)" >&2
  run_op op-proposer proposer "$PROPOSER_RPC_PORT" "$LOG_DIR/op-proposer.log" \
    --l1-eth-rpc="http://$HOST_ADDR:$L1_PORT" \
    --rollup-rpc="$SEQUENCER_RPC" \
    --game-factory-address="$DISPUTE_GAME_FACTORY_ADDRESS" \
    --game-type="$DISPUTE_GAME_TYPE" \
    --private-key="$PROPOSER_KEY" \
    --proposal-interval=20s --poll-interval=4s \
    --allow-non-finalized --wait-node-sync \
    --rpc.addr="$BIND_ADDR" --rpc.port="$PROPOSER_RPC_PORT"
fi

if [ "$WITH_VERIFIER" = "1" ]; then
  # A second node that never sequences: it learns the chain only from what the
  # batcher posted to L1, which is the property that makes the chain a rollup
  # rather than a database with an RPC.
  echo "starting verifier: second machine server, engine and op-node" >&2
  start_machine_server "$VERIFIER_MACHINE_PORT" "$LOG_DIR/machine-verifier.log"
  # Its own store: a pebble database is held by one process, and pointing two
  # nodes at one directory fails with a message about resources rather than
  # about sharing.
  MACHINE_REMOTE="http://127.0.0.1:$VERIFIER_MACHINE_PORT" \
  ENGINE_ADDR="$VERIFIER_ENGINE_ADDR" HTTP_ADDR="$VERIFIER_HTTP_ADDR" \
  DATA_DIR="${DATA_DIR:+$DATA_DIR-verifier}" \
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
  run_op op-node verifier "$VERIFIER_OPNODE_RPC_PORT" "$LOG_DIR/op-node-verifier.log" \
    --l1="http://$HOST_ADDR:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
    --rollup.l1-chain-config="$L1_CHAIN_CONFIG_ARG" \
    --l2="http://$HOST_ADDR:${VERIFIER_ENGINE_ADDR##*:}" --l2.jwt-secret="$JWT_ARG" \
    --rollup.config="$ROLLUP_CONFIG_ARG" \
    --verifier.l1-confs=0 --p2p.disable --l1.beacon.ignore \
    --rpc.addr="$BIND_ADDR" --rpc.port="$VERIFIER_OPNODE_RPC_PORT"
fi

echo >&2
echo "stack is up ($OP_RUNTIME op-node/op-batcher). logs in $LOG_DIR" >&2
echo "  L1     http://127.0.0.1:$L1_PORT" >&2
echo "  L2     http://127.0.0.1:${HTTP_ADDR##*:}" >&2
echo "  opnode http://127.0.0.1:$OPNODE_RPC_PORT" >&2
if [ "$WITH_VERIFIER" = "1" ]; then
  echo "  verifier L2     http://127.0.0.1:${VERIFIER_HTTP_ADDR##*:}" >&2
  echo "  verifier opnode http://127.0.0.1:$VERIFIER_OPNODE_RPC_PORT" >&2
fi
echo "watch blocks:  cast block-number --rpc-url http://127.0.0.1:${HTTP_ADDR##*:}" >&2
echo "ctrl-c to tear the stack down" >&2
wait
