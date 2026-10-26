// Command trackerbackup writes one nightly pg_dump of the tracker database and
// keeps the newest dumps, so the town's issue data is always restorable.
//
//	TRACKER_DSN=... trackerbackup [-dir DIR] [-keep 7] [-pg-bin DIR]
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
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "trackerbackup:", err)
		os.Exit(1)
	}
}
