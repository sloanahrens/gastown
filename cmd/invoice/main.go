// Command invoice prints one workspace's monthly invoice from the town's
// metering files (gt-ehbgh).
//
// It is read-only: it reads the seat-end ledger, the nightly tracker and Forgejo
// size files and the per-bead usage records under <town>/.runtime, plus the
// terms file named by -terms, and prints internal/metering/invoice's text to
// standard output, followed by a few notes on standard error. Sending,
// storing and PDF output are other beads.
//
// Exit status: 0 when the invoice printed, 1 when a source or the render failed,
// 2 on a bad argument or terms file. A failed read prints no invoice. The terms
// file is opened through the same tree, so its path is relative to
// <town>/.runtime.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain opens the town's .runtime tree and runs the command against it. It
// parses -town here because run needs the tree before it can read anything;
// run parses the same arguments again and holds every check that follows.
func realMain(args []string, stdout, stderr io.Writer) int {
	var o options
	if err := newFlagSet(stderr, &o).Parse(args); err != nil {
		return 2
	}
	if o.town == "" {
		fmt.Fprintln(stderr, "invoice: -town is required")
		return 2
	}
	return run(args, os.DirFS(filepath.Join(o.town, ".runtime")), stdout, stderr)
}

// options are the command's flags.
type options struct {
	town        string
	month       string
	workspace   string
	terms       string
	overheadUSD float64
}

// newFlagSet defines the flags on o. run and realMain both call it so a flag
// exists the moment main reads -town.
func newFlagSet(out io.Writer, o *options) *flag.FlagSet {
	fs := flag.NewFlagSet("invoice", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.town, "town", "", "the town root; every source is read under <town>/.runtime")
	fs.StringVar(&o.month, "month", "", "the invoice period, YYYY-MM")
	fs.StringVar(&o.workspace, "workspace", "", "the workspace id to invoice")
	fs.StringVar(&o.terms, "terms", "", "the terms JSON file, a path under <town>/.runtime")
	fs.Float64Var(&o.overheadUSD, "overhead-usd", 0, "the town's fixed monthly overhead in US dollars, split over the month's active workspaces")
	return fs
}
