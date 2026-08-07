#!/usr/bin/env bash
# Starts op-cartesi: the authenticated Engine API for op-node, and the public
# eth_* RPC for everything else.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

ensure_jwt
cd "$REPO_DIR"

RUN_FLAGS=()
if [ -n "$DATA_DIR" ]; then
  RUN_FLAGS+=(-datadir "$DATA_DIR")
fi

exec "${OP_CARTESI[@]}" run \
  "${CHAIN_FLAGS[@]}" "${RUN_FLAGS[@]}" \
  -engine.addr "$ENGINE_ADDR" \
  -http.addr "$HTTP_ADDR" \
  -engine.jwt-secret "$JWT_SECRET_FILE"
