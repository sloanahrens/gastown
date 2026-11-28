package main

import (
	"fmt"
	"io"
)

// treeRoot reads the work tree out of the sweep's command line: the value of
// --root, which sweepArgs puts first, so the command built in the swept tree
// sweeps that tree. Anything else is one stderr line naming the missing --root.
func treeRoot(args []string, stderr io.Writer) (work string, ok bool) {
	if len(args) < 2 || args[0] != "--root" || args[1] == "" {
		fmt.Fprintf(stderr, "%s: no --root work tree in the sweep's arguments %q\n", name, args)
		return "", false
	}
	return args[1], true
}
