// Command gateverdict prints the verdict of the gate's unit tier and integration vet, and exits
// with the gate's status. The Makefile's gate recipe calls it once both stages have finished, so
// the branches that decide the verdict are tested here rather than in the recipe (gt-w3aby).
//
//	go run ./internal/testpolicy/cmd/gateverdict -unit 0 -vet 0 -wall 42
package main

import (
	"flag"
	"fmt"
	"os"
)

// verdict returns the gate's last line and its status for the two stage results. The unit tier is
// checked first, as it was before the integration vet joined the gate: a red unit tier is named
// even when the vet also failed.
func verdict(unitRC, vetRC, wall int) (string, int) {
	switch {
	case unitRC != 0:
		return fmt.Sprintf("gate: FAILED at unit tier (Go suite, exit %d) after %ds wall", unitRC, wall), 1
	case vetRC != 0:
		return fmt.Sprintf("gate: FAILED at integration vet (exit %d) after %ds wall", vetRC, wall), 1
	}
	return fmt.Sprintf("gate: PASSED in %ds wall", wall), 0
}

func main() {
	unit := flag.Int("unit", 0, "exit status of the unit tier")
	vet := flag.Int("vet", 0, "exit status of the integration vet")
	wall := flag.Int("wall", 0, "wall seconds the gate has run")
	flag.Parse()
	line, code := verdict(*unit, *vet, *wall)
	fmt.Fprintln(os.Stderr, line)
	os.Exit(code)
}
