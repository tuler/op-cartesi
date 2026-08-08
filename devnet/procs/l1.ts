#!/usr/bin/env bun
// anvil, the devnet L1.
//
// Deliberately not --silent: this pane is where you watch L1 accept the
// batcher's calldata and the proposer's games.

import { chainParams, stack } from "../lib/env.ts";
import { addressing } from "../lib/optools.ts";
import { exec, procInit, say } from "../lib/proc.ts";

procInit("l1");
const { l1BindAddr } = addressing();
const { l1ChainId } = chainParams();

say(`anvil (L1, chain ${l1ChainId}) on ${l1BindAddr}:${stack.l1Port}`);
await exec([
    "anvil",
    "--host",
    l1BindAddr,
    "--port",
    String(stack.l1Port),
    "--chain-id",
    String(l1ChainId),
    "--block-time",
    String(stack.l1BlockTime),
]);
