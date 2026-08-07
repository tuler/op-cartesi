#!/usr/bin/env bash
# op-proposer reads optimism_outputAtBlock from op-node and creates a game per
# proposal. Nothing about it is op-cartesi-specific: the output root it submits
# already commits to the Cartesi outputs tree, because op-node builds it from
# the header's withdrawalsRoot.
#
# --allow-non-finalized because anvil has no beacon chain to finalize anything,
# so a proposer that waited for finality would never propose.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init op-proposer

wait_ready l1-contracts "the DisputeGameFactory"
reload_addresses
wait_for_port "$OPNODE_RPC_PORT" "op-node"

if [ -z "${DISPUTE_GAME_FACTORY_ADDRESS:-}" ]; then
  devnet_die "no DISPUTE_GAME_FACTORY_ADDRESS in $DEVNET_DIR/l1-addresses.env"
fi

resolve_op_runtime
devnet_say "op-proposer ($OP_RUNTIME, game type $DISPUTE_GAME_TYPE) rpc :$PROPOSER_RPC_PORT"
run_op op-proposer op-proposer "$PROPOSER_RPC_PORT" \
  --l1-eth-rpc="http://$HOST_ADDR:$L1_PORT" \
  --rollup-rpc="$SEQUENCER_RPC" \
  --game-factory-address="$DISPUTE_GAME_FACTORY_ADDRESS" \
  --game-type="$DISPUTE_GAME_TYPE" \
  --private-key="$PROPOSER_KEY" \
  --proposal-interval=20s --poll-interval=4s \
  --allow-non-finalized --wait-node-sync \
  --rpc.addr="$BIND_ADDR" --rpc.port="$PROPOSER_RPC_PORT"
