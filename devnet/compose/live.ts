// Is the chain the files describe the chain that is running?
//
// `docker compose up` is a statement about the state you want, not a script,
// and it is normal to run it again — to add the batcher to a stack that is
// already sequencing, or after a `stop`. Compose starts every service in the
// graph when you do, one-shots included, so without this the second `up`
// redeploys the L1 suite and re-anchors the rollup underneath an engine that
// is still serving the old chain. Nothing errors; the stack just stops
// agreeing with itself.
//
// So the two steps that produce the chain ask first whether their own output
// is still true, and a step whose work is already done exits 0 without
// repeating it. The question is answered against L1 rather than against a
// marker, because the thing that invalidates it is anvil forgetting
// everything: a devnet L1 restarts empty, so the recorded block is either
// still there with the hash the deploy committed to, or the deployment it
// belongs to is gone.

import type { Address } from "viem";
import { say } from "../lib/proc.ts";
import { l1Public } from "../lib/wallet.ts";

/** Earns viem's template-literal type by inspection rather than by cast, the
 * same doctrine as lib/env.ts. */
function isAddress(v: string): v is Address {
    return /^0x[0-9a-fA-F]{40}$/.test(v);
}

/** Whether L1 still has the block the deployment recorded, by hash. */
export async function anchorStillOnL1(recorded: Record<string, string>): Promise<boolean> {
    const hash = recorded.L1_GENESIS_HASH;
    const number = recorded.L1_GENESIS_NUMBER;
    if (!hash || !number) return false;
    try {
        const block = await l1Public().getBlock({ blockNumber: BigInt(number) });
        return block.hash === hash;
    } catch {
        // No such block: an L1 shorter than the one the deploy ran against,
        // which is what a restarted anvil looks like.
        return false;
    }
}

/** Whether the recorded OptimismPortal is still code on this L1 — the
 * deployment's own answer to "are you still there", as opposed to the anchor's
 * "is this the same chain". */
export async function deploymentStillOnL1(recorded: Record<string, string>): Promise<boolean> {
    const portal = recorded.DEPOSIT_CONTRACT_ADDRESS;
    if (portal === undefined || !isAddress(portal)) return false;
    if (!(await anchorStillOnL1(recorded))) return false;
    const code = await l1Public().getCode({ address: portal });
    return code !== undefined && code !== "0x";
}

/** Says what was found and why the step is doing nothing. */
export function alreadyDone(what: string): never {
    say(`${what} — nothing to do`);
    process.exit(0);
}
