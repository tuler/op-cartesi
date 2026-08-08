#!/usr/bin/env bun
// The OP Stack L1 contract suite, deployed with op-deployer.
//
// Optional (WITH_CONTRACTS=0 leaves this pane out), but it runs before the
// rollup is anchored rather than alongside it: op-node reads the SystemConfig
// at the anchor block to start derivation, and before the deployment there is
// nothing there to read. So genesis waits for this to finish, and everything
// downstream waits for genesis.

import { deployL1 } from "../deploy-l1.ts";
import { paths, stack } from "../lib/env.ts";
import { clearReady, forget, markReady, procInit, say, waitForPort } from "../lib/proc.ts";

procInit("l1-contracts");
clearReady("l1-contracts");
await waitForPort(stack.l1Port, "anvil");

// A fresh anvil forgets every deployment, so anything recorded by a previous
// run is stale the moment it boots.
forget(paths.l1Addresses);
await deployL1();
markReady("l1-contracts");

say("L1 contracts deployed; the pane stays down from here");
