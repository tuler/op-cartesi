#!/usr/bin/env bash
# Deploys the two contracts that let a Cartesi output be executed on L1 against
# an OP proposal, and records their addresses for withdraw.sh.
#
#   OPOutputsMerkleRootValidator — opens a proposal's root claim and records the
#     Cartesi outputs root it commits to.
#   OutputExecutor              — proves an output against that root with
#     Cartesi's own libraries and runs it.
#
# Requires the devnet running with contracts deployed (WITH_CONTRACTS=1).

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
: "${DEPLOYER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
: "${DEPLOYER_ADDRESS:=0x90F79bf6EB2c4f870365E785982E1f101E93b906}"
# How long a proposal must stand before its outputs may be executed. Zero on a
# devnet; a real chain sets this to cover the dispute window.
: "${OUTPUT_MATURITY_DELAY:=0}"
# Whether the game must have resolved for the defender. False is the
# permissioned posture this chain already runs under: proposals come from an
# authorised proposer and nothing can dispute them, because no fault proof VM
# can execute a Cartesi Machine yet.
: "${OUTPUT_REQUIRE_RESOLVED:=false}"
: "${OUTPUTS_ENV_FILE:=$DEVNET_DIR/outputs-addresses.env}"
: "${EXECUTOR_FUNDING:=10000000000000000000}"

if [ -z "${DISPUTE_GAME_FACTORY_ADDRESS:-}" ]; then
  echo "no DisputeGameFactory; run the devnet with WITH_CONTRACTS=1 first" >&2
  exit 1
fi

"$REPO_DIR/contracts/build.sh"
BIN=$(python3 -c "
import json,sys
d=json.load(open('$REPO_DIR/contracts/out/contracts.json'))['contracts']
print(d['src/OPOutputsMerkleRootValidator.sol:OPOutputsMerkleRootValidator']['bin'])
print(d['src/OutputExecutor.sol:OutputExecutor']['bin'])")
VALIDATOR_BIN=$(echo "$BIN" | sed -n 1p)
EXECUTOR_BIN=$(echo "$BIN" | sed -n 2p)

# The two contracts point at each other: the validator answers for one
# application, and that application is the executor. Rather than add a setter,
# the executor's address is computed from the deployer's next-but-one nonce and
# baked into the validator's constructor.
NONCE=$(cast nonce "$DEPLOYER_ADDRESS" --rpc-url "$L1_RPC")
EXECUTOR_ADDRESS=$(cast compute-address "$DEPLOYER_ADDRESS" --nonce $((NONCE + 1)) --rpc-url "$L1_RPC" | awk '{print $NF}')

echo "deploying OPOutputsMerkleRootValidator (app will be $EXECUTOR_ADDRESS)" >&2
VALIDATOR_ARGS=$(cast abi-encode 'f(address,uint32,address,uint256,bool)' \
  "$DISPUTE_GAME_FACTORY_ADDRESS" "$DISPUTE_GAME_TYPE" "$EXECUTOR_ADDRESS" \
  "$OUTPUT_MATURITY_DELAY" "$OUTPUT_REQUIRE_RESOLVED")
VALIDATOR_ADDRESS=$(cast send --private-key "$DEPLOYER_KEY" --rpc-url "$L1_RPC" --json \
  --create "0x${VALIDATOR_BIN}${VALIDATOR_ARGS#0x}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["contractAddress"])')

echo "deploying OutputExecutor" >&2
EXECUTOR_ARGS=$(cast abi-encode 'f(address)' "$VALIDATOR_ADDRESS")
DEPLOYED=$(cast send --private-key "$DEPLOYER_KEY" --rpc-url "$L1_RPC" --json \
  --create "0x${EXECUTOR_BIN}${EXECUTOR_ARGS#0x}" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["contractAddress"])')
if [ "$(echo "$DEPLOYED" | tr 'A-Z' 'a-z')" != "$(echo "$EXECUTOR_ADDRESS" | tr 'A-Z' 'a-z')" ]; then
  echo "executor landed at $DEPLOYED but the validator was told $EXECUTOR_ADDRESS" >&2
  exit 1
fi

# The executor pays vouchers out of its own balance, the way a Cartesi
# Application holds the assets it can be instructed to move. Bridging that to
# the ETH OptimismPortal holds is a separate question — see DESIGN §4.
cast send "$DEPLOYED" --value "$EXECUTOR_FUNDING" --private-key "$DEPLOYER_KEY" --rpc-url "$L1_RPC" > /dev/null

cat > "$OUTPUTS_ENV_FILE" <<EOF
# Written by devnet/deploy-outputs.sh.
OUTPUTS_VALIDATOR_ADDRESS=$VALIDATOR_ADDRESS
OUTPUT_EXECUTOR_ADDRESS=$DEPLOYED
EOF
echo "wrote $OUTPUTS_ENV_FILE" >&2
sed 's/^/  /' "$OUTPUTS_ENV_FILE" | grep -v '^  #' >&2
