#!/usr/bin/env bash
# The outputs suite: the validator that opens an OP proposal, the executor
# that pays vouchers against it, and the Cartesi-style portals — plus the
# portal registration the guest needs before it will credit deposits.
#
# This one does not autostart. It is the step you take when you want to move
# assets rather than just watch blocks, and it needs a proposal to exist
# first, so it is a pane you start yourself (`s` in mprocs) once the proposer
# has had a chance to propose. Everything it writes lands in
# devnet/outputs-addresses.env, which env.sh and lib/env.ts both read.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init outputs

wait_ready l1-contracts "the L1 contract deployment"
reload_addresses
wait_for_port "${HTTP_ADDR##*:}" "the engine's eth RPC"

"$DEVNET_DIR/deploy-outputs.sh"
devnet_say "the outputs suite is deployed; deposits and withdrawals will find it"
