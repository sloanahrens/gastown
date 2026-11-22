package daemon

import (
	"fmt"
	"strings"
)

// hmSkewTestOnlyPath reports whether p is out of the binary by its shape only because
// it is a test file or a testdata path (not markdown or docs): the paths a //go:embed
// directive can still carry into the binary (gt-pcixw3).
func hmSkewTestOnlyPath(p string) bool {
	return strings.HasSuffix(p, "_test.go") || strings.Contains("/"+p, "/testdata/")
}

// hmSkewReachesBinary reports whether any of the changed paths can change the hm
// binary: a path of binary shape does, and a test file or testdata path does when a
// //go:embed directive of the hm source at rev (in the clone src) covers it. The
// directives are read only when such a path is among the changes, with the om
// check's reading of them (omSkewEmbedRuleCovers over parseOMSkewEmbedRules). A
// clone that cannot answer, or output the parse cannot read, is an error and so no
// skew: a reading that cannot be taken is a log line.
func (d *Daemon) hmSkewReachesBinary(src, rev string, changed []string) (bool, error) {
	var testOnly []string
	for _, p := range changed {
		switch {
		case p == "":
		case hmSkewBinaryPath(p):
			return true, nil
		case hmSkewTestOnlyPath(p):
			testOnly = append(testOnly, p)
		}
	}
	if len(testOnly) == 0 {
		return false, nil
	}
	out, err := d.omSkewGitNoMatch("-C", src, "grep", "-n", "-F", "-e", "//go:embed", rev, "--", "*.go")
	if err != nil {
		return false, fmt.Errorf("cannot read the hm source's embed directives: %w", err)
	}
	rules, err := parseOMSkewEmbedRules(rev, out)
	if err != nil {
		return false, fmt.Errorf("the hm source's embed directives: %w", err)
	}
	for _, p := range testOnly {
		for _, r := range rules {
			if omSkewEmbedRuleCovers(r, p) {
				return true, nil
			}
		}
	}
	return false, nil
}
