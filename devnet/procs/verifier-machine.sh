#!/usr/bin/env bash
# The verifier's Cartesi Machine server — its own, because a server holds
# exactly one machine. Guest console output for the verifying node lands here.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init verifier-machine

devnet_say "verifier machine server on :$VERIFIER_MACHINE_PORT"
exec cartesi-jsonrpc-machine --server-address="127.0.0.1:$VERIFIER_MACHINE_PORT"
