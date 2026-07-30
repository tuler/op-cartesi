#!/usr/bin/env bash
# Starts op-cartesi: the authenticated Engine API for op-node, and the public
# eth_* RPC for everything else.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

ensure_jwt
cd "$REPO_DIR"

exec go run ./cmd/op-cartesi run \
  "${CHAIN_FLAGS[@]}" \
  -engine.addr "$ENGINE_ADDR" \
  -http.addr "$HTTP_ADDR" \
  -engine.jwt-secret "$JWT_SECRET_FILE"
