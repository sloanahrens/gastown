// Command trackersize writes one nightly report of the tracker's size per
// issue-id prefix: the issue rows and the bytes of their text columns.
//
// A run is given one tracker database's DSN and the workspace it belongs to, and
// writes one JSON object per prefix to <out>/<UTC date>.jsonl. The tracker is a
// centralized Postgres, so neither the address nor the workspace has a default
// or comes from the environment.
//
// Nothing runs it yet: its caller is a later bead that runs one report per
// workspace beside the nightly pg_dump job (docs/tracker-restore.md).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "trackersize:", err)
		os.Exit(1)
	}
}
