#!/usr/bin/env bash
# The guest program's account of the chain: the reports it emits per
# transaction, read back over cartesi_getTransactionEmissions.
#
# The machine's console output — Linux booting, guest stdout — is the
# `machine` pane. This is the other half: what the guest says about each
# input, including why it rejected one.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init guest

wait_for_port "${HTTP_ADDR##*:}" "the engine's eth RPC"

# The engine may be bound to 0.0.0.0 (that is how a container reaches it), but
# this is on the host, so it always dials loopback.
export L2_RPC="http://127.0.0.1:${HTTP_ADDR##*:}"

# From the repo root, which is where bun finds .env and the workspace.
cd "$REPO_DIR"
exec bun "$DEVNET_DIR/guest-log.ts"
