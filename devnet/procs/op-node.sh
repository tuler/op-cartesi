#!/usr/bin/env bash
# op-node in sequencer mode: it drives block production, and every L2 block
# carries the L1-attributes deposit it injects.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init op-node

wait_ready genesis "the rollup config"
# op-cartesi binds its listeners only after the chain is open and the machine
# has booted, so a port that answers is an engine that can serve.
wait_for_port "${ENGINE_ADDR##*:}" "the engine"

resolve_op_runtime
devnet_say "op-node (sequencer, $OP_RUNTIME) rpc :$OPNODE_RPC_PORT"
run_op op-node op-node "$OPNODE_RPC_PORT" \
  --l1="http://$HOST_ADDR:$L1_PORT" --l1.rpckind=basic --l1.trustrpc \
  --rollup.l1-chain-config="$L1_CHAIN_CONFIG_ARG" \
  --l2="http://$HOST_ADDR:${ENGINE_ADDR##*:}" --l2.jwt-secret="$JWT_ARG" \
  --rollup.config="$ROLLUP_CONFIG_ARG" \
  --sequencer.enabled --sequencer.l1-confs=0 --verifier.l1-confs=0 \
  --p2p.disable --l1.beacon.ignore \
  --rpc.addr="$BIND_ADDR" --rpc.port="$OPNODE_RPC_PORT"
