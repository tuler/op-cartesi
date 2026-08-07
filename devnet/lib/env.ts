// Shared configuration for the devnet client scripts — the TypeScript
// counterpart of env.sh, which the orchestration scripts still source. The
// two deliberately read the same variables, the same address files, and the
// same defaults, so a value set for one is set for both.
//
// User overrides can live in a `.env` at the repo root: bun loads it into
// process.env before any script code runs (from the invocation cwd — so run
// scripts from the repo root), and env.sh replays the same file with the
// same env-wins rule, so both surfaces see it. No code here reads .env;
// bun's auto-loading is the mechanism.
//
// Precedence, highest first, mirroring env.sh exactly:
//   1. the address files deploy scripts write (l1-addresses.env,
//      outputs-addresses.env, token.env) — machine-written deployment
//      outputs, sourced over everything (a bash `source` assigns
//      unconditionally);
//   2. the environment — which is where bun's .env loading lands, below
//      real exported variables;
//   3. the defaults below.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
    createPublicClient,
    createTestClient,
    createWalletClient,
    defineChain,
    http,
    rpcSchema,
    type Address,
    type Chain,
    type Hex,
} from "viem";
import { privateKeyToAccount } from "viem/accounts";

export const DEVNET_DIR = dirname(dirname(fileURLToPath(import.meta.url)));

function parseEnvFile(path: string): Record<string, string> {
    if (!existsSync(path)) return {};
    const vars: Record<string, string> = {};
    for (const line of readFileSync(path, "utf8").split("\n")) {
        const m = line.match(/^([A-Z0-9_]+)=("?)(.*)\2$/);
        if (m) vars[m[1]!] = m[3]!;
    }
    return vars;
}

const fileVars = {
    ...parseEnvFile(join(DEVNET_DIR, "l1-addresses.env")),
    ...parseEnvFile(join(DEVNET_DIR, "outputs-addresses.env")),
    ...parseEnvFile(join(DEVNET_DIR, "token.env")),
};

function env(name: string, fallback: string): string {
    return fileVars[name] ?? process.env[name] ?? fallback;
}

function optionalEnv(name: string): string | undefined {
    return fileVars[name] ?? process.env[name] ?? undefined;
}

/** The guards earn viem's template-literal types by inspection, the same
 * doctrine as the router's tx.ts: no casts, checked at runtime. */
function isAddress(v: string): v is Address {
    return /^0x[0-9a-f]{40}$/.test(v);
}

function isPrivateKey(v: string): v is Hex {
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

const httpAddr = env("HTTP_ADDR", "127.0.0.1:8545");
const l1Port = env("L1_PORT", "8600");

export const config = {
    l1ChainId: Number(env("L1_CHAIN_ID", "900")),
    l2ChainId: Number(env("L2_CHAIN_ID", "901")),
    l1Rpc: env("L1_RPC", `http://127.0.0.1:${l1Port}`),
    l2Rpc: env("L2_RPC", `http://${httpAddr}`),
    // Anvil account 1, kept off account 0 so deposits never race the
    // batcher's nonce.
    depositorKey: env(
        "DEPOSITOR_KEY",
        "0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
    ),
    // Anvil account 3 — the guest owner's key, and the default L2 sender the
    // old send-l2-tx.sh used.
    senderKey: env(
        "SENDER_KEY",
        "0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
    ),
    callerKey: env(
        "CALLER_KEY",
        "0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
    ),
    depositGasLimit: BigInt(env("DEPOSIT_GAS_LIMIT", "1000000")),
    depositContract: addressEnv("DEPOSIT_CONTRACT_ADDRESS", "0x6900000000000000000000000000000000000001"),
    disputeGameFactory: optionalAddress("DISPUTE_GAME_FACTORY_ADDRESS"),
    outputsValidator: optionalAddress("OUTPUTS_VALIDATOR_ADDRESS"),
    outputExecutor: optionalAddress("OUTPUT_EXECUTOR_ADDRESS"),
    erc20Portal: optionalAddress("ERC20_PORTAL_ADDRESS"),
    etherPortal: optionalAddress("ETHER_PORTAL_ADDRESS"),
    testToken: optionalAddress("TEST_TOKEN_ADDRESS"),
    tokenEnvFile: env("TOKEN_ENV_FILE", join(DEVNET_DIR, "token.env")),
};

export const l1Chain: Chain = defineChain({
    id: config.l1ChainId,
    name: "devnet-l1",
    nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
    rpcUrls: { default: { http: [config.l1Rpc] } },
});

// The L2 chain carries the OP-stack shape viem's op-stack actions resolve
// against: sourceId names the L1, and contracts are keyed by it — so
// `targetChain: l2Chain` finds the portal without every script naming an
// address. Deliberately not annotated `: Chain`: the widened type would
// erase the contracts entry the targetChain parameter's type requires.
export const l2Chain = defineChain({
    id: config.l2ChainId,
    name: "op-cartesi-devnet",
    nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
    rpcUrls: { default: { http: [config.l2Rpc] } },
    sourceId: config.l1ChainId,
    contracts: {
        portal: { [config.l1ChainId]: { address: config.depositContract } },
        ...(config.disputeGameFactory
            ? { disputeGameFactory: { [config.l1ChainId]: { address: config.disputeGameFactory } } }
            : {}),
    },
});

/** The cartesi_ RPC surface the scripts consume (engineapi/cartesi.go). */
type CartesiRpcSchema = [
    {
        Method: "cartesi_getTransactionEmissions";
        Parameters: [Hex];
        ReturnType: {
            outputs: { index: Hex; payload: Hex }[];
            reports: Hex[];
        };
    },
    {
        Method: "cartesi_getOutputProof";
        Parameters: [Hex, Hex];
        ReturnType: { output: Hex; outputHashesSiblings: Hex[] };
    },
];

export function l1Public() {
    return createPublicClient({ chain: l1Chain, transport: http() });
}

export function l1Wallet(key: string) {
    return createWalletClient({
        chain: l1Chain,
        transport: http(),
        account: privateKeyToAccount(asKey(key)),
    });
}

/** Anvil-only management calls (the contract-less deposit path). */
export function l1Test() {
    return createTestClient({ chain: l1Chain, transport: http(), mode: "anvil" });
}

export function l2Public() {
    return createPublicClient({
        chain: l2Chain,
        transport: http(),
        rpcSchema: rpcSchema<CartesiRpcSchema>(),
    });
}

export function l2Wallet(key: string) {
    return createWalletClient({
        chain: l2Chain,
        transport: http(),
        account: privateKeyToAccount(asKey(key)),
    });
}

function asKey(key: string): Hex {
    const k = key.toLowerCase();
    if (!isPrivateKey(k)) throw new Error(`not a 32-byte hex private key: ${key}`);
    return k;
}

/** Prints usage and exits — the `${1:?usage: …}` of the shell scripts. */
export function usage(message: string): never {
    console.error(`usage: ${message}`);
    process.exit(1);
}
