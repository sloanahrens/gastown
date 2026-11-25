// Command gateverdict prints the verdict of the gate's unit tier and integration vet, and exits
// with the gate's status: `make gate`'s last line is its output (gt-w3aby).
//
//	go run ./internal/testpolicy/cmd/gateverdict -unit 0 -vet 0 -wall 42
package main

import (
	"flag"
	"fmt"
	"os"
)

// verdict returns the gate's last line, a note for stderr when the wall is negative, and the
// gate's status for the two stage results. A negative wall means the clock was read earlier at
// the end of the run than at the start, so it prints as 0 with the note instead of breaking the
// line's format (gt-5qfrbu). The unit tier is checked first, the order the gate recipe reports
// its stages in: a red unit tier is named even when the vet also failed.
func verdict(unitRC, vetRC, wall int) (line, note string, code int) {
	if wall < 0 {
		note = "gate: the clock stepped back during the run; the wall reads 0s"
		wall = 0
	}
	switch {
	case unitRC != 0:
		return fmt.Sprintf("gate: FAILED at unit tier (Go suite, exit %d) after %ds wall", unitRC, wall), note, 1
	case vetRC != 0:
		return fmt.Sprintf("gate: FAILED at integration vet (exit %d) after %ds wall", vetRC, wall), note, 1
	}
	return fmt.Sprintf("gate: PASSED in %ds wall", wall), note, 0
}

func main() {
	unit := flag.Int("unit", 0, "exit status of the unit tier")
	vet := flag.Int("vet", 0, "exit status of the integration vet")
	wall := flag.Int("wall", 0, "wall seconds the gate has run")
	flag.Parse()
	line, note, code := verdict(*unit, *vet, *wall)
	// Before changing the order here, read "The gate" in docs/testing.md: the verdict must stay
	// the gate's last line.
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	fmt.Fprintln(os.Stderr, line)
	os.Exit(code)
}
