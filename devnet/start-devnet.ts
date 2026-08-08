#!/usr/bin/env bun
// Brings up the whole stack — anvil as L1, the OP Stack L1 contracts, a
// Cartesi Machine, op-cartesi as the execution engine, op-node sequencing on
// top, a batcher, a proposer, and a second node verifying from L1 alone — with
// every piece running in a pane of its own under mprocs.
//
// This script does not start any of them. It checks that the run can succeed,
// writes the mprocs config for the pieces this run wants, and hands over. The
// processes themselves are one script each under devnet/procs/, and each waits
// for what it depends on rather than being started in order — which is what
// makes any single one restartable from the UI (`r`) without restarting the
// stack.
//
//   ./devnet/start-devnet.ts                     the lot
//   WITH_CONTRACTS=0 ./devnet/start-devnet.ts    no L1 contracts: blocks and
//                                                derivation, no deposits
//   WITH_VERIFIER=0 WITH_BATCHER=0 ./devnet/start-devnet.ts   sequencer only
//
// Prerequisites: anvil, cartesi-jsonrpc-machine, go, and either
// op-node/op-batcher/op-proposer on PATH or docker. Plus a machine snapshot:
// ./devnet/build-snapshot.ts, once.

import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { paths, portOf, stack } from "./lib/env.ts";
import { buildOpCartesi, ensureJwt } from "./lib/opcartesi.ts";
import { deployerInDocker, opRuntime, requireOpTools } from "./lib/optools.ts";
import {
    die,
    forget,
    forgetRecordedProcs,
    removeContainers,
    requireCommands,
    requireFreePorts,
    run,
    runningProcs,
    say,
    stopRecordedProcs,
} from "./lib/proc.ts";

if (process.argv[2] === "-h" || process.argv[2] === "--help") {
    // The header comment above, which is the usage: everything from the line
    // after the shebang up to the first line that is not a comment.
    for (const line of readFileSync(new URL(import.meta.url).pathname, "utf8")
        .split("\n")
        .slice(1)) {
        if (!line.startsWith("//")) break;
        console.log(line.replace(/^\/\/ ?/, ""));
    }
    process.exit(0);
}

const mprocsConfig = process.env.MPROCS_CONFIG ?? join(paths.state, "mprocs.json");

// --- what this run will start -----------------------------------------------
const runtime = opRuntime();
// Resolved once and passed down, so the processes this starts inherit a
// decision rather than each making their own.
process.env.OP_RUNTIME = runtime;

// mprocs comes from the repo's own node_modules first, so everyone runs the
// version bun install pinned; a global one, or bunx, is the fallback.
function resolveMprocs(): string[] {
    const pinned = [
        join(paths.devnet, "node_modules", ".bin", "mprocs"),
        join(paths.repo, "node_modules", ".bin", "mprocs"),
    ];
    if (process.env.MPROCS) return [process.env.MPROCS];
    for (const path of pinned) if (existsSync(path)) return [path];
    if (Bun.which("mprocs") !== null) return ["mprocs"];
    if (Bun.which("bunx") !== null) return ["bunx", "mprocs"];
    die("mprocs not found: run `bun install` at the repo root, or npm i -g mprocs");
}
const mprocs = resolveMprocs();

// --- preflight --------------------------------------------------------------
// Everything that can be known to fail before any process starts is checked
// here, because a missing binary discovered by pane 7 four minutes in reads as
// a devnet that is broken rather than a laptop that is missing a tool.

// mprocs is a terminal UI and says so itself — but only after everything below
// has been checked and built, which is a long way to go to be told that.
if (!process.stdin.isTTY || !process.stdout.isTTY) {
    die(
        "mprocs needs a terminal; run this from an interactive shell (the individual processes under devnet/procs/ can be run on their own)",
    );
}

// Bringing a second devnet up over a running one would delete the records the
// first is stopped from, leaving it running and unstoppable. The port check
// below usually catches this first — but not if the running one was started on
// other ports.
const already = runningProcs();
if (already.length > 0) {
    die(`a devnet is already running (${already.join(" ")}) — ./devnet/stop-devnet.ts first`);
}

requireCommands("anvil", "cartesi-jsonrpc-machine", "go");
requireOpTools(runtime);
if (deployerInDocker()) requireCommands("docker");

if (!existsSync(stack.snapshotDir)) {
    die(`no machine snapshot at ${stack.snapshotDir} — run ./devnet/build-snapshot.ts first`);
}

// op-node needs the secret before op-cartesi would otherwise create it, and in
// docker it is a bind mount, which docker would silently create as a directory
// if the file were missing.
ensureJwt();

const ports = [
    stack.l1Port,
    stack.machinePort,
    stack.genesisMachinePort,
    stack.opNodeRpcPort,
    portOf(stack.engineAddr),
    portOf(stack.httpAddr),
    ...(stack.withBatcher ? [stack.batcherRpcPort] : []),
    ...(stack.withProposer && stack.withContracts ? [stack.proposerRpcPort] : []),
    ...(stack.withVerifier
        ? [
              stack.verifierMachinePort,
              stack.verifierOpNodeRpcPort,
              portOf(stack.verifierEngineAddr),
              portOf(stack.verifierHttpAddr),
          ]
        : []),
];
await requireFreePorts(ports);

// Compiled once, here, rather than `go run` in three panes: the panes then
// hold the engine itself, so mprocs stops the engine when it stops the pane
// instead of stopping a toolchain wrapper and leaving its child behind. It
// also means a compile error shows up now, not as a pane that exits.
await buildOpCartesi();

// --- a run starts from nothing ----------------------------------------------
// A fresh anvil forgets every deployment, so every address and anchor a
// previous run recorded is stale the moment it boots. Removing the files makes
// the scripts say "run ./devnet/deploy-outputs.ts first" instead of no-op'ing
// transactions against codeless addresses.
rmSync(paths.state, { recursive: true, force: true });
rmSync(paths.logs, { recursive: true, force: true });
mkdirSync(paths.state, { recursive: true });
mkdirSync(paths.logs, { recursive: true });
for (const file of [
    paths.l1Addresses,
    paths.outputsAddresses,
    paths.chainGenesis,
    paths.rollupConfig,
]) {
    forget(file);
}

if (runtime === "docker") {
    removeContainers();
    Bun.spawnSync(["docker", "network", "create", stack.dockerNetwork]);
}

// --- the panes --------------------------------------------------------------
// Written rather than checked in, because which processes exist is a function
// of the WITH_* flags: a devnet without a batcher should not show a batcher
// pane that is permanently down. Order is the order of the pipeline, and
// mprocs keeps it.
interface Pane {
    autostart: boolean;
    cmd: string[];
}

const panes: Record<string, Pane> = {};
const pane = (name: string, autostart = true) => {
    panes[name] = { autostart, cmd: [process.execPath, join(paths.devnet, "procs", `${name}.ts`)] };
};

pane("info");
pane("l1");
if (stack.withContracts) pane("l1-contracts");
pane("genesis");
pane("machine");
pane("engine");
if (stack.withGuestLog) pane("guest");
pane("op-node");
if (stack.withBatcher) pane("op-batcher");
if (stack.withProposer && stack.withContracts) pane("op-proposer");
if (stack.withVerifier) {
    pane("verifier-machine");
    pane("verifier-engine");
    pane("verifier-node");
}
// The one step that waits for you: deploying the outputs suite needs a
// proposal to exist, and not every run wants assets. Press `s` on it.
if (stack.withContracts) pane("outputs", false);

writeFileSync(
    mprocsConfig,
    `${JSON.stringify({ proc_list_title: "op-cartesi devnet", scrollback: 20000, procs: panes }, null, 2)}\n`,
);

// --- hand over --------------------------------------------------------------
// The endpoint summary is the `info` pane rather than something printed here:
// mprocs takes over the whole screen, so anything written now is gone the
// moment it starts.
say(`starting mprocs (${runtime} OP tools); logs also go to ${paths.logs}/<pane>.log`);

// mprocs stops every process it started when it quits, killing each one's
// whole process group — which is what the machine servers need, since the
// forks op-cartesi prunes reparent to init and out of reach of any walk. This
// covers the other ending: mprocs killed rather than quit.
async function teardown(): Promise<void> {
    removeContainers();
    await stopRecordedProcs("SIGTERM");
    await stopRecordedProcs("SIGKILL");
    forgetRecordedProcs();
}

const { code } = await run([...mprocs, "--config", mprocsConfig, "--log-dir", paths.logs]);
await teardown();
process.exit(code);
