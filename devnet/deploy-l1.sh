#!/usr/bin/env bash
# Deploys the OP Stack L1 contract suite with op-deployer and records what the
# rest of the devnet needs from it.
#
# Only the L1 half of op-deployer's output is used. Its `inspect genesis` and
# `inspect rollup` describe an op-geth L2 — predeploys, an allocs file, a
# genesis hash derived from EVM state — none of which exists here: this chain's
# genesis comes from the Cartesi Machine's root hash. So the addresses are
# taken from `inspect l1`, the initial system config is read back off the
# deployed SystemConfig, and the L2 side is generated as it always was.
#
# Writes devnet/l1-addresses.env, which env.sh sources automatically.

source "$(dirname "${BASH_SOURCE[0]}")/env.sh"

: "${L1_PORT:=8600}"
: "${L1_RPC:=http://127.0.0.1:$L1_PORT}"
# The deployer is kept off the batcher's and depositor's accounts so nothing
# competes for a nonce. Anvil account 3.
: "${DEPLOYER_KEY:=0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6}"
: "${DEPLOYER_ADDRESS:=0x90F79bf6EB2c4f870365E785982E1f101E93b906}"
: "${PROPOSER_ADDRESS:=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC}"
: "${SEQUENCER_ADDRESS:=0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65}"
: "${DEPLOY_WORKDIR:=$DEVNET_DIR/deployment}"
: "${L1_ADDRESSES_FILE:=$DEVNET_DIR/l1-addresses.env}"

if ! cast chain-id --rpc-url "$L1_RPC" > /dev/null 2>&1; then
  echo "no L1 at $L1_RPC — start one first" >&2
  exit 1
fi

# op-deployer ships as a docker image like the rest of the OP tools.
deployer() {
  if command -v op-deployer > /dev/null 2>&1; then
    op-deployer "$@"
  else
    docker run --rm --add-host="host.docker.internal:host-gateway" \
      -v "$DEPLOY_WORKDIR:/w" -w /w "$OP_DEPLOYER_IMAGE" op-deployer "$@"
  fi
}
# Inside a container the L1 is reached over the host gateway, so anvil has to
# be listening on more than loopback for this to work.
deployer_l1_rpc() {
  if command -v op-deployer > /dev/null 2>&1; then
    echo "$L1_RPC"
  else
    echo "http://host.docker.internal:$L1_PORT"
  fi
}

rm -rf "$DEPLOY_WORKDIR"
mkdir -p "$DEPLOY_WORKDIR"

# A devnet L1 is not a chain id op-deployer knows, and the standard intent
# resolves OPCM from a per-chain table — hence `custom`, which deploys the
# superchain contracts and the implementations along the way. (`op-deployer
# bootstrap` exists for placing those separately; with a custom intent it is
# not needed, because apply does it.)
echo "generating an op-deployer intent for L1 $L1_CHAIN_ID / L2 $L2_CHAIN_ID" >&2
deployer init --intent-type custom --l1-chain-id "$L1_CHAIN_ID" \
  --l2-chain-ids "$L2_CHAIN_ID" --workdir /w > /dev/null

python3 - "$DEPLOY_WORKDIR/intent.toml" <<PY
import re, sys
path = sys.argv[1]
s = open(path).read()
roles = {
    "SuperchainProxyAdminOwner": "$DEPLOYER_ADDRESS",
    "SuperchainGuardian":        "$DEPLOYER_ADDRESS",
    "Challenger":                "$DEPLOYER_ADDRESS",
    "l1ProxyAdminOwner":         "$DEPLOYER_ADDRESS",
    "l2ProxyAdminOwner":         "$DEPLOYER_ADDRESS",
    "systemConfigOwner":         "$DEPLOYER_ADDRESS",
    "challenger":                "$DEPLOYER_ADDRESS",
    "unsafeBlockSigner":         "$SEQUENCER_ADDRESS",
    "batcher":                   "$BATCHER_ADDRESS",
    "proposer":                  "$PROPOSER_ADDRESS",
    "baseFeeVaultRecipient":      "$DEPLOYER_ADDRESS",
    "l1FeeVaultRecipient":        "$DEPLOYER_ADDRESS",
    "sequencerFeeVaultRecipient": "$DEPLOYER_ADDRESS",
    "operatorFeeVaultRecipient":  "$DEPLOYER_ADDRESS",
}
# liquidityControllerOwner is deliberately left zero: setting it switches the
# chain to a custom gas token and the intent then fails validation for want of
# a token name.
for key, value in roles.items():
    s = re.sub(r'(?m)^(\s*%s\s*=\s*)"0x0{40}"' % key, lambda m: m.group(1) + '"%s"' % value, s)
# The L2 parameters that end up in the SystemConfig have to agree with the
# chain op-cartesi actually serves, since op-node reads them back off L1.
s = s.replace("eip1559DenominatorCanyon = 0", "eip1559DenominatorCanyon = $EIP1559_DENOMINATOR")
s = s.replace("eip1559Denominator = 0", "eip1559Denominator = $EIP1559_DENOMINATOR")
s = s.replace("eip1559Elasticity = 0", "eip1559Elasticity = $EIP1559_ELASTICITY")
s = re.sub(r"(?m)^(\s*gasLimit\s*=\s*)\d+", lambda m: m.group(1) + "$GAS_LIMIT", s)
open(path, "w").write(s)
PY

echo "deploying the L1 contract suite (this takes a minute)" >&2
deployer apply --workdir /w --l1-rpc-url "$(deployer_l1_rpc)" \
  --private-key "$DEPLOYER_KEY" > "$DEPLOY_WORKDIR/apply.log" 2>&1 || {
  echo "op-deployer apply failed; last lines of $DEPLOY_WORKDIR/apply.log:" >&2
  tail -20 "$DEPLOY_WORKDIR/apply.log" >&2
  exit 1
}

deployer inspect l1 --workdir /w "$L2_CHAIN_ID" > "$DEPLOY_WORKDIR/l1.json"
deployer inspect rollup --workdir /w "$L2_CHAIN_ID" > "$DEPLOY_WORKDIR/rollup-opdeployer.json"

# The batch inbox is derived from the chain id rather than deployed, so it is
# read from op-deployer's own rollup config; everything else is a contract.
python3 - "$DEPLOY_WORKDIR" "$L1_ADDRESSES_FILE" <<'PY'
import json, sys
workdir, out = sys.argv[1], sys.argv[2]
l1 = json.load(open(f"{workdir}/l1.json"))
rc = json.load(open(f"{workdir}/rollup-opdeployer.json"))
with open(out, "w") as f:
    f.write("# Written by devnet/deploy-l1.sh. Sourced by env.sh.\n")
    f.write("# Addresses of the OP Stack contracts deployed on the devnet L1.\n")
    f.write(f'DEPOSIT_CONTRACT_ADDRESS={l1["OptimismPortalProxy"]}\n')
    f.write(f'L1_SYSTEM_CONFIG_ADDRESS={l1["SystemConfigProxy"]}\n')
    f.write(f'BATCH_INBOX_ADDRESS={rc["batch_inbox_address"]}\n')
    f.write(f'DISPUTE_GAME_FACTORY_ADDRESS={l1["DisputeGameFactoryProxy"]}\n')
    f.write(f'L1_STANDARD_BRIDGE_ADDRESS={l1["L1StandardBridgeProxy"]}\n')
    f.write(f'ANCHOR_STATE_REGISTRY_ADDRESS={l1["AnchorStateRegistryProxy"]}\n')
    f.write("# The rollup starts after the block the contracts landed in, not\n")
    f.write("# at L1 genesis: before this block there is no SystemConfig to read.\n")
    f.write(f'L1_GENESIS_HASH={rc["genesis"]["l1"]["hash"]}\n')
    f.write(f'L1_GENESIS_NUMBER={rc["genesis"]["l1"]["number"]}\n')
print(f"wrote {out}", file=sys.stderr)
PY

# The fee scalars are read back off the deployed SystemConfig rather than
# assumed: op-node takes the genesis system config from rollup.json and then
# applies SystemConfig updates from L1 on top, so a rollup.json that disagrees
# with the contract starts the chain on values L1 will later contradict.
SYSCFG=$(grep '^L1_SYSTEM_CONFIG_ADDRESS=' "$L1_ADDRESSES_FILE" | cut -d= -f2)
SCALAR=$(cast call "$SYSCFG" "scalar()(uint256)" --rpc-url "$L1_RPC" | awk '{print $1}')
python3 - "$SCALAR" "$L1_ADDRESSES_FILE" <<'PY'
import sys
scalar = int(sys.argv[1], 0)
raw = scalar.to_bytes(32, "big")
# Ecotone encoding: version byte, then blobBaseFeeScalar and baseFeeScalar as
# the last two uint32s.
version = raw[0]
blob = int.from_bytes(raw[24:28], "big")
base = int.from_bytes(raw[28:32], "big")
if version != 1:
    print(f"SystemConfig scalar is version {version}, not Ecotone (1)", file=sys.stderr)
    raise SystemExit(1)
with open(sys.argv[2], "a") as f:
    f.write(f"BASE_FEE_SCALAR={base}\n")
    f.write(f"BLOB_BASE_FEE_SCALAR={blob}\n")
print(f"SystemConfig scalars: base={base} blob={blob}", file=sys.stderr)
PY

echo >&2
echo "L1 contracts deployed. Addresses in $L1_ADDRESSES_FILE:" >&2
sed 's/^/  /' "$L1_ADDRESSES_FILE" | grep -v '^  #' >&2
