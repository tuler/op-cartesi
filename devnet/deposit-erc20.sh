#!/usr/bin/env bash
# Deposits ERC-20 tokens through OPERC20Portal.
#
#   ./devnet/deposit-erc20.sh [amount] [token]
#
# Deploys a test token on first use and records it in devnet/token.env.
#
# The portal escrows the tokens in the application contract — the same contract
# a voucher later runs from — and hands the guest Cartesi's own packed deposit
# payload, carried as the data of an OptimismPortal deposit. Nothing here
# touches L1StandardBridge: that bridge escrows in itself and releases only
# against an OptimismPortal withdrawal proof, which this chain cannot produce.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
: "${DEPOSITOR_KEY:=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d}"
: "${TOKEN_ENV_FILE:=$DEVNET_DIR/token.env}"

AMOUNT="${1:-1000000000000000000}"
TOKEN="${2:-${TEST_TOKEN_ADDRESS:-}}"

if [ -z "${ERC20_PORTAL_ADDRESS:-}" ] || [ -z "${OUTPUT_EXECUTOR_ADDRESS:-}" ]; then
  echo "run ./devnet/deploy-outputs.sh first" >&2
  exit 1
fi

DEPOSITOR=$(cast wallet address --private-key "$DEPOSITOR_KEY")

if [ -z "$TOKEN" ]; then
  echo "deploying a test token" >&2
  cd "$REPO_DIR/contracts"
  OUT=$(forge script script/DeployTestToken.s.sol:DeployTestToken \
    --rpc-url "$L1_RPC" --private-key "$DEPOSITOR_KEY" --broadcast 2>&1)
  TOKEN=$(echo "$OUT" | grep -oE 'TEST_TOKEN_ADDRESS=0x[0-9a-fA-F]{40}' | head -1 | cut -d= -f2)
  if [ -z "$TOKEN" ]; then
    echo "$OUT" | tail -20 >&2
    exit 1
  fi
  {
    echo "# Written by devnet/deposit-erc20.sh."
    echo "TEST_TOKEN_ADDRESS=$TOKEN"
  } > "$TOKEN_ENV_FILE"
  echo "  token $TOKEN, supply held by $DEPOSITOR" >&2
fi

echo "depositing $AMOUNT of $TOKEN through the portal at $ERC20_PORTAL_ADDRESS" >&2
cast send "$TOKEN" 'approve(address,uint256)' "$ERC20_PORTAL_ADDRESS" "$AMOUNT" \
  --private-key "$DEPOSITOR_KEY" --rpc-url "$L1_RPC" > /dev/null
cast send "$ERC20_PORTAL_ADDRESS" 'depositERC20Tokens(address,address,uint256,bytes)' \
  "$TOKEN" "$OUTPUT_EXECUTOR_ADDRESS" "$AMOUNT" 0x \
  --private-key "$DEPOSITOR_KEY" --rpc-url "$L1_RPC" > /dev/null

echo "  escrowed in the application: $(cast call "$TOKEN" 'balanceOf(address)(uint256)' "$OUTPUT_EXECUTOR_ADDRESS" --rpc-url "$L1_RPC" | awk '{print $1}')" >&2
echo >&2
echo "the guest credits $DEPOSITOR once the chain derives the deposit; check with:" >&2
echo "  ./devnet/balance.sh $DEPOSITOR $TOKEN" >&2
