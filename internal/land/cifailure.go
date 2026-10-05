package land

import "strings"

// TestFailure is one Go test a candidate gate's job log tail reported failing,
// and the package it ran in.
type TestFailure struct {
	Package string
	Test    string
}

// CIFailure is one red candidate-gate verdict: the bead whose candidate it
// gated, the failing job's log tail, and the Go tests parsed out of it. Tests
// is empty when the tail named none, which is a failure to record as unparsed
// rather than guess at (gt-xvw20).
type CIFailure struct {
	Bead  string
	Tail  string
	Tests []TestFailure
}

// ParseTestFailures reads a candidate job log tail's go test output: a
// "--- FAIL: TestX" line names a test, and the "FAIL <pkg>" summary that
// closes its package's block names the package those tests ran in. A subtest
// failure is reported for its top-level test, so one test is one key. A tail
// in no shape this reads yields nothing: an unparsed failure never counts
// toward the repeat rule (gt-xvw20).
func ParseTestFailures(tail string) []TestFailure {
	var (
		out     []TestFailure
		pending []string
		seen    = map[TestFailure]bool{}
	)
	for _, line := range strings.Split(tail, "\n") {
		if m := failedTestRE.FindStringSubmatch(line); m != nil {
			pending = append(pending, topLevelTest(m[1]))
			continue
		}
		m := packageLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil || m[1] != "FAIL" {
			continue
		}
		for _, name := range pending {
			tf := TestFailure{Package: m[2], Test: name}
			if !seen[tf] {
				seen[tf] = true
				out = append(out, tf)
			}
		}
		pending = nil
	}
	return out
}

// topLevelTest is a go test name without its subtest path: a subtest failure
// is its parent test's.
func topLevelTest(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}
