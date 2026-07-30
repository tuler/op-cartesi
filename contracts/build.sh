#!/usr/bin/env bash
# Compiles the L1 contracts with solc in docker, because there is no forge here
# and the OP monorepo's own toolchain is more than this needs.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${SOLC_IMAGE:=ethereum/solc:stable}"
OUT="$DIR/out"
rm -rf "$OUT"; mkdir -p "$OUT"
docker run --rm -v "$DIR:/c" -w /c "$SOLC_IMAGE" \
  --base-path /c \
  "@openzeppelin-contracts-5.2.0/=oz/" \
  --optimize --combined-json abi,bin \
  src/OPOutputsMerkleRootValidator.sol src/OutputExecutor.sol test/TestERC20.sol > "$OUT/contracts.json"
echo "wrote $OUT/contracts.json" >&2
