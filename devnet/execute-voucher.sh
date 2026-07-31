#!/usr/bin/env bash
# Takes a voucher the guest emitted and runs it on L1.
#
#   ./devnet/execute-voucher.sh <L2 tx hash>
#
# This is the second half of every withdrawal, and it is the same half whatever
# is being withdrawn: the guest decides what the call is, and this only has to
# prove that the machine really emitted it. Wait for a proposal covering the
# voucher's block, open that proposal's root claim, take the outputs root out
# of it, and hand Cartesi's own verifier a proof against it.
#
# Prints the executed output's chain-wide index on stdout.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
: "${L2_RPC:=http://127.0.0.1:${HTTP_ADDR##*:}}"
: "${CALLER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"

TXHASH="${1:?usage: execute-voucher.sh <L2 tx hash>}"
if [ -z "${OUTPUT_EXECUTOR_ADDRESS:-}" ]; then
  echo "run ./devnet/deploy-outputs.sh first" >&2
  exit 1
fi

# 1. Find the voucher's chain-wide index and the block that produced it.
until cast rpc eth_getTransactionReceipt "$TXHASH" --rpc-url "$L2_RPC" | grep -q blockNumber; do sleep 2; done
INDEX=$(cast rpc cartesi_getTransactionEmissions "$TXHASH" --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;o=json.load(sys.stdin)["outputs"];print(int(o[0]["index"],16)) if o else exit("the transaction emitted no provable output")')
OUTBLOCK=$(cast rpc eth_getTransactionReceipt "$TXHASH" --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;print(int(json.load(sys.stdin)["blockNumber"],16))')
echo "  voucher is output $INDEX, in L2 block $OUTBLOCK" >&2

# 2. Wait for a proposal whose L2 block is at or past the voucher's.
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

# 3. Open that proposal's root claim on L1. The four words are the preimage
#    op-node hashed; the third is the Cartesi outputs root.
BLOCKHEX=$(printf '0x%x' "$PROPOSED_BLOCK")
read -r STATEROOT OUTPUTSROOT BLOCKHASH <<< "$(cast rpc eth_getBlockByNumber "$BLOCKHEX" false --rpc-url "$L2_RPC" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["stateRoot"],d["withdrawalsRoot"],d["hash"])')"
cast send "$OUTPUTS_VALIDATOR_ADDRESS" \
  'accept(uint256,(bytes32,bytes32,bytes32,bytes32))' "$FOUND" \
  "($(cast to-uint256 0),$STATEROOT,$OUTPUTSROOT,$BLOCKHASH)" \
  --private-key "$CALLER_KEY" --rpc-url "$L1_RPC" > /dev/null
echo "  accepted outputs root $OUTPUTSROOT from game $FOUND" >&2

# 4. Prove the voucher against that block and execute it. The proof is against
#    the proposed block's tree, not the emitting block's — a withdrawal has to
#    stay provable as the tree grows past it.
PROOF=$(cast rpc cartesi_getOutputProof "$(printf '0x%x' "$INDEX")" "$BLOCKHEX" --rpc-url "$L2_RPC")
OUTPUT=$(echo "$PROOF" | python3 -c 'import sys,json;print(json.load(sys.stdin)["output"])')
SIBLINGS=$(echo "$PROOF" | python3 -c 'import sys,json;print("["+",".join(json.load(sys.stdin)["outputHashesSiblings"])+"]")')
cast send "$OUTPUT_EXECUTOR_ADDRESS" 'executeOutput(bytes,(uint64,bytes32[]))' \
  "$OUTPUT" "($INDEX,$SIBLINGS)" --private-key "$CALLER_KEY" --rpc-url "$L1_RPC" > /dev/null
echo "  executed output $INDEX" >&2

echo "$INDEX"
