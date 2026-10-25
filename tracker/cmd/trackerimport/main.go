// Command trackerimport copies one rig's Dolt database into the tracker and
// proves the copy: it reads Dolt (read-only), writes Postgres in one
// transaction, then compares the two.
//
//	DOLT_DSN=user:pw@tcp(host:port)/rigdb TRACKER_DSN=postgres://... \
//	  trackerimport [-dry-run] [-rig-name gt=gastown]...
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
		fmt.Fprintln(os.Stderr, "trackerimport:", err)
		os.Exit(1)
	}
}
