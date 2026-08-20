#!/usr/bin/env bun
// The OP Stack L1 suite: deploy-l1.ts, plus the one question compose makes
// worth asking — has this already been done?
//
// `docker compose up` is not a run, it is a statement about what should be up,
// and running it again to add a service is ordinary. So this deploys when the
// recorded deployment is gone, and stands aside when it is still on L1
// (compose/live.ts).

import { deployL1 } from "../deploy-l1.ts";
import { paths, readEnvFile } from "../lib/env.ts";
import { alreadyDone, deploymentStillOnL1 } from "./live.ts";

const recorded = readEnvFile(paths.l1Addresses);
if (await deploymentStillOnL1(recorded)) {
    alreadyDone(
        `the L1 suite is already deployed (OptimismPortal ${recorded.DEPOSIT_CONTRACT_ADDRESS}, anchored at block ${recorded.L1_GENESIS_NUMBER})`,
    );
}

await deployL1();
