// Command polecat-image keeps gastown-polecat:dev at the installed gt's commit,
// publishes it when a registry is named, and trims what runs accumulate
// (gt-tpg0p.5 through gt-tpg0p.7). plugin.md beside this program is the
// contract; run.sh beside it is the exec shim.
//
// It is also the trim helper a cache volume's short-lived container runs, which
// is why the image ships this binary.
package main

import (
	"os"

	"github.com/steveyegge/gastown/internal/polecatimage"
)

func main() {
	os.Exit(polecatimage.Main(os.Args[1:]))
}
