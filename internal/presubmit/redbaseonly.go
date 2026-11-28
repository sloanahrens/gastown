package presubmit

import (
	"fmt"
	"strings"
)

// RedBaseOnlyTestsAndDocs reports whether base...HEAD changes at least one file and every changed file is a Go test file or a Markdown document, so the red-on-base check can pass a change no test can judge (gt-nghgls). An empty base or a failed diff is an error, so a change it could not read never reads as one touching only tests and docs.
func RedBaseOnlyTestsAndDocs(run Runner, base string) (bool, error) {
	if base == "" {
		return false, fmt.Errorf("red-base only tests and docs: empty base")
	}
	revs := base + "...HEAD"
	out, err := run([]string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--name-only", "--no-renames", "-z", "--end-of-options", revs})
	if err != nil {
		return false, fmt.Errorf("git diff --name-only %s: %w", revs, err)
	}
	changed := 0
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "_test.go") && !strings.HasSuffix(p, ".md") {
			return false, nil
		}
		changed++
	}
	return changed > 0, nil
}
