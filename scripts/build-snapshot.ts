#!/usr/bin/env bun
// Builds the Cartesi Machine snapshot that becomes the chain's genesis state:
// the routed guest of `demo/` (docs/EVM-COMPAT.md), built with
// `cartesi build` (@cartesi/cli 2.0 alpha) into demo/.cartesi/image —
// a stored machine, booted and parked at its first input yield, which is what
// makes genesis reproducible: the chain's genesis state root is the stored
// machine's own root hash.
//
// Genesis parameters travel differently than they did for the Lua guest:
// CHAIN_ID and OWNER are Dockerfile build ARGs whose defaults match lib/env.ts
// (chain 901, owner anvil account 3). To deviate, set build_args on
// [drives.root] in demo/cartesi.toml — they are consensus parameters,
// baked into the image environment and covered by the genesis state root.
// The accounts drive geometry lives in the same cartesi.toml.
//
// Requires: @cartesi/cli (npm i -g @cartesi/cli@alpha), docker with riscv64
// support, and `bun install` run once at the repo root (the app is a bun
// workspace member; the build's docker context is the repo root).

import { join } from "node:path";
import { addresses, chainParams, paths, stack } from "devnet/env";
import { die, must } from "devnet/proc";

if (Bun.which("cartesi") === null) {
    die("cartesi is not on PATH; install it with: npm i -g @cartesi/cli@alpha");
}

const { guestOwner } = addresses();
const { l2ChainId } = chainParams();
if (
    guestOwner.toLowerCase() !== "0x90f79bf6eb2c4f870365e785982e1f101e93b906" ||
    l2ChainId !== 901
) {
    console.error("note: GUEST_OWNER/L2_CHAIN_ID differ from the image defaults; set matching");
    console.error("build_args on [drives.root] in demo/cartesi.toml or the snapshot will");
    console.error("disagree with the chain flags.");
}

await must(["cartesi", "build"], { cwd: join(paths.repo, "demo") });

console.error(`stored machine snapshot in ${stack.snapshotDir}`);
console.error("this is the chain's genesis state; bring the devnet up with: docker compose up");
