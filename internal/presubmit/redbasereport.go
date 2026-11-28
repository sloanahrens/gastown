package presubmit

import (
	"fmt"
	"io"
)

// RedBaseReport writes one line per finding, "<file>:<line>: <test> passes without the change", then the count line, and reports ok only when there is no finding (gt-2kl144). A failed write is returned rather than read as a clean report. The count names the plan's tests and the packages holding one, so a run that judged nothing still reads as a count and not as silence.
func RedBaseReport(w io.Writer, plan RedBasePlanResult, findings []RedBaseFinding) (bool, error) {
	n, m := 0, 0
	for _, p := range plan.Packages {
		if len(p.Tests) > 0 {
			n, m = n+len(p.Tests), m+1
		}
	}
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "%s:%d: %s passes without the change\n", f.Test.File, f.Test.Line, f.Test.Name); err != nil {
			return false, err
		}
	}
	_, err := fmt.Fprintf(w, "redbase: %d new or changed test(s) in %d package(s), %d passing without the change\n", n, m, len(findings))
	return err == nil && len(findings) == 0, err
}
