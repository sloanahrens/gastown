package rigaudit

import (
	"fmt"
	"io"
)

// Town audits the town at townRoot and returns the exit code: it builds the
// input with Load, prints Report's audit to out and returns its code. A load
// error prints "RIG AUDIT: FAILED: <reason>" and returns 1, so a town whose
// config cannot be read never reads as clean.
func Town(townRoot string, state func(rig string) string, out io.Writer) int {
	in, err := Load(townRoot, state)
	if err != nil {
		fmt.Fprintf(out, "RIG AUDIT: FAILED: %v\n", err)
		return 1
	}
	return Report(out, in)
}
