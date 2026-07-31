#!/usr/bin/env bash
# Runs an ether withdrawal end to end: ask the guest for one, then prove and
# execute the voucher it emits.
#
#   ./devnet/withdraw.sh <recipient> <wei>
#
# This is the whole point of the outputs tree. The guest emits a voucher, the
# tree root goes into the block's withdrawalsRoot, op-proposer's claim commits
# to that root, and Cartesi's own proof libraries verify the voucher against it
# on L1 — with no change to OptimismPortal, op-node, op-batcher or op-proposer.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"

TO="${1:?usage: withdraw.sh <recipient> <wei>}"
AMOUNT="${2:?usage: withdraw.sh <recipient> <wei>}"

# Ask the guest to withdraw: "w" ‖ recipient ‖ amount, as a plain L2
# transaction payload.
REQUEST="0x77${TO#0x}$(cast to-uint256 "$AMOUNT" | sed 's/^0x//')"
echo "asking the guest to withdraw $AMOUNT wei to $TO" >&2
TXHASH=$("$DEVNET_DIR/send-l2-tx.sh" "$REQUEST")
echo "  L2 tx $TXHASH" >&2

BALANCE_BEFORE=$(cast balance "$TO" --rpc-url "$L1_RPC")
"$DEVNET_DIR/execute-voucher.sh" "$TXHASH" > /dev/null
BALANCE_AFTER=$(cast balance "$TO" --rpc-url "$L1_RPC")

echo >&2
echo "withdrawal executed on L1" >&2
echo "  recipient $TO" >&2
echo "  balance   $BALANCE_BEFORE -> $BALANCE_AFTER" >&2
