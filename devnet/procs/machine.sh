#!/usr/bin/env bash
# The Cartesi Machine server the sequencer's engine drives.
#
# This pane is the guest's console: the emulator writes the machine's own
# output here — Linux booting, then whatever the guest program (ledger-app)
# prints — including from the servers op-cartesi forks per block, which
# inherit these file descriptors. The guest's *structured* account of each
# transaction, the reports it emits, is the `guest` pane instead.

source "$(dirname "${BASH_SOURCE[0]}")/../lib/devnet.sh"
proc_init machine

devnet_say "machine server on :$MACHINE_PORT (snapshot $SNAPSHOT_DIR)"
exec cartesi-jsonrpc-machine --server-address="127.0.0.1:$MACHINE_PORT"
