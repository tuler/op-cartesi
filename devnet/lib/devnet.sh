#!/usr/bin/env bash
# shellcheck shell=bash
# Shared plumbing for the devnet processes.
#
# Every script under devnet/procs/ sources this file, and so does
# start-devnet.sh. It holds three things:
#
#   - the devnet's topology (ports, which optional pieces are on),
#   - the waiting primitives each process uses to hold off until what it
#     depends on is actually up,
#   - the native-or-docker decision for the OP tools, and the addressing that
#     follows from it.
#
# The waiting is what replaces the sequential bring-up the single start script
# used to do. mprocs starts every process at once and has no notion of
# dependencies, so each process waits for its own: op-node blocks until the
# engine's port answers, the engine blocks until rollup.json exists, and so
# on. That is what makes each pane independently restartable — pressing `r` on
# op-node re-runs exactly the same waits.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/env.sh"

# --- topology ---------------------------------------------------------------
: "${L1_PORT:=8600}"
: "${L1_BLOCK_TIME:=2}"
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
: "${BATCHER_RPC_PORT:=8548}"
: "${PROPOSER_RPC_PORT:=8560}"

# The optional pieces. Each one is a pane of its own, so turning one off
# removes a pane rather than changing what the remaining ones do.
: "${WITH_BATCHER:=1}"
: "${WITH_VERIFIER:=1}"
# Deploy the OP L1 contract suite with op-deployer. Set to 0 for the older,
# faster bring-up with placeholder addresses and no contracts.
: "${WITH_CONTRACTS:=1}"
: "${WITH_PROPOSER:=1}"
# The guest's own account of what it did with each transaction, read back from
# the chain through cartesi_getTransactionEmissions. Needs bun.
: "${WITH_GUEST_LOG:=1}"

# The routed guest's snapshot, as `cartesi build` stores it
# (./devnet/build-snapshot.sh wraps that build).
: "${SNAPSHOT_DIR:=$REPO_DIR/ledger-app/.cartesi/image}"
: "${LOG_DIR:=$DEVNET_DIR/logs}"
# Where the processes leave notes for each other: readiness markers, the
# generated mprocs config, and one pid file per running process.
: "${STATE_DIR:=$DEVNET_DIR/.state}"
: "${L1_CHAIN_CONFIG:=$DEVNET_DIR/l1-chain-config.json}"
# Written by procs/genesis.sh, sourced by env.sh: the L1 block the rollup is
# anchored at and the L2 genesis timestamp derived from it. Both are
# consensus-relevant and both have to reach the engine, which is a different
# process from the one that computed them.
: "${CHAIN_GENESIS_FILE:=$DEVNET_DIR/chain-genesis.env}"
# How long a process waits for something it depends on. Deploying the L1
# suite takes about a minute and booting Linux inside the machine can take
# two, so this is generous on purpose.
: "${WAIT_TIMEOUT:=600}"

CONTAINER_PREFIX="op-cartesi-devnet"
DOCKER_NETWORK="op-cartesi-devnet"

# --- pane output ------------------------------------------------------------
# Everything a process has to say about itself goes to stderr with a marker,
# so its own narration is distinguishable from the output of the program it
# supervises — which is the whole content of the pane below it.
devnet_say() { echo "==> $*" >&2; }
devnet_die() { echo "!!! $*" >&2; exit 1; }

# --- process bookkeeping ----------------------------------------------------
# mprocs starts each process in a session of its own and stops it by killing
# that whole process group, which is exactly right for the machine servers:
# op-cartesi forks one per block and the forks it prunes reparent to init,
# out of reach of any parent-to-child walk, but never out of their process
# group.
#
# What that leaves uncovered is mprocs itself being killed rather than quit.
# So each process records its group here, and stop-devnet.sh (which
# start-devnet.sh also runs on the way out) kills whatever is left — after
# checking the pid still belongs to a devnet process, since a recorded pid
# that has been recycled belongs to someone else.
proc_init() {
  PROC_NAME=$1
  mkdir -p "$STATE_DIR/pids"
  # The start time goes in alongside the pid: a pid on its own is not an
  # identity, and by the time anything reads this file the process it names
  # may be someone else's. The pair is checked before any signal is sent.
  printf '%s\t%s\n' "$$" "$(proc_started_at $$)" > "$STATE_DIR/pids/$PROC_NAME"
}

# Empty for a pid that is not running. Written as an assignment rather than a
# pipeline so that `ps` failing on a dead pid is an empty answer and not, under
# `set -o pipefail`, the end of the caller.
proc_started_at() {
  local when
  when=$(ps -o lstart= -p "$1" 2>/dev/null) || return 0
  echo "$when" | tr -s ' ' | sed 's/^ *//;s/ *$//'
}

# stop_recorded_procs — signals the process group of everything still recorded
# as running, skipping any pid that is gone or has been recycled.
#
# Groups rather than pids, because op-cartesi forks a machine server per block
# and the ones it prunes reparent to init: out of reach of a parent-to-child
# walk, but never out of their process group. mprocs already does this when it
# stops a process; this is for the case where mprocs itself was killed rather
# than quit, and for `./devnet/stop-devnet.sh` after the fact.
stop_recorded_procs() {
  local signal=${1:-TERM}
  local file name pid started now stopped=()
  [ -d "$STATE_DIR/pids" ] || return 0
  for file in "$STATE_DIR"/pids/*; do
    [ -e "$file" ] || continue
    name=$(basename "$file")
    IFS=$'\t' read -r pid started < "$file" || true
    now=$(proc_started_at "${pid:-0}")
    # Gone, or the pid belongs to someone else now: forget it either way.
    if [ -z "$now" ] || [ "$now" != "${started:-}" ]; then
      rm -f "$file"
      continue
    fi
    kill "-$signal" -- "-$pid" 2>/dev/null || kill "-$signal" "$pid" 2>/dev/null || true
    stopped+=("$name")
  done
  if [ ${#stopped[@]} -gt 0 ]; then
    devnet_say "sent SIG$signal to: ${stopped[*]}"
    sleep 1
  fi
}

# The recorded processes that are still alive, by pane name.
running_procs() {
  local file name pid started
  [ -d "$STATE_DIR/pids" ] || return 0
  for file in "$STATE_DIR"/pids/*; do
    [ -e "$file" ] || continue
    name=$(basename "$file")
    IFS=$'\t' read -r pid started < "$file" || true
    [ "$(proc_started_at "${pid:-0}")" = "${started:-}" ] && echo "$name"
  done
  return 0
}

# Containers are not children of anything here, so they go by name — and the
# names are prefixed, so this only ever touches what this devnet started.
remove_containers() {
  command -v docker > /dev/null 2>&1 || return 0
  local names
  names=$(docker ps -aq --filter "name=^${CONTAINER_PREFIX}-" 2>/dev/null || true)
  if [ -n "$names" ]; then
    # shellcheck disable=SC2086
    docker rm -f $names > /dev/null 2>&1 || true
    devnet_say "removed the ${CONTAINER_PREFIX}-* containers"
  fi
  docker network rm "$DOCKER_NETWORK" > /dev/null 2>&1 || true
}

# --- readiness --------------------------------------------------------------
mark_ready() {
  mkdir -p "$STATE_DIR"
  : > "$STATE_DIR/$1.ready"
}

clear_ready() { rm -f "$STATE_DIR/$1.ready"; }

# wait_ready <name> <description> — blocks until the named step has finished.
# Steps that produce files (rollup.json, l1-addresses.env) are announced with
# a marker rather than by the file appearing, because a file exists from its
# first byte and these are read whole by whoever waits.
wait_ready() {
  local marker="$STATE_DIR/$1.ready" waited=0
  [ -e "$marker" ] && return 0
  devnet_say "waiting for $2"
  while [ ! -e "$marker" ]; do
    sleep 0.5
    waited=$((waited + 1))
    if [ "$waited" -gt $((WAIT_TIMEOUT * 2)) ]; then
      devnet_die "gave up after ${WAIT_TIMEOUT}s waiting for $2"
    fi
  done
}

port_free() {
  ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# wait_for_port <port> <description> — blocks until something answers on the
# port. For op-cartesi this is an exact readiness signal and not an
# approximation: it binds its listeners only after the chain is open and the
# machine has booted, so a port that answers is an engine that can serve.
wait_for_port() {
  local port=$1 what=$2 waited=0
  port_free "$port" || return 0
  devnet_say "waiting for $what on :$port"
  while port_free "$port"; do
    sleep 0.5
    waited=$((waited + 1))
    if [ "$waited" -gt $((WAIT_TIMEOUT * 2)) ]; then
      devnet_die "gave up after ${WAIT_TIMEOUT}s waiting for $what on :$port"
    fi
  done
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
    echo "stop them (./devnet/stop-devnet.sh, or set the matching *_PORT variables) and try again" >&2
    exit 1
  fi
}

# The deploy scripts write their results to files that env.sh sources, but a
# process that started before the deploy finished sourced them when they were
# not there yet. Re-reading after the wait is what picks the addresses up.
reload_addresses() {
  local f
  for f in l1-addresses.env outputs-addresses.env "$(basename "$CHAIN_GENESIS_FILE")"; do
    # shellcheck disable=SC1090
    [ -f "$DEVNET_DIR/$f" ] && source "$DEVNET_DIR/$f"
  done
  return 0
}

# --- native or docker -------------------------------------------------------
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
#
# start-devnet.sh resolves this once and exports the result, so the processes
# it starts inherit a decision rather than each making their own. Running a
# process script by hand re-derives it, which lands in the same place.
op_tools() {
  local tools=(op-node)
  [ "$WITH_BATCHER" = "1" ] && tools+=(op-batcher)
  [ "$WITH_PROPOSER" = "1" ] && [ "$WITH_CONTRACTS" = "1" ] && tools+=(op-proposer)
  echo "${tools[@]}"
}

resolve_op_runtime() {
  local tool tools
  read -r -a tools <<< "$(op_tools)"
  if [ "$OP_RUNTIME" = "auto" ]; then
    OP_RUNTIME=native
    for tool in "${tools[@]}"; do
      command -v "$tool" > /dev/null 2>&1 || OP_RUNTIME=docker
    done
    if [ "$OP_RUNTIME" = "docker" ] && ! command -v docker > /dev/null 2>&1; then
      devnet_die "need docker, or all of: ${tools[*]} on PATH"
    fi
  elif [ "$OP_RUNTIME" = "native" ]; then
    for tool in "${tools[@]}"; do
      command -v "$tool" > /dev/null 2>&1 || devnet_die "OP_RUNTIME=native but $tool is not on PATH"
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
    # Exported, because the engine processes read these from the environment
    # and would otherwise fall back to env.sh's loopback defaults — which the
    # containers cannot reach. Rebinding is idempotent, so a process that
    # inherits an already-rebound value lands on the same address.
    export ENGINE_ADDR="$BIND_ADDR:${ENGINE_ADDR##*:}"
    export HTTP_ADDR="$BIND_ADDR:${HTTP_ADDR##*:}"
    export VERIFIER_ENGINE_ADDR="$BIND_ADDR:${VERIFIER_ENGINE_ADDR##*:}"
    export VERIFIER_HTTP_ADDR="$BIND_ADDR:${VERIFIER_HTTP_ADDR##*:}"
    # op-batcher talks to op-node, and both are containers, so they share a
    # user-defined network and address each other by name. Going back out
    # through the host would mean publishing op-node's RPC on every interface
    # rather than just loopback — a port bound to the host's loopback is not
    # reachable from the bridge gateway, which is exactly how this first failed.
    SEQUENCER_RPC="http://$CONTAINER_PREFIX-op-node:$OPNODE_RPC_PORT"
  else
    HOST_ADDR="127.0.0.1"
    BIND_ADDR="127.0.0.1"
    ROLLUP_CONFIG_ARG="$ROLLUP_CONFIG_FILE"
    JWT_ARG="$JWT_SECRET_FILE"
    L1_CHAIN_CONFIG_ARG="$L1_CHAIN_CONFIG"
    SEQUENCER_RPC="http://127.0.0.1:$OPNODE_RPC_PORT"
  fi
  export OP_RUNTIME DEPLOYER_IN_DOCKER L1_BIND_ADDR HOST_ADDR BIND_ADDR
  export ROLLUP_CONFIG_ARG JWT_ARG L1_CHAIN_CONFIG_ARG SEQUENCER_RPC
}

# run_op <kind> <name> <rpc-port> [args...] — runs one OP tool in the
# foreground, so mprocs supervises it and its output is the pane.
#
# Natively that is an exec. In docker the container is not a child of
# anything, so killing the client would leave it running: the trap removes it
# by name on the way out, and the name is prefixed so this only ever touches
# containers this devnet started.
run_op() {
  local kind=$1 name=$2 rpc_port=$3
  shift 3
  if [ "$OP_RUNTIME" != "docker" ]; then
    exec "$kind" "$@"
  fi

  local image=$OP_NODE_IMAGE
  [ "$kind" = "op-batcher" ] && image=$OP_BATCHER_IMAGE
  [ "$kind" = "op-proposer" ] && image=$OP_PROPOSER_IMAGE
  local container="$CONTAINER_PREFIX-$name"
  docker network create "$DOCKER_NETWORK" > /dev/null 2>&1 || true
  docker rm -f "$container" > /dev/null 2>&1 || true
  # shellcheck disable=SC2064
  trap "docker rm -f '$container' > /dev/null 2>&1 || true" EXIT INT TERM
  docker run --rm --name "$container" \
    --network "$DOCKER_NETWORK" \
    --add-host="host.docker.internal:host-gateway" \
    -p "127.0.0.1:$rpc_port:$rpc_port" \
    -v "$ROLLUP_CONFIG_FILE:$ROLLUP_CONFIG_ARG:ro" \
    -v "$JWT_SECRET_FILE:$JWT_ARG:ro" \
    -v "$L1_CHAIN_CONFIG:$L1_CHAIN_CONFIG_ARG:ro" \
    "$image" "$kind" "$@" &
  wait $!
}

# start_machine_server <port> — a cartesi-jsonrpc-machine in the background of
# whoever calls it, for the steps that need a machine briefly rather than for
# the run's duration. Sets MACHINE_SERVER_PID.
start_machine_server() {
  local port=$1
  cartesi-jsonrpc-machine --server-address="127.0.0.1:$port" &
  MACHINE_SERVER_PID=$!
  wait_for_port "$port" "machine server"
}

# Walking parent to child, for the one server this stack starts and stops
# again mid-run. It is not how the long-lived servers are torn down — those
# are a process group, which survives the emulator's forks being reparented
# to init — but the genesis server has no forks to lose.
kill_tree() {
  local pid=$1 kids kid
  kids=$(pgrep -P "$pid" 2>/dev/null || true)
  kill "$pid" 2>/dev/null || true
  for kid in $kids; do kill_tree "$kid"; done
}
