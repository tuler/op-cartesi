#!/usr/bin/env bash
# The verifier's op-node: same rollup config as the sequencer's, without
# --sequencer.enabled, so everything it knows comes from L1.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init verifier-node

wait_ready genesis "the rollup config"
wait_for_port "${VERIFIER_ENGINE_ADDR##*:}" "the verifier's engine"

resolve_op_runtime
devnet_say "op-node (verifier, $OP_RUNTIME) rpc :$VERIFIER_OPNODE_RPC_PORT"
run_op op-node verifier-node "$VERIFIER_OPNODE_RPC_PORT" \
  --l1="http://$HOST_ADDR:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
  --rollup.l1-chain-config="$L1_CHAIN_CONFIG_ARG" \
  --l2="http://$HOST_ADDR:${VERIFIER_ENGINE_ADDR##*:}" --l2.jwt-secret="$JWT_ARG" \
  --rollup.config="$ROLLUP_CONFIG_ARG" \
  --verifier.l1-confs=0 --p2p.disable --l1.beacon.ignore \
  --rpc.addr="$BIND_ADDR" --rpc.port="$VERIFIER_OPNODE_RPC_PORT"
