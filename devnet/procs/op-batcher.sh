#!/usr/bin/env bash
# op-batcher posts the sequenced blocks to L1 as calldata, which is what
# advances the safe head — and what the verifier rebuilds the chain from.
#
# Calldata rather than blobs: blobs would need a beacon endpoint, and this
# devnet deliberately runs without one.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init op-batcher

wait_for_port "${HTTP_ADDR##*:}" "the engine's eth RPC"
wait_for_port "$OPNODE_RPC_PORT" "op-node"

resolve_op_runtime
devnet_say "op-batcher ($OP_RUNTIME, calldata mode) rpc :$BATCHER_RPC_PORT"
run_op op-batcher op-batcher "$BATCHER_RPC_PORT" \
  --l1-eth-rpc="http://$HOST_ADDR:$L1_PORT" \
  --l2-eth-rpc="http://$HOST_ADDR:${HTTP_ADDR##*:}" \
  --rollup-rpc="$SEQUENCER_RPC" \
  --private-key="$BATCHER_KEY" \
  --data-availability-type=calldata \
  --sub-safety-margin=4 --poll-interval=2s --num-confirmations=1 \
  --max-channel-duration=2 \
  --rpc.addr="$BIND_ADDR" --rpc.port="$BATCHER_RPC_PORT"
