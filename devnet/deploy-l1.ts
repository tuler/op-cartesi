#!/usr/bin/env bun
// Deploys the OP Stack L1 contract suite with op-deployer and records what the
// rest of the devnet needs from it.
//
// Only the L1 half of op-deployer's output is used. Its `inspect genesis` and
// `inspect rollup` describe an op-geth L2 — predeploys, an allocs file, a
// genesis hash derived from EVM state — none of which exists here: this
// chain's genesis comes from the Cartesi Machine's root hash. So the addresses
// are taken from `inspect l1`, the initial system config is read back off the
// deployed SystemConfig, and the L2 side is generated as it always was.
//
// Writes devnet/l1-addresses.env, which lib/env.ts reads back.

import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { parseAbi } from "viem";
import { addresses, chainParams, config, paths } from "./lib/env.ts";
import { MULTICALL3_ADDRESS, MULTICALL3_CODE } from "./lib/multicall3.ts";
import { die, forget, must, run, say } from "./lib/proc.ts";
import { l1Public } from "./lib/wallet.ts";

const systemConfigAbi = parseAbi(["function scalar() view returns (uint256)"]);

/** op-deployer, which is on PATH because the image the deploy step runs in
 * puts it there (devnet/compose/Dockerfile). The OP monorepo publishes no
 * binaries — its releases carry source archives only — so an image is how you
 * get one, and the compose bring-up means nobody has to arrange that per-host:
 * the version is pinned by the tag that image is built from. */
function deployer(): string[] {
    if (Bun.which("op-deployer") === null) {
        die(
            "op-deployer is not on PATH — run this step with `docker compose run --rm l1-contracts`",
        );
    }
    return ["op-deployer"];
}

/** The intent op-deployer generates is a template: every role is the zero
 * address and the L2 parameters are zero. These are the ones that have to
 * become real, because they end up in the SystemConfig that op-node reads
 * back — a deployment that disagrees with the chain op-cartesi serves is a
 * chain that starts on values L1 will later contradict. */
function fillIntent(intent: string): string {
    const a = addresses();
    const p = chainParams();
    const roles: Record<string, string> = {
        SuperchainProxyAdminOwner: a.deployerAddress,
        SuperchainGuardian: a.deployerAddress,
        Challenger: a.deployerAddress,
        l1ProxyAdminOwner: a.deployerAddress,
        l2ProxyAdminOwner: a.deployerAddress,
        systemConfigOwner: a.deployerAddress,
        challenger: a.deployerAddress,
        unsafeBlockSigner: a.sequencerAddress,
        batcher: p.batcherAddress,
        proposer: a.proposerAddress,
        baseFeeVaultRecipient: a.deployerAddress,
        l1FeeVaultRecipient: a.deployerAddress,
        sequencerFeeVaultRecipient: a.deployerAddress,
        operatorFeeVaultRecipient: a.deployerAddress,
    };
    let out = intent;
    // liquidityControllerOwner is deliberately left zero: setting it switches
    // the chain to a custom gas token and the intent then fails validation for
    // want of a token name.
    for (const [key, value] of Object.entries(roles)) {
        out = out.replace(new RegExp(`^(\\s*${key}\\s*=\\s*)"0x0{40}"`, "gm"), `$1"${value}"`);
    }
    out = out.replace(/(\beip1559DenominatorCanyon\s*=\s*)0\b/g, `$1${p.eip1559Denominator}`);
    out = out.replace(/(\beip1559Denominator\s*=\s*)0\b/g, `$1${p.eip1559Denominator}`);
    out = out.replace(/(\beip1559Elasticity\s*=\s*)0\b/g, `$1${p.eip1559Elasticity}`);
    out = out.replace(/^(\s*gasLimit\s*=\s*)\d+/gm, `$1${p.gasLimit}`);
    return out;
}

/** The Ecotone encoding of SystemConfig.scalar: a version byte, then
 * blobBaseFeeScalar and baseFeeScalar as the last two uint32s. */
function decodeScalar(scalar: bigint): { base: bigint; blob: bigint } {
    const version = scalar >> 248n;
    if (version !== 1n) die(`SystemConfig scalar is version ${version}, not Ecotone (1)`);
    return { blob: (scalar >> 32n) & 0xffffffffn, base: scalar & 0xffffffffn };
}

export async function deployL1(): Promise<void> {
    // Whatever a previous deployment recorded is stale from here on, and it is
    // read back through lib/env.ts by everything downstream — including, one
    // step later, the anchor. Forgetting it first is the deploy's own business
    // rather than its caller's, since either bring-up can start it.
    forget(paths.l1Addresses);
    const p = chainParams();
    const a = addresses();
    const l1 = l1Public();
    try {
        await l1.getChainId();
    } catch {
        die(`no L1 at ${config.l1Rpc} — start one first`);
    }

    // Multicall3 at its canonical address: anvil does not predeploy it, and
    // viem's op-stack actions (waitToProve → getGames) multicall the dispute
    // games through it. Placed by cheatcode because the canonical address
    // only comes from a presigned deployment this devnet has no reason to
    // replay; l1Chain (lib/wallet.ts) declares the address to viem.
    say(`placing Multicall3 at ${MULTICALL3_ADDRESS}`);
    await l1.request({
        method: "anvil_setCode" as never,
        params: [MULTICALL3_ADDRESS, MULTICALL3_CODE] as never,
    });

    const opDeployer = deployer();
    const argv = (...args: string[]) => [...opDeployer, ...args];
    rmSync(paths.deployWorkdir, { recursive: true, force: true });
    mkdirSync(paths.deployWorkdir, { recursive: true });

    // A devnet L1 is not a chain id op-deployer knows, and the standard intent
    // resolves OPCM from a per-chain table — hence `custom`, which deploys the
    // superchain contracts and the implementations along the way.
    // (`op-deployer bootstrap` exists for placing those separately; with a
    // custom intent it is not needed, because apply does it.)
    say(`generating an op-deployer intent for L1 ${p.l1ChainId} / L2 ${p.l2ChainId}`);
    await must(
        argv(
            "init",
            "--intent-type",
            "custom",
            "--l1-chain-id",
            String(p.l1ChainId),
            "--l2-chain-ids",
            String(p.l2ChainId),
            "--workdir",
            paths.deployWorkdir,
        ),
        { capture: true },
    );

    const intentPath = join(paths.deployWorkdir, "intent.toml");
    writeFileSync(intentPath, fillIntent(readFileSync(intentPath, "utf8")));

    say("deploying the L1 contract suite (this takes a minute)");
    const applied = await run(
        argv(
            "apply",
            "--workdir",
            paths.deployWorkdir,
            "--l1-rpc-url",
            config.l1Rpc,
            "--private-key",
            a.deployerKey,
        ),
    );
    if (applied.code !== 0) die(`op-deployer apply exited ${applied.code}`);

    const inspect = async (what: string) => {
        const { stdout } = await must(
            argv("inspect", what, "--workdir", paths.deployWorkdir, String(p.l2ChainId)),
            { capture: true },
        );
        return JSON.parse(stdout);
    };
    const l1Out = await inspect("l1");
    // The batch inbox is derived from the chain id rather than deployed, so it
    // comes from op-deployer's own rollup config; everything else is a
    // contract. The rollup starts after the block the contracts landed in, not
    // at L1 genesis: before this block there is no SystemConfig to read.
    const rollupOut = await inspect("rollup");

    const lines = [
        "# Written by devnet/deploy-l1.ts. Read back by devnet/lib/env.ts.",
        "# Addresses of the OP Stack contracts deployed on the devnet L1.",
        `DEPOSIT_CONTRACT_ADDRESS=${l1Out.OptimismPortalProxy}`,
        `L1_SYSTEM_CONFIG_ADDRESS=${l1Out.SystemConfigProxy}`,
        `BATCH_INBOX_ADDRESS=${rollupOut.batch_inbox_address}`,
        `DISPUTE_GAME_FACTORY_ADDRESS=${l1Out.DisputeGameFactoryProxy}`,
        `L1_STANDARD_BRIDGE_ADDRESS=${l1Out.L1StandardBridgeProxy}`,
        `L1_CROSS_DOMAIN_MESSENGER_ADDRESS=${l1Out.L1CrossDomainMessengerProxy}`,
        `ANCHOR_STATE_REGISTRY_ADDRESS=${l1Out.AnchorStateRegistryProxy}`,
        "# The rollup starts after the block the contracts landed in, not",
        "# at L1 genesis: before this block there is no SystemConfig to read.",
        `L1_GENESIS_HASH=${rollupOut.genesis.l1.hash}`,
        `L1_GENESIS_NUMBER=${rollupOut.genesis.l1.number}`,
    ];

    // The fee scalars are read back off the deployed SystemConfig rather than
    // assumed: op-node takes the genesis system config from rollup.json and
    // then applies SystemConfig updates from L1 on top, so a rollup.json that
    // disagrees with the contract starts the chain on values L1 will later
    // contradict.
    const scalar = await l1.readContract({
        address: l1Out.SystemConfigProxy,
        abi: systemConfigAbi,
        functionName: "scalar",
    });
    const { base, blob } = decodeScalar(scalar);
    lines.push(`BASE_FEE_SCALAR=${base}`, `BLOB_BASE_FEE_SCALAR=${blob}`);
    say(`SystemConfig scalars: base=${base} blob=${blob}`);

    writeFileSync(paths.l1Addresses, `${lines.join("\n")}\n`);
    console.log(`L1 contracts deployed. Addresses in ${paths.l1Addresses}:`);
    for (const line of lines) if (!line.startsWith("#")) console.log(`  ${line}`);
}

if (import.meta.main) await deployL1();
