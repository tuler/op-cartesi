#!/usr/bin/env bash
# Deploys the contracts that make Cartesi outputs executable on L1 against OP
# proposals, plus Cartesi-style portals that route deposits through
# OptimismPortal instead of an InputBox.
#
# See contracts/ for the Foundry project; this only supplies the addresses the
# devnet already knows and records what came back.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
: "${DEPLOYER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
# How long a proposal must stand before its outputs may be executed, and
# whether its game must have resolved. Zero and false are the permissioned
# posture this chain already runs under — nothing can dispute a proposal,
# because no fault proof VM can execute a Cartesi Machine. A chain with a real
# proof system sets both.
: "${OUTPUT_MATURITY_DELAY:=0}"
: "${OUTPUT_REQUIRE_RESOLVED:=false}"
: "${OUTPUTS_ENV_FILE:=$DEVNET_DIR/outputs-addresses.env}"
: "${EXECUTOR_FUNDING:=10000000000000000000}"
# OptimismPortal's floor is 21000 + 16 per byte, and the registration
# calldata (registerPortal's selector plus two ABI words) is 68 bytes. The
# chain meters in machine cycles and ignores this, but L1 still checks it.
: "${REGISTER_GAS_LIMIT:=100000}"

if [ -z "${DISPUTE_GAME_FACTORY_ADDRESS:-}" ]; then
  echo "no DisputeGameFactory; run the devnet with WITH_CONTRACTS=1 first" >&2
  exit 1
fi
if ! command -v forge > /dev/null 2>&1; then
  echo "forge is not on PATH; install foundry (https://getfoundry.sh)" >&2
  exit 1
fi

cd "$REPO_DIR/contracts"
[ -d dependencies ] || forge soldeer install

OUT=$(DISPUTE_GAME_FACTORY_ADDRESS="$DISPUTE_GAME_FACTORY_ADDRESS" \
  DEPOSIT_CONTRACT_ADDRESS="$DEPOSIT_CONTRACT_ADDRESS" \
  DISPUTE_GAME_TYPE="$DISPUTE_GAME_TYPE" \
  OUTPUT_MATURITY_DELAY="$OUTPUT_MATURITY_DELAY" \
  OUTPUT_REQUIRE_RESOLVED="$OUTPUT_REQUIRE_RESOLVED" \
  forge script script/DeployOutputs.s.sol:DeployOutputs \
    --rpc-url "$L1_RPC" --private-key "$DEPLOYER_KEY" --broadcast 2>&1)

if ! echo "$OUT" | grep -q "OUTPUT_EXECUTOR_ADDRESS="; then
  echo "$OUT" | tail -20 >&2
  exit 1
fi

{
  echo "# Written by devnet/deploy-outputs.sh. Sourced by env.sh."
  echo "$OUT" | grep -oE '(OUTPUTS_VALIDATOR|OUTPUT_EXECUTOR|ERC20_PORTAL|ETHER_PORTAL)_ADDRESS=0x[0-9a-fA-F]{40}'
} > "$OUTPUTS_ENV_FILE"
source "$OUTPUTS_ENV_FILE"

# The executor pays vouchers from its own balance, the way a Cartesi
# Application holds the assets it can be told to move.
#
# Ether deposited through OptimismPortal does not land here — it stays in OP's
# lockbox, where no voucher can reach it — so an ether withdrawal is only
# payable because of this funding. OPEtherPortal is the path where the two
# directions agree; see DESIGN §7d.
cast send "$OUTPUT_EXECUTOR_ADDRESS" --value "$EXECUTOR_FUNDING" \
  --private-key "$DEPLOYER_KEY" --rpc-url "$L1_RPC" > /dev/null

# Tell the guest which contracts it may credit deposits from: an owner call
# to the routed guest's config contract (EVM-COMPAT §6),
# registerPortal(uint8 kind, address portal), carried as an L1 deposit
# addressed to GUEST_CONFIG_ADDRESS.
#
# The guest cannot have the portals baked into its genesis state: they do not
# exist until L1 is deployed, and L1 cannot be deployed after a genesis that
# names them. So they arrive as an input like any other — one the config
# contract takes only from the owner address baked into the snapshot. An EOA
# calling the portal directly is not aliased, so the deposit reaches the
# guest with `from` set to the owner itself, which is what it checks. Once
# registered, the guest routes deposits from these senders to the portal
# receiver, whatever address they are sent to.
if [ "$(cast wallet address --private-key "$DEPLOYER_KEY")" != "$GUEST_OWNER" ]; then
  echo "the deploy key is not the guest's owner ($GUEST_OWNER); the guest will ignore this registration" >&2
  exit 1
fi
register_portal() {
  cast send "$DEPOSIT_CONTRACT_ADDRESS" \
    "depositTransaction(address,uint256,uint64,bool,bytes)" \
    "$GUEST_CONFIG_ADDRESS" 0 "$REGISTER_GAS_LIMIT" false \
    "$(cast calldata 'registerPortal(uint8,address)' "$1" "$2")" \
    --private-key "$DEPLOYER_KEY" --rpc-url "$L1_RPC" > /dev/null
}
register_portal 0 "$ETHER_PORTAL_ADDRESS"
register_portal 1 "$ERC20_PORTAL_ADDRESS"

echo "wrote $OUTPUTS_ENV_FILE" >&2
grep -v '^#' "$OUTPUTS_ENV_FILE" | sed 's/^/  /' >&2
echo "  registered both portals with the guest" >&2
