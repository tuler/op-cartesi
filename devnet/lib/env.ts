// The devnet's configuration, in one place.
//
// This used to be two files that had to agree variable for variable: env.sh
// for the shell orchestration and this one for the client scripts. Everything
// is TypeScript on bun now, so there is one reader, and "same names, same
// defaults" is a property of the code rather than a promise in a comment.
//
// Three layers, highest first:
//
//   1. the address files the deploy steps write (l1-addresses.env,
//      chain-genesis.env, outputs-addresses.env, token.env) — machine-written
//      deployment outputs, above everything, because a value the deployment
//      produced is not a preference;
//   2. the environment, which is where a `.env` at the repo root lands;
//   3. the defaults below.
//
// The address files are read on every call to `chainParams()` and
// `addresses()`, not once at import: a process that starts before the deploy
// it waits for would otherwise hold the values from before it ran.
//
// What the values are used to build — the chain definitions and the viem
// clients — is lib/wallet.ts, so the half of the devnet that only wants a
// port or a path does not import viem's op-stack surface to get one.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { Address, Hex } from "viem";

const DEVNET_DIR = dirname(dirname(fileURLToPath(import.meta.url)));
const REPO_DIR = dirname(DEVNET_DIR);

/** Everything the devnet writes and reads back, by absolute path. */
export const paths = {
    devnet: DEVNET_DIR,
    repo: REPO_DIR,
    /** The engine, compiled once by start-devnet.ts. */
    opCartesiBin: join(REPO_DIR, "bin", "op-cartesi"),
    jwtSecret: process.env.JWT_SECRET_FILE ?? join(DEVNET_DIR, "jwt.hex"),
    rollupConfig: process.env.ROLLUP_CONFIG_FILE ?? join(DEVNET_DIR, "rollup.json"),
    l1ChainConfig: process.env.L1_CHAIN_CONFIG ?? join(DEVNET_DIR, "l1-chain-config.json"),
    l1Addresses: join(DEVNET_DIR, "l1-addresses.env"),
    chainGenesis: join(DEVNET_DIR, "chain-genesis.env"),
    outputsAddresses: process.env.OUTPUTS_ENV_FILE ?? join(DEVNET_DIR, "outputs-addresses.env"),
    tokenEnv: process.env.TOKEN_ENV_FILE ?? join(DEVNET_DIR, "token.env"),
    deployWorkdir: process.env.DEPLOY_WORKDIR ?? join(DEVNET_DIR, "deployment"),
    logs: process.env.LOG_DIR ?? join(DEVNET_DIR, "logs"),
    /** Readiness markers, pid records and the generated mprocs config. */
    state: process.env.STATE_DIR ?? join(DEVNET_DIR, ".state"),
} as const;

function parseEnvFile(path: string): Record<string, string> {
    if (!existsSync(path)) return {};
    const vars: Record<string, string> = {};
    for (const line of readFileSync(path, "utf8").split("\n")) {
        const m = line.match(/^([A-Za-z_][A-Za-z0-9_]*)=("?)(.*)\2$/);
        if (m) vars[m[1]!] = m[3]!;
    }
    return vars;
}

// bun auto-loads a .env, but only from the directory it was invoked in, and
// these scripts are invoked from panes with their own cwd. Loading the repo
// root's explicitly makes overrides work wherever a script is run from —
// below real environment variables, which is the precedence bun itself uses.
for (const [key, value] of Object.entries(parseEnvFile(join(REPO_DIR, ".env")))) {
    if (process.env[key] === undefined) process.env[key] = value;
}

/** The deployment outputs, re-read on every call. Later files win, which is
 * the order the deploys happen in. */
function deploymentVars(): Record<string, string> {
    return {
        ...parseEnvFile(paths.l1Addresses),
        ...parseEnvFile(paths.chainGenesis),
        ...parseEnvFile(paths.outputsAddresses),
        ...parseEnvFile(paths.tokenEnv),
    };
}

function pick(files: Record<string, string>, name: string): string | undefined {
    return files[name] ?? process.env[name] ?? undefined;
}

const fileVars = deploymentVars();

function env(name: string, fallback: string): string {
    return pick(fileVars, name) ?? fallback;
}

function optionalEnv(name: string): string | undefined {
    return pick(fileVars, name);
}

function num(name: string, fallback: number): number {
    const v = process.env[name];
    return v === undefined || v === "" ? fallback : Number(v);
}

/** `WITH_*` flags are 1/0 in the environment, as they were for the shell. */
function flag(name: string, fallback: boolean): boolean {
    const v = process.env[name];
    if (v === undefined || v === "") return fallback;
    return v !== "0" && v.toLowerCase() !== "false";
}

/** The guards earn viem's template-literal types by inspection, the same
 * doctrine as the router's tx.ts: no casts, checked at runtime. */
function isAddress(v: string): v is Address {
    return /^0x[0-9a-f]{40}$/.test(v);
}

function isHash(v: string): v is Hex {
    return /^0x[0-9a-f]{64}$/.test(v);
}

function addressEnv(name: string, fallback: string): Address {
    const v = env(name, fallback).toLowerCase();
    if (!isAddress(v)) throw new Error(`${name} is not a 20-byte hex address: ${v}`);
    return v;
}

function optionalAddress(name: string): Address | undefined {
    const v = optionalEnv(name)?.toLowerCase();
    return v !== undefined && isAddress(v) ? v : undefined;
}

// --- defaults ---------------------------------------------------------------
// Kept as literals rather than inlined, because several of them are the same
// anvil account seen from two sides (the deployer is the guest's owner; the
// batcher is the address baked into the rollup config's system config).
const ANVIL_0_KEY = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80";
const ANVIL_1_KEY = "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d";
const ANVIL_2_KEY = "0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a";
const ANVIL_3_KEY = "0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6";
const ANVIL_0 = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266";
const ANVIL_2 = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC";
const ANVIL_3 = "0x90F79bf6EB2c4f870365E785982E1f101E93b906";
const SEQUENCER_ADDRESS_DEFAULT = "0x15d34AAf54267DB7D7c367839AAf71A00a2C6A65";

const httpAddrDefault = "127.0.0.1:8545";
const l1Port = num("L1_PORT", 8600);

export const config = {
    l1ChainId: Number(env("L1_CHAIN_ID", "900")),
    l2ChainId: Number(env("L2_CHAIN_ID", "901")),
    l1Rpc: env("L1_RPC", `http://127.0.0.1:${l1Port}`),
    l2Rpc: env("L2_RPC", `http://${env("HTTP_ADDR", httpAddrDefault)}`),
    // Anvil account 1, kept off account 0 so deposits never race the
    // batcher's nonce.
    depositorKey: env("DEPOSITOR_KEY", ANVIL_1_KEY),
    // Anvil account 3 — the guest owner's key, and the default L2 sender.
    senderKey: env("SENDER_KEY", ANVIL_3_KEY),
    callerKey: env("CALLER_KEY", ANVIL_3_KEY),
    depositGasLimit: BigInt(env("DEPOSIT_GAS_LIMIT", "1000000")),
    depositContract: addressEnv(
        "DEPOSIT_CONTRACT_ADDRESS",
        "0x6900000000000000000000000000000000000001",
    ),
    disputeGameFactory: optionalAddress("DISPUTE_GAME_FACTORY_ADDRESS"),
    outputsValidator: optionalAddress("OUTPUTS_VALIDATOR_ADDRESS"),
    outputExecutor: optionalAddress("OUTPUT_EXECUTOR_ADDRESS"),
    erc20Portal: optionalAddress("ERC20_PORTAL_ADDRESS"),
    etherPortal: optionalAddress("ETHER_PORTAL_ADDRESS"),
    l1StandardBridge: optionalAddress("L1_STANDARD_BRIDGE_ADDRESS"),
    l1CrossDomainMessenger: optionalAddress("L1_CROSS_DOMAIN_MESSENGER_ADDRESS"),
    testToken: optionalAddress("TEST_TOKEN_ADDRESS"),
    tokenEnvFile: paths.tokenEnv,
};

// --- the stack --------------------------------------------------------------
// Ports, and which of the optional pieces this run wants. Nothing here comes
// from a deployment, so unlike the addresses it is a snapshot: a process that
// waits does not need to re-read its own port.
export const stack = {
    l1Port,
    /** anvil's --block-time. The L2's own is `blockTime` below. */
    l1BlockTime: num("L1_BLOCK_TIME", 2),
    machinePort: num("MACHINE_PORT", 6300),
    // Generating the rollup config loads the snapshot too, and a machine
    // server holds exactly one machine, so the generator gets its own server
    // rather than recycling the node's.
    genesisMachinePort: num("GENESIS_MACHINE_PORT", 6301),
    opNodeRpcPort: num("OPNODE_RPC_PORT", 9545),
    batcherRpcPort: num("BATCHER_RPC_PORT", 8548),
    proposerRpcPort: num("PROPOSER_RPC_PORT", 8560),
    engineAddr: env("ENGINE_ADDR", "127.0.0.1:8551"),
    httpAddr: env("HTTP_ADDR", httpAddrDefault),
    // The second node: its own engine, machine server and op-node, deriving
    // purely from L1 rather than sequencing.
    verifierEngineAddr: env("VERIFIER_ENGINE_ADDR", "127.0.0.1:8571"),
    verifierHttpAddr: env("VERIFIER_HTTP_ADDR", "127.0.0.1:8565"),
    verifierMachinePort: num("VERIFIER_MACHINE_PORT", 6400),
    verifierOpNodeRpcPort: num("VERIFIER_OPNODE_RPC_PORT", 9555),

    withContracts: flag("WITH_CONTRACTS", true),
    withBatcher: flag("WITH_BATCHER", true),
    withProposer: flag("WITH_PROPOSER", true),
    withVerifier: flag("WITH_VERIFIER", true),
    withGuestLog: flag("WITH_GUEST_LOG", true),

    /** The routed guest's snapshot, as `cartesi build` stores it. */
    snapshotDir: process.env.SNAPSHOT_DIR ?? join(REPO_DIR, "demo", ".cartesi", "image"),
    /** Directory for the chain store. Empty keeps the chain in memory. */
    dataDir: process.env.DATA_DIR ?? "",
    /** How long a process waits for what it depends on. Deploying the L1
     * suite takes about a minute and booting Linux inside the machine can
     * take two, so this is generous on purpose. */
    waitTimeoutMs: num("WAIT_TIMEOUT", 600) * 1000,

    // The OP monorepo ships no binaries: its releases carry source archives
    // only, so docker is the official distribution. `auto` uses whatever is on
    // PATH and falls back to the published images; force either with
    // OP_RUNTIME=docker or OP_RUNTIME=native. The three are versioned
    // independently upstream, so the tags do not match.
    opRuntime: process.env.OP_RUNTIME ?? "auto",
    opNodeImage: env(
        "OP_NODE_IMAGE",
        "us-docker.pkg.dev/oplabs-tools-artifacts/images/op-node:v1.19.3",
    ),
    opBatcherImage: env(
        "OP_BATCHER_IMAGE",
        "us-docker.pkg.dev/oplabs-tools-artifacts/images/op-batcher:v1.16.11",
    ),
    opProposerImage: env(
        "OP_PROPOSER_IMAGE",
        "us-docker.pkg.dev/oplabs-tools-artifacts/images/op-proposer:v1.16.3",
    ),
    opDeployerImage: env(
        "OP_DEPLOYER_IMAGE",
        "us-docker.pkg.dev/oplabs-tools-artifacts/images/op-deployer:v0.7.1",
    ),

    containerPrefix: "op-cartesi-devnet",
    dockerNetwork: "op-cartesi-devnet",
} as const;

/** The port half of a `host:port` listen address. */
export function portOf(addr: string): number {
    return Number(addr.slice(addr.lastIndexOf(":") + 1));
}

// --- consensus-relevant parameters ------------------------------------------
// These determine the L2 genesis block hash, so `generate-config.ts` and the
// engine must produce them identically — which is why they are one function
// rather than two lists. Read fresh, because chain-genesis.env and
// l1-addresses.env are written by processes that run after this module was
// first imported.
export function chainParams() {
    const files = deploymentVars();
    const value = (name: string, fallback: string) => pick(files, name) ?? fallback;
    const hash = (name: string, fallback: string): Hex => {
        const v = value(name, fallback).toLowerCase();
        if (!isHash(v)) throw new Error(`${name} is not a 32-byte hex hash: ${v}`);
        return v;
    };
    const addr = (name: string, fallback: string): Address => {
        const v = value(name, fallback).toLowerCase();
        if (!isAddress(v)) throw new Error(`${name} is not a 20-byte hex address: ${v}`);
        return v;
    };
    return {
        l1ChainId: Number(value("L1_CHAIN_ID", "900")),
        l2ChainId: Number(value("L2_CHAIN_ID", "901")),
        /** The L2 block time op-node sequences at. */
        blockTime: Number(value("BLOCK_TIME", "2")),
        gasLimit: value("GAS_LIMIT", "30000000"),
        maxCyclesPerInput: value("MAX_CYCLES_PER_INPUT", "1000000000"),
        checkpointInterval: value("CHECKPOINT_INTERVAL", "25"),
        /** Anchored to the L1 block the rollup starts after, so op-node's
         * derivation clock and the engine's genesis agree. Written by
         * procs/genesis.ts. */
        genesisTimestamp: value("GENESIS_TIMESTAMP", "0"),
        l1GenesisHash: hash("L1_GENESIS_HASH", `0x${"0".repeat(64)}`),
        l1GenesisNumber: value("L1_GENESIS_NUMBER", "0"),
        // The batcher's address must match the one in the rollup config's
        // genesis system config, or op-node ignores its batches.
        batcherAddress: addr("BATCHER_ADDRESS", ANVIL_0),
        batchInboxAddress: addr(
            "BATCH_INBOX_ADDRESS",
            "0xff00000000000000000000000000000000000901",
        ),
        depositContractAddress: addr(
            "DEPOSIT_CONTRACT_ADDRESS",
            "0x6900000000000000000000000000000000000001",
        ),
        l1SystemConfigAddress: addr(
            "L1_SYSTEM_CONFIG_ADDRESS",
            "0x6900000000000000000000000000000000000002",
        ),
        // Defaults match op-node's, but with contracts deployed these are read
        // off the real SystemConfig.
        baseFeeScalar: value("BASE_FEE_SCALAR", "1368"),
        blobBaseFeeScalar: value("BLOB_BASE_FEE_SCALAR", "810949"),
        // Consensus-relevant on the L1 side: op-node reads these back from
        // SystemConfig, so the deployment has to be told the same numbers.
        eip1559Denominator: value("EIP1559_DENOMINATOR", "50"),
        eip1559Elasticity: value("EIP1559_ELASTICITY", "6"),
    };
}

/** The deployment's addresses and keys, re-read on every call. */
export function addresses() {
    const files = deploymentVars();
    const value = (name: string, fallback: string) => pick(files, name) ?? fallback;
    const opt = (name: string): Address | undefined => {
        const v = pick(files, name)?.toLowerCase();
        return v !== undefined && isAddress(v) ? v : undefined;
    };
    const addr = (name: string, fallback: string): Address => {
        const v = value(name, fallback).toLowerCase();
        if (!isAddress(v)) throw new Error(`${name} is not a 20-byte hex address: ${v}`);
        return v;
    };
    return {
        batcherKey: value("BATCHER_KEY", ANVIL_0_KEY),
        // Anvil account 2, matching the proposer role in the op-deployer
        // intent — the permissioned game rejects proposals from anyone else.
        proposerKey: value("PROPOSER_KEY", ANVIL_2_KEY),
        proposerAddress: addr("PROPOSER_ADDRESS", ANVIL_2),
        sequencerAddress: addr("SEQUENCER_ADDRESS", SEQUENCER_ADDRESS_DEFAULT),
        // The deployer is kept off the batcher's and depositor's accounts so
        // nothing competes for a nonce. Anvil account 3 — which is also the
        // guest's owner, deliberately: the portal registration is an owner
        // call and the deploy is what sends it.
        deployerKey: value("DEPLOYER_KEY", ANVIL_3_KEY),
        deployerAddress: addr("DEPLOYER_ADDRESS", ANVIL_3),
        // The address the guest accepts configuration from — portal
        // registrations and the fee, through the config contract at
        // guestConfigAddress. It is baked into the snapshot (a Dockerfile ARG
        // of `demo/`, this same default), so it is consensus-relevant, and
        // it has to be known before anything is deployed, which is the point:
        // the portals are not.
        guestOwner: addr("GUEST_OWNER", ANVIL_3),
        guestConfigAddress: addr(
            "GUEST_CONFIG_ADDRESS",
            "0xc751000000000000000000000000000000000002",
        ),
        disputeGameFactory: opt("DISPUTE_GAME_FACTORY_ADDRESS"),
        // op-deployer registers only the permissioned game (type 1), which is
        // the right one here: there is no fault proof VM that can execute a
        // Cartesi Machine, so proposals are made by a trusted proposer and
        // never disputed. That is how OP chains launch.
        disputeGameType: value("DISPUTE_GAME_TYPE", "1"),
        outputsValidator: opt("OUTPUTS_VALIDATOR_ADDRESS"),
        outputExecutor: opt("OUTPUT_EXECUTOR_ADDRESS"),
        etherPortal: opt("ETHER_PORTAL_ADDRESS"),
        erc20Portal: opt("ERC20_PORTAL_ADDRESS"),
        l1StandardBridge: opt("L1_STANDARD_BRIDGE_ADDRESS"),
        l1CrossDomainMessenger: opt("L1_CROSS_DOMAIN_MESSENGER_ADDRESS"),
    };
}

/** Prints usage and exits — the `${1:?usage: …}` of the shell scripts. */
export function usage(message: string): never {
    console.error(`usage: ${message}`);
    process.exit(1);
}
