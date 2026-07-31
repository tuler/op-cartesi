#!/usr/bin/env bash
# Runs an ERC-20 withdrawal end to end.
#
#   ./devnet/withdraw-erc20.sh <recipient> <amount> [token]
#
# The only difference from the ether case is what the voucher says. There the
# call is "send the recipient this much ether"; here it is "token, transfer
# this much to the recipient" — one call from the application contract, which
# has held the tokens since the deposit. Everything downstream is identical,
# because to the outputs tree a voucher is a voucher.
#
# This is the direction OP's standard bridge cannot serve on this chain: its
# release path runs through OptimismPortal.proveWithdrawalTransaction, which
# wants an MPT proof against an L2 state root that here is a Cartesi hash tree.
# See DESIGN §7d.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"

TO="${1:?usage: withdraw-erc20.sh <recipient> <amount> [token]}"
AMOUNT="${2:?usage: withdraw-erc20.sh <recipient> <amount> [token]}"
TOKEN="${3:-${TEST_TOKEN_ADDRESS:-}}"
if [ -z "$TOKEN" ]; then
  echo "no token given and no TEST_TOKEN_ADDRESS; run ./devnet/deposit-erc20.sh first" >&2
  exit 1
fi

# "t" ‖ token ‖ recipient ‖ amount.
REQUEST="0x74${TOKEN#0x}${TO#0x}$(cast to-uint256 "$AMOUNT" | sed 's/^0x//')"
echo "asking the guest to withdraw $AMOUNT of $TOKEN to $TO" >&2
TXHASH=$("$DEVNET_DIR/send-l2-tx.sh" "$REQUEST")
echo "  L2 tx $TXHASH" >&2

BEFORE=$(cast call "$TOKEN" 'balanceOf(address)(uint256)' "$TO" --rpc-url "$L1_RPC" | awk '{print $1}')
"$DEVNET_DIR/execute-voucher.sh" "$TXHASH" > /dev/null
AFTER=$(cast call "$TOKEN" 'balanceOf(address)(uint256)' "$TO" --rpc-url "$L1_RPC" | awk '{print $1}')

echo >&2
echo "withdrawal executed on L1" >&2
echo "  token     $TOKEN" >&2
echo "  recipient $TO" >&2
echo "  balance   $BEFORE -> $AFTER" >&2
