package main

import (
	"fmt"
	"io"
)

// runBuilt runs the swept tree's built sweep command bin in work with args and
// env, and returns its exit status, or a negative value when it produced none.
// The first stderr line names bin, so the log shows which command ran before it
// starts (gt-aryn31).
func (t treeSweep) runBuilt(bin, work string, args, env []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "%s: the sweep is the tree's %s, run as %s\n", name, goSweepDir, bin)
	return t.Run(bin, args, work, env, stdout, stderr)
}
