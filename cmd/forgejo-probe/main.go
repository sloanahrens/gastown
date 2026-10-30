// Command forgejo-probe proves a rig's gate runs green in the real Forgejo
// runner before the rig is cut over (gt-tpg0p.85). Its exit codes are in -h.
package main

import (
	"context"
	"os"

	"github.com/steveyegge/gastown/internal/forgejoprobe"
)

func main() {
	os.Exit(forgejoprobe.Run(context.Background(), os.Args[1:], os.Getenv,
		forgejoprobe.Streams{Stdout: os.Stdout, Stderr: os.Stderr},
		forgejoprobe.SystemDeps()))
}
