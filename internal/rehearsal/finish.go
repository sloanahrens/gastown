package rehearsal

import (
	"io"
	"strings"
)

// Finish prints the Judge's verdict on r to out, one line reading pass or fail
// and then one line per reason, and returns 0 on a pass and 1 on a fail or a
// write error.
func Finish(r Record, out io.Writer) int {
	v := Judge(r)
	verdict := "fail"
	if v.Pass {
		verdict = "pass"
	}
	lines := append([]string{verdict}, v.Reasons...)
	if _, err := io.WriteString(out, strings.Join(lines, "\n")+"\n"); err != nil {
		return 1
	}
	if v.Pass {
		return 0
	}
	return 1
}
