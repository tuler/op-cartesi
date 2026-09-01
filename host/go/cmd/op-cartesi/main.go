// op-cartesi is the Engine API shim that lets a Cartesi Machine serve as the
// execution layer of an OP Stack chain. It speaks engine_* + a minimal eth_*
// subset to op-node on an authenticated port, eth_* to everything else on a
// public port, and drives a Cartesi Machine (remote emulator, or an in-memory
// mock for development) behind them.
//
// Subcommands:
//
//	run      serve the Engine API and eth RPC (default)
//	genesis  print the rollup.json op-node needs to derive this chain
//	config   print the chain configuration document every node must share
package main

import (
	"fmt"
	"log/slog"
	"os"
)

func main() {
	args := os.Args[1:]
	command := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}

	var err error
	switch command {
	case "run":
		err = runCommand(args)
	case "genesis":
		err = genesisCommand(args)
	case "config":
		err = configCommand(args)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stderr, "usage: op-cartesi [run|genesis|config] [flags]\n\n"+
			"  run      serve the Engine API (op-node) and eth RPC\n"+
			"  genesis  print the rollup.json op-node needs to derive this chain\n"+
			"  config   print the chain configuration document every node must share\n\n"+
			"Run 'op-cartesi <command> -h' for the flags of each command.\n")
		return
	default:
		err = fmt.Errorf("unknown command %q; try 'op-cartesi help'", command)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
