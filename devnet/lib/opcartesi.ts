// Invoking op-cartesi itself: the flags that decide what chain it serves, and
// the two subcommands the devnet uses.
//
// `genesis` and `run` must be given the same chain flags — they are what
// determine the L2 genesis block hash, so a rollup config generated with one
// set and an engine started with another is a chain op-node rejects for
// serving the wrong genesis. Hence one function producing both.

import { existsSync, writeFileSync } from "node:fs";
import { chainParams, paths, stack } from "./env.ts";
import { exec, must, say } from "./proc.ts";

/** The engine, as something to run.
 *
 * In a container it is a binary on PATH, named by OP_CARTESI_BIN. On a host it
 * is whatever `go build -o bin/` left behind, and failing that `go run` — a
 * wrapper around the process that matters, which is fine for the steps that
 * use it here (they run it to completion) and not for serving a chain. */
export function opCartesi(): string[] {
    if (process.env.OP_CARTESI_BIN) return [process.env.OP_CARTESI_BIN];
    if (existsSync(paths.opCartesiBin)) return [paths.opCartesiBin];
    return ["go", "run", "./host/go/cmd/op-cartesi"];
}

export interface MachineOptions {
    /** A cartesi-jsonrpc-machine server to run the real machine on. Empty
     * runs the deterministic in-memory mock. */
    remote?: string;
    /** Directory of a stored machine for the server to load. */
    snapshot?: string;
}

/** Writes the chain configuration document: the consensus parameters every
 * node of this chain must agree on (docs/BLOCKS-SPEC.md §4).
 *
 * The engine writes it rather than this file, so there is one implementation
 * of what the parameters are and what their defaults mean. This used to be a
 * list of Go command-line flags built here and pasted onto both `genesis` and
 * `run` — which worked while the only engine was the Go one, and is exactly
 * what an engine in another language could not consume. */
export async function writeChainConfig(): Promise<void> {
    const p = chainParams();
    await must([
        ...opCartesi(),
        "config",
        "-chain-id",
        String(p.l2ChainId),
        "-genesis.timestamp",
        p.genesisTimestamp,
        "-gas-limit",
        p.gasLimit,
        "-max-cycles-per-input",
        p.maxCyclesPerInput,
        "-out",
        paths.chainConfig,
    ]);
}

/** How an engine is told which chain to serve, and which machine to drive.
 * The first half is the document; the second is this node's own business and
 * differs between the sequencer and the verifier. */
export function engineFlags(machine: MachineOptions = {}): string[] {
    const remote = machine.remote ?? process.env.MACHINE_REMOTE ?? "";
    const snapshot = machine.snapshot ?? process.env.MACHINE_SNAPSHOT ?? "";
    return [
        "-chain-config",
        paths.chainConfig,
        ...(remote ? ["-machine.remote", remote] : []),
        ...(snapshot ? ["-machine.snapshot", snapshot] : []),
    ];
}

/** Writes a fresh 32-byte hex secret if there is none.
 *
 * op-node needs it before op-cartesi would otherwise create it, and in docker
 * it is a bind mount, which docker would silently create as a directory if the
 * file were missing. */
export function ensureJwt(): void {
    if (existsSync(paths.jwtSecret)) return;
    const secret = crypto.getRandomValues(new Uint8Array(32));
    writeFileSync(paths.jwtSecret, `${Buffer.from(secret).toString("hex")}\n`);
    say(`wrote a new JWT secret to ${paths.jwtSecret}`);
}

/** Generates the rollup.json op-node derives this chain from. */
export async function generateRollupConfig(machine: MachineOptions): Promise<void> {
    const p = chainParams();
    await must(
        [
            ...opCartesi(),
            "genesis",
            ...engineFlags(machine),
            "-out",
            paths.rollupConfig,
            "-l1.chain-id",
            String(p.l1ChainId),
            "-l1.genesis-hash",
            p.l1GenesisHash,
            "-l1.genesis-number",
            p.l1GenesisNumber,
            "-block-time",
            String(p.blockTime),
            "-batcher-address",
            p.batcherAddress,
            "-batch-inbox-address",
            p.batchInboxAddress,
            "-deposit-contract-address",
            p.depositContractAddress,
            "-l1-system-config-address",
            p.l1SystemConfigAddress,
            "-base-fee-scalar",
            p.baseFeeScalar,
            "-blob-base-fee-scalar",
            p.blobBaseFeeScalar,
        ],
        { cwd: paths.repo },
    );
    say(`wrote ${paths.rollupConfig}`);
}

export interface EngineOptions extends MachineOptions {
    /** The authenticated Engine API op-node drives. */
    engineAddr: string;
    /** The public eth_* RPC everything else uses. */
    httpAddr: string;
    /** Directory for the persistent chain store; empty keeps it in memory. */
    dataDir?: string;
}

/** Runs the engine in the foreground and exits with it. */
export function runEngine(options: EngineOptions): Promise<never> {
    const dataDir = options.dataDir ?? stack.dataDir;
    return exec(
        [
            ...opCartesi(),
            "run",
            ...engineFlags(options),
            ...(dataDir ? ["-datadir", dataDir] : []),
            "-engine.addr",
            options.engineAddr,
            "-http.addr",
            options.httpAddr,
            "-engine.jwt-secret",
            paths.jwtSecret,
        ],
        { cwd: paths.repo },
    );
}
