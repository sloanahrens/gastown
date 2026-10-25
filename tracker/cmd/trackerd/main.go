// Command trackerd is the town tracker service. It does not serve yet: the
// store (C3, C4) and server (C5) packages land first.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "trackerd: not implemented yet (see docs/plans/2026-10-25-town-tracker-plan.md, tasks C3-C5)")
	os.Exit(1)
}
