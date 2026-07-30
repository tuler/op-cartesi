#!/usr/bin/env bash
# Runs a withdrawal end to end: ask the guest for one, wait for a proposal that
# covers it, prove the voucher against that proposal on L1, and execute it.
#
#   ./devnet/withdraw.sh <recipient> <wei>
#
# This is the whole point of the outputs tree. The guest emits a voucher, the
# tree root goes into the block's withdrawalsRoot, op-proposer's claim commits
# to that root, and Cartesi's own proof libraries verify the voucher against it
# on L1 — with no change to OptimismPortal, op-node, op-batcher or op-proposer.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"
[ -f "$DEVNET_DIR/outputs-addresses.env" ] && source "$DEVNET_DIR/outputs-addresses.env"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${OPNODE_RPC:=http://127.0.0.1:9545}"
: "${CALLER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"

TO="${1:?usage: withdraw.sh <recipient> <wei>}"
AMOUNT="${2:?usage: withdraw.sh <recipient> <wei>}"
if [ -z "${OUTPUT_EXECUTOR_ADDRESS:-}" ]; then
  echo "run ./devnet/deploy-outputs.sh first" >&2
  exit 1
fi

# 1. Ask the guest to withdraw: "w" ‖ recipient ‖ amount, as a plain L2
#    transaction payload.
REQUEST="0x77${TO#0x}$(cast to-uint256 "$AMOUNT" | sed 's/^0x//')"
BEFORE=$(cast rpc cartesi_getOutputsRoot latest --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["count"],16))')
echo "asking the guest to withdraw $AMOUNT wei to $TO" >&2
TXHASH=$("$DEVNET_DIR/send-l2-tx.sh" "$REQUEST")
echo "  L2 tx $TXHASH" >&2

# 2. Find the voucher's chain-wide index.
until cast rpc eth_getTransactionReceipt "$TXHASH" --rpc-url "$L2_RPC" | grep -q blockNumber; do sleep 2; done
INDEX=$(cast rpc cartesi_getTransactionEmissions "$TXHASH" --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;o=json.load(sys.stdin)["outputs"];print(int(o[0]["index"],16)) if o else exit("the transaction emitted no provable output")')
echo "  voucher is output $INDEX (there were $BEFORE before it)" >&2

# 3. Wait for a proposal whose L2 block is at or past the voucher's block.
OUTBLOCK=$(cast rpc eth_getTransactionReceipt "$TXHASH" --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["blockNumber"],16))')
echo "  waiting for a proposal covering L2 block $OUTBLOCK" >&2
while :; do
  GAMES=$(cast call "$DISPUTE_GAME_FACTORY_ADDRESS" 'gameCount()(uint256)' --rpc-url "$L1_RPC" | awk '{print $1}')
  FOUND=""
  for (( i=GAMES-1; i>=0; i-- )); do
    GAME=$(cast call "$DISPUTE_GAME_FACTORY_ADDRESS" 'gameAtIndex(uint256)(uint32,uint64,address)' "$i" --rpc-url "$L1_RPC" | tail -1)
    BLOCK=$(cast call "$GAME" 'l2BlockNumber()(uint256)' --rpc-url "$L1_RPC" | awk '{print $1}')
    if [ "$BLOCK" -ge "$OUTBLOCK" ]; then FOUND="$i"; PROPOSED_BLOCK="$BLOCK"; break; fi
  done
  [ -n "$FOUND" ] && break
  sleep 4
done
echo "  game $FOUND proposes L2 block $PROPOSED_BLOCK" >&2

# 4. Open that proposal's root claim on L1. The four words are the preimage
#    op-node hashed; the third is the Cartesi outputs root.
BLOCKHEX=$(printf '0x%x' "$PROPOSED_BLOCK")
read -r STATEROOT OUTPUTSROOT BLOCKHASH <<< "$(cast rpc eth_getBlockByNumber "$BLOCKHEX" false --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["stateRoot"],d["withdrawalsRoot"],d["hash"])')"
cast send "$OUTPUTS_VALIDATOR_ADDRESS" \
  'accept(uint256,(bytes32,bytes32,bytes32,bytes32))' "$FOUND" \
  "($(cast to-uint256 0),$STATEROOT,$OUTPUTSROOT,$BLOCKHASH)" \
  --private-key "$CALLER_KEY" --rpc-url "$L1_RPC" > /dev/null
echo "  accepted outputs root $OUTPUTSROOT from game $FOUND" >&2

# 5. Prove the voucher against that block and execute it.
PROOF=$(cast rpc cartesi_getOutputProof "$(printf '0x%x' "$INDEX")" "$BLOCKHEX" --rpc-url "$L2_RPC")
OUTPUT=$(echo "$PROOF" | python3 -c 'import sys,json;print(json.load(sys.stdin)["output"])')
SIBLINGS=$(echo "$PROOF" | python3 -c 'import sys,json;print("["+",".join(json.load(sys.stdin)["outputHashesSiblings"])+"]")')

BALANCE_BEFORE=$(cast balance "$TO" --rpc-url "$L1_RPC")
cast send "$OUTPUT_EXECUTOR_ADDRESS" 'executeOutput(bytes,(uint64,bytes32[]))' \
  "$OUTPUT" "($INDEX,$SIBLINGS)" --private-key "$CALLER_KEY" --rpc-url "$L1_RPC" > /dev/null
BALANCE_AFTER=$(cast balance "$TO" --rpc-url "$L1_RPC")

echo >&2
echo "withdrawal executed on L1" >&2
echo "  recipient $TO" >&2
echo "  balance   $BALANCE_BEFORE -> $BALANCE_AFTER" >&2
