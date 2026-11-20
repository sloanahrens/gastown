package presubmit

import (
	"go/build/constraint"
	"strings"
)

// IntegrationTagged reports whether src is Go source that only compiles with
// the integration build tag set (gt-x8h50e).
//
// The integration vet compiles tagged tests without running them, so a check
// that lists those files must not count a file that builds without the tag
// (gt-9p2wh7). A //go:build line that does not parse counts as tagged, so the
// list errs toward listing a file.
func IntegrationTagged(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "package ") {
			return false
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return true
		}
		return !expr.Eval(func(tag string) bool { return tag != "integration" }) &&
			expr.Eval(func(string) bool { return true })
	}
	return false
}
