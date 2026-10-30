// Command forgejo-probe proves a rig's gate runs green in the real Forgejo
// runner before the rig is cut over (gt-tpg0p.85). Its exit codes are in -h.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
)

func main() {
	// A SIGINT or SIGTERM cancels the run rather than killing the process, so the
	// run's exit still deletes the probe branch, the worktree and any stale record.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	rc := forgejoprobe.Run(ctx, os.Args[1:], os.Getenv,
		forgejoprobe.Streams{Stdout: os.Stdout, Stderr: os.Stderr},
		forgejoprobe.SystemDeps())
	stop()
	os.Exit(rc)
}
