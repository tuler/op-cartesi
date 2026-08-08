// Running things, and waiting for things to be running.
//
// mprocs starts every pane at once and has no notion of dependencies, so the
// ordering the bring-up needs is each process's own business: op-node blocks
// until the engine's port answers, the engine blocks until the rollup config
// exists, and so on. These are the primitives they block with — which is also
// what makes any single pane restartable, since restarting one re-runs
// exactly the same waits.

import {
    existsSync,
    mkdirSync,
    readdirSync,
    readFileSync,
    rmSync,
    unlinkSync,
    writeFileSync,
} from "node:fs";
import { connect } from "node:net";
import { join } from "node:path";
import { paths, stack } from "./env.ts";

const FORWARDED: NodeJS.Signals[] = ["SIGINT", "SIGTERM", "SIGHUP"];

/** Everything a process has to say about itself goes to stderr with a marker,
 * so its own narration is distinguishable from the output of the program it
 * supervises — which is the whole content of the pane below it.
 *
 * Written rather than console.error'd because bun paints console.error red on
 * a terminal, and "waiting for anvil" is not an error. */
export function say(message: string): void {
    process.stderr.write(`==> ${message}\n`);
}

export function die(message: string): never {
    console.error(`!!! ${message}`);
    process.exit(1);
}

// A pane is a terminal, and a viem error printed raw is forty lines of stack
// and metadata for one sentence of cause. Every process imports this module,
// so installing the handler here makes a failure read as a failure — with the
// whole thing still one environment variable away.
function reportAndExit(error: unknown): never {
    if (process.env.DEVNET_DEBUG) {
        console.error(error);
        process.exit(1);
    }
    const message =
        error instanceof Error
            ? // viem's errors carry a one-line summary; everything else has
              // to be cut down to its first line.
              ((error as { shortMessage?: string }).shortMessage ?? error.message.split("\n")[0]!)
            : String(error);
    console.error(`!!! ${message}`);
    console.error("    (DEVNET_DEBUG=1 for the full error)");
    process.exit(1);
}

process.on("uncaughtException", reportAndExit);
process.on("unhandledRejection", reportAndExit);

export const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// --- running ----------------------------------------------------------------

export interface RunOptions {
    cwd?: string;
    env?: Record<string, string>;
    /** Collect stdout instead of letting it through to the pane. */
    capture?: boolean;
}

export interface RunResult {
    code: number;
    stdout: string;
}

/** Runs a command in the foreground and resolves with its exit code.
 *
 * Bun cannot replace its own process image the way `exec` does in a shell, so
 * the child is a child — which matters twice. It shares this process's group,
 * so mprocs stopping the pane stops both; and signals arriving here are
 * forwarded, so a process supervised by anything else stops too. */
export async function run(cmd: string[], options: RunOptions = {}): Promise<RunResult> {
    const child = Bun.spawn(cmd, {
        cwd: options.cwd,
        env: { ...process.env, ...options.env },
        stdio: ["inherit", options.capture ? "pipe" : "inherit", "inherit"],
    });
    const forward = (signal: NodeJS.Signals) => {
        try {
            child.kill(signal);
        } catch {
            // Already gone; nothing to forward to.
        }
    };
    const handlers = FORWARDED.map((signal) => {
        const handler = () => forward(signal);
        process.on(signal, handler);
        return [signal, handler] as const;
    });
    try {
        const stdout = options.capture ? await new Response(child.stdout).text() : "";
        const code = await child.exited;
        return { code, stdout };
    } finally {
        for (const [signal, handler] of handlers) process.off(signal, handler);
    }
}

/** Runs a command and exits with its status — what a process script ends on,
 * standing in for the shell's `exec`. */
export async function exec(cmd: string[], options: RunOptions = {}): Promise<never> {
    const { code } = await run(cmd, options);
    process.exit(code);
}

/** Runs a command, failing loudly if it does. */
export async function must(cmd: string[], options: RunOptions = {}): Promise<RunResult> {
    const result = await run(cmd, options);
    if (result.code !== 0) die(`${cmd[0]} exited ${result.code}`);
    return result;
}

/** Whether every named command is on PATH, reporting all that are not: a
 * missing binary discovered by pane 7 four minutes in reads as a devnet that
 * is broken rather than a laptop that is missing a tool. */
export function requireCommands(...names: string[]): void {
    const missing = names.filter((name) => Bun.which(name) === null);
    if (missing.length > 0) die(`not on PATH: ${missing.join(" ")}`);
}

// --- ports ------------------------------------------------------------------

/** True when nothing is listening. A connection that is refused is the answer;
 * anything else (a firewall, a half-open socket) counts as in use. */
export function portFree(port: number, host = "127.0.0.1"): Promise<boolean> {
    return new Promise((resolve) => {
        const socket = connect({ port, host });
        const done = (free: boolean) => {
            socket.destroy();
            resolve(free);
        };
        socket.setTimeout(1000);
        socket.once("connect", () => done(false));
        socket.once("timeout", () => done(false));
        socket.once("error", () => done(true));
    });
}

/** Blocks until something answers on the port.
 *
 * For op-cartesi this is exact rather than approximate: it binds its listeners
 * only after the chain is open and the machine has booted, so a port that
 * answers is an engine that can serve. */
export async function waitForPort(port: number, what: string): Promise<void> {
    if (!(await portFree(port))) return;
    say(`waiting for ${what} on :${port}`);
    const deadline = Date.now() + stack.waitTimeoutMs;
    while (await portFree(port)) {
        if (Date.now() > deadline) {
            die(`gave up after ${stack.waitTimeoutMs / 1000}s waiting for ${what} on :${port}`);
        }
        await sleep(500);
    }
}

/** A port still held from a previous run is reported rather than cleared: the
 * process holding it may not be ours. This is also the failure the check
 * exists to make legible — a machine server left behind answers just well
 * enough for op-cartesi to connect and then fail with "machine exists". */
export async function requireFreePorts(ports: number[]): Promise<void> {
    const busy: number[] = [];
    for (const port of ports) if (!(await portFree(port))) busy.push(port);
    if (busy.length === 0) return;
    console.error(`ports already in use: ${busy.join(" ")}`);
    if (Bun.which("lsof") !== null) {
        for (const port of busy) {
            const pids = Bun.spawnSync(["lsof", "-ti", `tcp:${port}`])
                .stdout.toString()
                .trim()
                .split("\n");
            console.error(`  :${port} held by pid(s) ${pids.join(" ")}`);
        }
    }
    die("stop them (./devnet/stop-devnet.ts, or set the matching *_PORT variables) and try again");
}

// --- readiness --------------------------------------------------------------
// Steps that finish rather than run — the L1 deployment, the rollup config —
// announce themselves with a marker rather than by the file they produce
// appearing, because a file exists from its first byte and these are read
// whole by whoever waits.

const marker = (name: string) => join(paths.state, `${name}.ready`);

export function markReady(name: string): void {
    mkdirSync(paths.state, { recursive: true });
    writeFileSync(marker(name), "");
}

export function clearReady(name: string): void {
    rmSync(marker(name), { force: true });
}

export async function waitReady(name: string, what: string): Promise<void> {
    if (existsSync(marker(name))) return;
    say(`waiting for ${what}`);
    const deadline = Date.now() + stack.waitTimeoutMs;
    while (!existsSync(marker(name))) {
        if (Date.now() > deadline) {
            die(`gave up after ${stack.waitTimeoutMs / 1000}s waiting for ${what}`);
        }
        await sleep(500);
    }
}

// --- process bookkeeping ----------------------------------------------------
// mprocs starts each process in a session of its own and stops it by killing
// that whole process group, which is exactly right for the machine servers:
// op-cartesi forks one per block and the forks it prunes reparent to init,
// out of reach of any parent-to-child walk, but never out of their process
// group.
//
// What that leaves uncovered is mprocs itself being killed rather than quit.
// So each process records its group here, and stop-devnet.ts (which
// start-devnet.ts also runs on the way out) stops whatever is left.

const pidDir = () => join(paths.state, "pids");

/** Empty for a pid that is not running. A pid on its own is not an identity —
 * by the time anything reads the record, the process it names may be someone
 * else's — so the start time is what makes the pair one. */
export function procStartedAt(pid: number): string {
    const result = Bun.spawnSync(["ps", "-o", "lstart=", "-p", String(pid)]);
    if (result.exitCode !== 0) return "";
    return result.stdout.toString().trim().replace(/\s+/g, " ");
}

export function procInit(name: string): void {
    mkdirSync(pidDir(), { recursive: true });
    writeFileSync(join(pidDir(), name), `${process.pid}\t${procStartedAt(process.pid)}\n`);
}

interface Recorded {
    name: string;
    pid: number;
    alive: boolean;
}

function recorded(): Recorded[] {
    if (!existsSync(pidDir())) return [];
    return readdirSync(pidDir()).map((name) => {
        const [pid, started] = readFileSync(join(pidDir(), name), "utf8").trim().split("\t");
        const id = Number(pid);
        return { name, pid: id, alive: Number.isFinite(id) && procStartedAt(id) === started };
    });
}

/** The recorded processes that are still running, by pane name. */
export function runningProcs(): string[] {
    return recorded()
        .filter((p) => p.alive)
        .map((p) => p.name);
}

/** Signals the process group of everything still recorded as running,
 * forgetting any pid that is gone or has been recycled.
 *
 * Groups rather than pids, because of the machine servers reparented to init:
 * out of reach of a parent-to-child walk, but never out of their group. */
export async function stopRecordedProcs(signal: NodeJS.Signals = "SIGTERM"): Promise<void> {
    const stopped: string[] = [];
    for (const { name, pid, alive } of recorded()) {
        if (!alive) {
            rmSync(join(pidDir(), name), { force: true });
            continue;
        }
        try {
            process.kill(-pid, signal);
        } catch {
            try {
                process.kill(pid, signal);
            } catch {
                // Gone between the check and the signal.
            }
        }
        stopped.push(name);
    }
    if (stopped.length > 0) {
        say(`sent ${signal} to: ${stopped.join(" ")}`);
        await sleep(1000);
    }
}

export function forgetRecordedProcs(): void {
    rmSync(pidDir(), { recursive: true, force: true });
}

/** Containers are not children of anything here, so they go by name — and the
 * names are prefixed, so this only ever touches what this devnet started. */
export function removeContainers(): void {
    if (Bun.which("docker") === null) return;
    const listed = Bun.spawnSync([
        "docker",
        "ps",
        "-aq",
        "--filter",
        `name=^${stack.containerPrefix}-`,
    ]);
    const ids = listed.stdout.toString().trim().split("\n").filter(Boolean);
    if (ids.length > 0) {
        Bun.spawnSync(["docker", "rm", "-f", ...ids]);
        say(`removed the ${stack.containerPrefix}-* containers`);
    }
    Bun.spawnSync(["docker", "network", "rm", stack.dockerNetwork]);
}

/** Removes a file if it is there. */
export function forget(path: string): void {
    try {
        unlinkSync(path);
    } catch {
        // Not there is the desired state either way.
    }
}
