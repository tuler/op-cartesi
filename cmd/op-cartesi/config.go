package main

import (
	"flag"
	"fmt"
	"os"
)

// configCommand writes the chain configuration document: the consensus
// parameters of docs/BLOCKS-SPEC.md §4, complete and explicit, from the flags
// or from another document.
//
// It exists so that a deployment writes those parameters down **once** and
// every node reads that file, rather than each being handed a command line
// that has to match. They cannot be checked against each other after the
// fact: §4.1 shows up as a genesis hash op-node rejects, but §4.2 is
// invisible to that handshake and surfaces as a state root divergence at the
// first block that notices.
//
// It also normalizes: a document with fields omitted comes back with the
// defaults filled in, so what a node was told is a thing a reader can see.
func configCommand(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	var cf chainFlags
	cf.register(fs)
	out := fs.String("out", "-", "output file, or - for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	params, err := cf.params()
	if err != nil {
		return err
	}

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := params.Write(w); err != nil {
		return fmt.Errorf("writing the chain configuration: %w", err)
	}
	return nil
}
