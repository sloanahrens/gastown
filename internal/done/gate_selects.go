package done

import (
	"fmt"
	"os/exec"
	"strings"
)

// gateListRunner runs one command in the worktree and returns its combined
// output. It is a seam: a unit test substitutes a fake, so no test runs a real
// go test (gt-hx8f5i).
type gateListRunner func(dir string, args ...string) ([]byte, error)

// runGoTestList is the real gateListRunner: go test with the arguments the
// Gate line's pattern and packages give.
func runGoTestList(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// checkGateSelects refuses a Gate line whose go test -run pattern selects no
// test on the branch: -run matches test function names, so a pattern naming
// none runs the gate green while it tests nothing (gt-hx8f5i). A list that
// fails to build is a refusal carrying the first lines of its output, never a
// pass.
func checkGateSelects(run gateListRunner, dir, gate string) error {
	if run == nil {
		run = runGoTestList
	}
	for _, gr := range gateRunLines(gate) {
		out, err := run(dir, append([]string{"test", "-list", gr.Pattern}, gr.Packages...)...)
		if err != nil {
			return fmt.Errorf("Gate line %q: go test -list could not build the test binary:\n%s", gr.Line, firstLines(out, 10))
		}
		if !listsATest(out) {
			return fmt.Errorf("Gate line %q selects no test in %s: -run matches test function names; name the tests so the pattern selects them", gr.Line, strings.Join(gr.Packages, " "))
		}
	}
	return nil
}

// listsATest reports whether go test -list output names a test. The only
// lines that are not names are a package's result line ("ok pkg 0.002s") and
// its no-test-files line ("? pkg [no test files]").
func listsATest(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "ok" || fields[0] == "?" {
			continue
		}
		return true
	}
	return false
}

// firstLines returns up to n lines of out, so a refusal carries the start of
// a failed list's output and not the whole build log.
func firstLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
