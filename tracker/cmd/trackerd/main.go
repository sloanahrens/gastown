// Command trackerd is the town tracker service: an HTTP API over Postgres on
// a loopback address.
//
//	TRACKER_DSN=... trackerd -migrate                         bring the schema up to date and exit
//	TRACKER_DSN=... trackerd -issue-token ACTOR -token-file F  create a token, write it to F (0600), exit
//	TRACKER_DSN=... trackerd [-listen 127.0.0.1:7433]         serve
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
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "trackerd:", err)
		os.Exit(1)
	}
}
