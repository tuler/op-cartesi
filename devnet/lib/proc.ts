// Running things.
//
// What is left of the shell's job in this package: spawn a program, wait for
// it, fail loudly, and forget a file. The waiting a bring-up needs — for a
// port to answer, for a step to have finished, for a stack to be gone — is not
// here, because compose does it: a healthcheck, a `depends_on` condition, a
// `down`. This file is what the steps themselves are written with.

import { unlinkSync } from "node:fs";

const FORWARDED: NodeJS.Signals[] = ["SIGINT", "SIGTERM", "SIGHUP"];

/** Everything a process has to say about itself goes to stderr with a marker,
 * so its own narration is distinguishable from the output of the program it
 * supervises — which is the rest of `docker compose logs <service>`.
 *
 * Written rather than console.error'd because bun paints console.error red on
 * a terminal, and "wrote rollup.json" is not an error. */
export function say(message: string): void {
    process.stderr.write(`==> ${message}\n`);
}

export function die(message: string): never {
    console.error(`!!! ${message}`);
    process.exit(1);
}

// A container's log is a terminal, and a viem error printed raw is forty lines
// of stack and metadata for one sentence of cause. Every step imports this
// module, so installing the handler here makes a failure read as a failure —
// with the whole thing still one environment variable away.
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

// --- running ----------------------------------------------------------------

export interface RunOptions {
    cwd?: string;
    env?: Record<string, string>;
    /** Collect stdout instead of letting it through to the log. */
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
 * so a container stopping stops both; and signals arriving here are forwarded,
 * so `docker compose stop` reaches the program that is actually doing the
 * work. */
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

/** Runs a command and exits with its status — what a step ends on, standing in
 * for the shell's `exec`. */
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

/** Removes a file if it is there. */
export function forget(path: string): void {
    try {
        unlinkSync(path);
    } catch {
        // Not there is the desired state either way.
    }
}
