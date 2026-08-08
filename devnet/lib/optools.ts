// Where op-node, op-batcher and op-proposer come from, and what everything
// addresses everything else by as a result.
//
// The OP monorepo publishes no binaries at all, so the tools run either from
// PATH or from the images it does publish. The choice is all-or-nothing rather
// than per-tool, because it also decides addressing: a container reaches the
// host over the gateway, a binary over loopback, and mixing the two would mean
// two addressing schemes in one stack. So `auto` picks native only when every
// tool this run will actually start is present — having op-node and op-batcher
// but not op-proposer used to select native and then fail on `op-proposer:
// command not found`, several minutes in.

import { paths, portOf, stack } from "./env.ts";
import { die, exec, run, say } from "./proc.ts";

export type OpRuntime = "native" | "docker";

/** The tools this run will actually start. */
export function opTools(): string[] {
    const tools = ["op-node"];
    if (stack.withBatcher) tools.push("op-batcher");
    if (stack.withProposer && stack.withContracts) tools.push("op-proposer");
    return tools;
}

/** Resolves `auto` without judging the result: start-devnet.ts validates and
 * then exports the answer, so the processes it starts inherit a decision
 * rather than each making their own. Running one on its own re-derives it,
 * which lands in the same place. */
export function opRuntime(): OpRuntime {
    if (stack.opRuntime === "native" || stack.opRuntime === "docker") return stack.opRuntime;
    return opTools().every((tool) => Bun.which(tool) !== null) ? "native" : "docker";
}

/** op-deployer has no binaries at all, so deploying contracts means docker
 * even when op-node does not — and that alone forces anvil off loopback. */
export function deployerInDocker(): boolean {
    return stack.withContracts && Bun.which("op-deployer") === null;
}

/** Fails now rather than four minutes in. */
export function requireOpTools(runtime: OpRuntime): void {
    if (runtime === "native") {
        const missing = opTools().filter((tool) => Bun.which(tool) === null);
        if (missing.length > 0) die(`OP_RUNTIME=native but not on PATH: ${missing.join(" ")}`);
        return;
    }
    if (Bun.which("docker") === null) {
        die(`need docker, or all of: ${opTools().join(" ")} on PATH`);
    }
}

export interface Addressing {
    runtime: OpRuntime;
    /** How a process reaches anvil and the engine: the host gateway from a
     * container, loopback from a binary. */
    hostAddr: string;
    /** What the OP tools bind their own RPCs to. */
    bindAddr: string;
    /** What anvil binds to — 0.0.0.0 as soon as anything runs in a container,
     * including op-deployer on an otherwise native run. That is a devnet-only
     * trade: on a shared network it exposes anvil, which is why it is not what
     * the fully native path does. */
    l1BindAddr: string;
    engineAddr: string;
    httpAddr: string;
    verifierEngineAddr: string;
    verifierHttpAddr: string;
    /** The config paths as the tools see them: bind-mount targets in docker. */
    rollupConfigArg: string;
    jwtArg: string;
    l1ChainConfigArg: string;
    /** Where op-batcher and op-proposer find the sequencer's op-node. */
    sequencerRpc: string;
}

export function addressing(): Addressing {
    const runtime = opRuntime();
    const rebind = (addr: string, bind: string) => `${bind}:${portOf(addr)}`;
    if (runtime === "docker") {
        // op-batcher talks to op-node, and both are containers, so they share
        // a user-defined network and address each other by name. Going back
        // out through the host would mean publishing op-node's RPC on every
        // interface rather than just loopback — a port bound to the host's
        // loopback is not reachable from the bridge gateway, which is exactly
        // how this first failed.
        return {
            runtime,
            hostAddr: "host.docker.internal",
            bindAddr: "0.0.0.0",
            l1BindAddr: "0.0.0.0",
            engineAddr: rebind(stack.engineAddr, "0.0.0.0"),
            httpAddr: rebind(stack.httpAddr, "0.0.0.0"),
            verifierEngineAddr: rebind(stack.verifierEngineAddr, "0.0.0.0"),
            verifierHttpAddr: rebind(stack.verifierHttpAddr, "0.0.0.0"),
            rollupConfigArg: "/config/rollup.json",
            jwtArg: "/config/jwt.hex",
            l1ChainConfigArg: "/config/l1-chain-config.json",
            sequencerRpc: `http://${stack.containerPrefix}-op-node:${stack.opNodeRpcPort}`,
        };
    }
    return {
        runtime,
        hostAddr: "127.0.0.1",
        bindAddr: "127.0.0.1",
        l1BindAddr: deployerInDocker() ? "0.0.0.0" : "127.0.0.1",
        engineAddr: stack.engineAddr,
        httpAddr: stack.httpAddr,
        verifierEngineAddr: stack.verifierEngineAddr,
        verifierHttpAddr: stack.verifierHttpAddr,
        rollupConfigArg: paths.rollupConfig,
        jwtArg: paths.jwtSecret,
        l1ChainConfigArg: paths.l1ChainConfig,
        sequencerRpc: `http://127.0.0.1:${stack.opNodeRpcPort}`,
    };
}

/** The txmgr settings every OP tool that sends L1 transactions needs here.
 *
 * op-service's transaction manager is tuned for a 12-second L1 that reorgs:
 * it waits `--txmgr.receipt-query-interval` (12s) before it so much as looks
 * for the receipt, and calls a transaction confirmed only `--num-confirmations`
 * (10) blocks deep. Anvil mines every `L1_BLOCK_TIME` seconds and never
 * reorgs, so both numbers are wrong by an order of magnitude — and wrong in a
 * way that is not merely slow. While txmgr believes a transaction is still
 * unmined, a 12-second rebroadcast timer re-publishes it verbatim; against
 * anvil that timer always fires first, so every proposal is sent twice and
 * anvil rejects the second copy:
 *
 *     Error: Transaction rejected: nonce too low
 *
 * Nothing is lost when that happens — txmgr reads the rejection as proof the
 * nonce already landed and moves on, at debug level — but the L1 pane is where
 * you watch the batcher and the proposer work, and a rejection per transaction
 * is not what working looks like. Polling at the block time means the receipt
 * is seen a block after it is written, long before anything rebroadcasts.
 *
 * Rebroadcast itself is left on: it is the only thing that recovers a
 * transaction anvil dropped, and with the receipt seen in time it never fires.
 */
export function txmgrArgs(): string[] {
    return [
        "--num-confirmations=1",
        `--txmgr.receipt-query-interval=${Math.max(1, stack.l1BlockTime)}s`,
    ];
}

const images: Record<string, string> = {
    "op-node": stack.opNodeImage,
    "op-batcher": stack.opBatcherImage,
    "op-proposer": stack.opProposerImage,
};

/** Runs one OP tool in the foreground, so mprocs supervises it and its output
 * is the pane.
 *
 * In docker the container is not a child of anything, so stopping the client
 * would leave it running: it is removed by name on the way out, and the name
 * is prefixed so this only ever touches containers this devnet started. */
export async function runOp(
    kind: "op-node" | "op-batcher" | "op-proposer",
    name: string,
    rpcPort: number,
    args: string[],
): Promise<never> {
    const { runtime, rollupConfigArg, jwtArg, l1ChainConfigArg } = addressing();
    requireOpTools(runtime);
    say(`${kind} (${runtime}) rpc :${rpcPort}`);
    if (runtime === "native") return exec([kind, ...args]);

    const container = `${stack.containerPrefix}-${name}`;
    Bun.spawnSync(["docker", "network", "create", stack.dockerNetwork]);
    Bun.spawnSync(["docker", "rm", "-f", container]);
    let code: number;
    try {
        ({ code } = await run([
            "docker",
            "run",
            "--rm",
            "--name",
            container,
            "--network",
            stack.dockerNetwork,
            "--add-host=host.docker.internal:host-gateway",
            "-p",
            `127.0.0.1:${rpcPort}:${rpcPort}`,
            "-v",
            `${paths.rollupConfig}:${rollupConfigArg}:ro`,
            "-v",
            `${paths.jwtSecret}:${jwtArg}:ro`,
            "-v",
            `${paths.l1ChainConfig}:${l1ChainConfigArg}:ro`,
            images[kind]!,
            kind,
            ...args,
        ]));
    } finally {
        // The container outlives the client that started it, so it is removed
        // here rather than left to whatever stops this process.
        Bun.spawnSync(["docker", "rm", "-f", container]);
    }
    process.exit(code);
}
