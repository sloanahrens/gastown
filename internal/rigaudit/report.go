package rigaudit

import (
	"fmt"
	"io"
)

// Report prints the audit for in to out and returns the exit code. Each
// problem prints as "<kind padded to 9> <text>", then Verdict's line; the code
// is 1 when the verdict is not ok, else 0. An input with no rigs, or a nil
// Config, prints "RIG AUDIT: FAILED: <reason>" and returns 1 before any check,
// so a town with nothing to audit never reads as clean.
func Report(out io.Writer, in Input) int {
	if len(in.KnownRigs()) == 0 {
		fmt.Fprintln(out, "RIG AUDIT: FAILED: no rigs in the registry")
		return 1
	}
	if in.Config == nil {
		fmt.Fprintln(out, "RIG AUDIT: FAILED: the town config is empty or unreadable")
		return 1
	}
	problems := Problems(in)
	for _, p := range problems {
		fmt.Fprintf(out, "%-9s %s\n", p.Kind, p.Text)
	}
	line, ok := Verdict(problems, in.Unparked, in.Parked)
	fmt.Fprintln(out, line)
	if !ok {
		return 1
	}
	return 0
}
