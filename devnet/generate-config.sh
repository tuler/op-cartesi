#!/usr/bin/env bash
# Generates the rollup.json op-node needs to derive this chain. The chain flags
# must match those used by start-shim.sh, or op-node will reject the engine for
# serving a different genesis block.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

cd "$REPO_DIR"
"${OP_CARTESI[@]}" genesis \
  "${CHAIN_FLAGS[@]}" \
  -out "$ROLLUP_CONFIG_FILE" \
  -l1.chain-id "$L1_CHAIN_ID" \
  -l1.genesis-hash "$L1_GENESIS_HASH" \
  -l1.genesis-number "$L1_GENESIS_NUMBER" \
  -block-time "$BLOCK_TIME" \
  -batcher-address "$BATCHER_ADDRESS" \
  -batch-inbox-address "$BATCH_INBOX_ADDRESS" \
  -deposit-contract-address "$DEPOSIT_CONTRACT_ADDRESS" \
  -l1-system-config-address "$L1_SYSTEM_CONFIG_ADDRESS" \
  -base-fee-scalar "$BASE_FEE_SCALAR" \
  -blob-base-fee-scalar "$BLOB_BASE_FEE_SCALAR"

echo "wrote $ROLLUP_CONFIG_FILE" >&2
