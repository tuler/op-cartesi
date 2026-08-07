#!/usr/bin/env bash
# Brings up the whole stack — anvil as L1, the OP Stack L1 contracts, a
# Cartesi Machine, op-cartesi as the execution engine, op-node sequencing on
# top, a batcher, a proposer, and a second node verifying from L1 alone — with
# every piece running in a pane of its own under mprocs.
#
# This script does not start any of them. It checks that the run can succeed,
# writes the mprocs config for the pieces this run wants, and hands over. The
# processes themselves are one script each under devnet/procs/, and each waits
# for what it depends on rather than being started in order — which is what
# makes any single one restartable from the UI (`r`) without restarting the
# stack.
#
#   ./devnet/start-devnet.sh                 the lot
#   WITH_CONTRACTS=0 ./devnet/start-devnet.sh   no L1 contracts: blocks and
#                                               derivation, no deposits
#   WITH_VERIFIER=0 WITH_BATCHER=0 ./devnet/start-devnet.sh   sequencer only
#
# Prerequisites: anvil, cast, cartesi-jsonrpc-machine, go, python3, bun, and
# either op-node/op-batcher/op-proposer on PATH or docker. Plus a machine
# snapshot: ./devnet/build-snapshot.sh, once.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/devnet.sh"

case "${1:-}" in
  -h|--help)
    # The header comment above, which is the usage: everything from the line
    # after the shebang up to the first line that is not a comment.
    awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
    exit 0
    ;;
esac

: "${MPROCS_CONFIG:=$STATE_DIR/mprocs.yaml}"

# --- what this run will start -----------------------------------------------
resolve_op_runtime

# mprocs comes from the repo's own node_modules first, so everyone runs the
# version bun install pinned; a global one, or bunx, is the fallback.
resolve_mprocs() {
  if [ -n "${MPROCS:-}" ]; then MPROCS_CMD=("$MPROCS")
  elif [ -x "$DEVNET_DIR/node_modules/.bin/mprocs" ]; then MPROCS_CMD=("$DEVNET_DIR/node_modules/.bin/mprocs")
  elif [ -x "$REPO_DIR/node_modules/.bin/mprocs" ]; then MPROCS_CMD=("$REPO_DIR/node_modules/.bin/mprocs")
  elif command -v mprocs > /dev/null 2>&1; then MPROCS_CMD=(mprocs)
  elif command -v bunx > /dev/null 2>&1; then MPROCS_CMD=(bunx mprocs)
  elif command -v npx > /dev/null 2>&1; then MPROCS_CMD=(npx --yes mprocs)
  else
    devnet_die "mprocs not found: run \`bun install\` at the repo root, or npm i -g mprocs"
  fi
}
resolve_mprocs

# --- preflight --------------------------------------------------------------
# Everything that can be known to fail before any process starts is checked
# here, because a missing binary discovered by pane 7 four minutes in reads as
# a devnet that is broken rather than a laptop that is missing a tool.
require_commands() {
  local cmd missing=()
  for cmd in "$@"; do
    command -v "$cmd" > /dev/null 2>&1 || missing+=("$cmd")
  done
  [ ${#missing[@]} -eq 0 ] || devnet_die "not on PATH: ${missing[*]}"
}

# mprocs is a terminal UI and says so itself — but only after everything below
# has been checked and built, which is a long way to go to be told that.
if [ ! -t 0 ] || [ ! -t 1 ]; then
  devnet_die "mprocs needs a terminal; run this from an interactive shell (the individual processes under devnet/procs/ can be run on their own)"
fi

# Bringing a second devnet up over a running one would delete the records the
# first is stopped from, leaving it running and unstoppable. The port check
# below usually catches this first — but not if the running one was started on
# other ports.
ALREADY=$(running_procs | tr '\n' ' ')
if [ -n "${ALREADY// /}" ]; then
  devnet_die "a devnet is already running ($ALREADY) — ./devnet/stop-devnet.sh first"
fi

require_commands anvil cast cartesi-jsonrpc-machine go python3
[ "$WITH_GUEST_LOG" = "1" ] && require_commands bun
if [ "$OP_RUNTIME" = "docker" ] || [ "$DEPLOYER_IN_DOCKER" = "1" ]; then
  require_commands docker
fi

if [ ! -d "$SNAPSHOT_DIR" ]; then
  devnet_die "no machine snapshot at $SNAPSHOT_DIR — run ./devnet/build-snapshot.sh first"
fi

# op-node needs the secret before op-cartesi would otherwise create it, and in
# docker it is a bind mount, which docker would silently create as a directory
# if the file were missing.
ensure_jwt

REQUIRED_PORTS=("$L1_PORT" "$MACHINE_PORT" "$GENESIS_MACHINE_PORT" "$OPNODE_RPC_PORT" "${ENGINE_ADDR##*:}" "${HTTP_ADDR##*:}")
[ "$WITH_BATCHER" = "1" ] && REQUIRED_PORTS+=("$BATCHER_RPC_PORT")
[ "$WITH_PROPOSER" = "1" ] && [ "$WITH_CONTRACTS" = "1" ] && REQUIRED_PORTS+=("$PROPOSER_RPC_PORT")
[ "$WITH_VERIFIER" = "1" ] && REQUIRED_PORTS+=("$VERIFIER_MACHINE_PORT" "$VERIFIER_OPNODE_RPC_PORT" "${VERIFIER_ENGINE_ADDR##*:}" "${VERIFIER_HTTP_ADDR##*:}")
require_free_ports "${REQUIRED_PORTS[@]}"

# Compiled once, here, rather than `go run` in three panes: the panes then
# hold the engine itself, so mprocs stops the engine when it stops the pane
# instead of stopping a toolchain wrapper and leaving its child behind. It
# also means a compile error shows up now, not as a pane that exits.
export OP_CARTESI_BIN="$REPO_DIR/bin/op-cartesi"
devnet_say "building op-cartesi"
mkdir -p "$(dirname "$OP_CARTESI_BIN")"
(cd "$REPO_DIR" && go build -o "$OP_CARTESI_BIN" ./cmd/op-cartesi)

# --- a run starts from nothing ----------------------------------------------
# A fresh anvil forgets every deployment, so every address and anchor a
# previous run recorded is stale the moment it boots. Removing the files makes
# the scripts say "run ./devnet/deploy-outputs.sh first" instead of no-op'ing
# transactions against codeless addresses.
rm -rf "$STATE_DIR" "$LOG_DIR"
mkdir -p "$STATE_DIR" "$LOG_DIR"
rm -f "$DEVNET_DIR/l1-addresses.env" "$DEVNET_DIR/outputs-addresses.env" \
  "$CHAIN_GENESIS_FILE" "$ROLLUP_CONFIG_FILE"

if [ "$OP_RUNTIME" = "docker" ]; then
  remove_containers
  docker network create "$DOCKER_NETWORK" > /dev/null 2>&1 || true
fi

# --- the panes --------------------------------------------------------------
# Written rather than checked in, because which processes exist is a function
# of the WITH_* flags: a devnet without a batcher should not show a batcher
# pane that is permanently down. Order is the order of the pipeline, and
# mprocs keeps it.
proc_entry() {
  local name=$1 autostart=${2:-true}
  cat >> "$MPROCS_CONFIG" <<YAML
  $name:
    autostart: $autostart
    cmd: ["$DEVNET_DIR/procs/$name.sh"]
YAML
}

cat > "$MPROCS_CONFIG" <<YAML
# Generated by devnet/start-devnet.sh — edits are overwritten on the next run.
proc_list_title: op-cartesi devnet
scrollback: 20000
procs:
YAML

proc_entry info
proc_entry l1
[ "$WITH_CONTRACTS" = "1" ] && proc_entry l1-contracts
proc_entry genesis
proc_entry machine
proc_entry engine
[ "$WITH_GUEST_LOG" = "1" ] && proc_entry guest
proc_entry op-node
[ "$WITH_BATCHER" = "1" ] && proc_entry op-batcher
[ "$WITH_PROPOSER" = "1" ] && [ "$WITH_CONTRACTS" = "1" ] && proc_entry op-proposer
if [ "$WITH_VERIFIER" = "1" ]; then
  proc_entry verifier-machine
  proc_entry verifier-engine
  proc_entry verifier-node
fi
# The one step that waits for you: deploying the outputs suite needs a
# proposal to exist, and not every run wants assets. Press `s` on it.
[ "$WITH_CONTRACTS" = "1" ] && proc_entry outputs false

# --- hand over --------------------------------------------------------------
# The endpoint summary is the `info` pane rather than something printed here:
# mprocs takes over the whole screen, so anything written now is gone the
# moment it starts.
devnet_say "starting mprocs ($OP_RUNTIME OP tools); logs also go to $LOG_DIR/<pane>.log"

# mprocs stops every process it started when it quits, killing each one's
# whole process group — which is what the machine servers need, since the
# forks op-cartesi prunes reparent to init and out of reach of any walk. The
# trap covers the other ending: mprocs killed rather than quit.
trap '"$DEVNET_DIR/stop-devnet.sh" > /dev/null 2>&1 || true' EXIT

"${MPROCS_CMD[@]}" --config "$MPROCS_CONFIG" --log-dir "$LOG_DIR"
