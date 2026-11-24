package done

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// gateListRunner runs one command in the worktree and returns its combined
// output: a test substitutes a fake, so no test runs a real go test (gt-hx8f5i).
type gateListRunner func(dir string, args ...string) ([]byte, error)

// gateListCommand runs one command under env and returns its combined output.
// The real one starts a process; a test substitutes a fake so the runner's
// directory, environment and arguments are read without running go.
type gateListCommand func(dir string, env []string, args ...string) ([]byte, error)

// execGateListCommand is the real gateListCommand.
func execGateListCommand(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// newGateListRunner is the gateListRunner gt done wires. environ is the base
// environment and cgoEnv adds the entries a darwin host needs to compile the
// dolt dependency's icu4c include (AGENTS.md says a bare go test fails
// without them); both are read when the runner runs, so wiring it reads no
// host state (gt-hx8f5i).
func newGateListRunner(environ func() []string, cgoEnv func([]string) []string, run gateListCommand) gateListRunner {
	return func(dir string, args ...string) ([]byte, error) {
		base := environ()
		env := append(append([]string(nil), base...), cgoEnv(base)...)
		return run(dir, env, args...)
	}
}

// checkGateSelects refuses a Gate line whose go test -run pattern selects no
// test on the branch: -run matches test function names, so a pattern naming
// none runs the gate green while it tests nothing (gt-hx8f5i). A list that
// fails, or a runner that is not wired, is a refusal, never a pass.
func checkGateSelects(run gateListRunner, dir, gate string) error {
	for _, gr := range gateRunLines(gate) {
		if run == nil {
			return fmt.Errorf("Gate line %q: no go test runner is configured, so its tests cannot be listed", gr.Line)
		}
		out, err := run(dir, append([]string{"test", "-list", gr.Pattern}, gr.Packages...)...)
		if err != nil {
			return fmt.Errorf("Gate line %q: go test -list did not complete (%v), so its tests cannot be checked:\n%s", gr.Line, err, firstLines(out, 10))
		}
		if !listsATest(out) {
			return fmt.Errorf("Gate line %q selects no test in %s: -run matches test function names; name the tests so the pattern selects them", gr.Line, strings.Join(gr.Packages, " "))
		}
	}
	return nil
}

// testNameRE matches the names go test -list prints: tests, benchmarks, examples, fuzz.
var testNameRE = regexp.MustCompile(`^(Test|Benchmark|Example|Fuzz)[\p{L}\p{N}_]*$`)

// listsATest reports whether go test -list output names a test. The runners
// combine stdout and stderr, so the output also holds go test's own lines and
// anything a package's init prints; only a name-shaped line counts, so that
// noise cannot stand in for a test (gt-hx8f5i).
func listsATest(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if testNameRE.MatchString(strings.TrimSpace(line)) {
			return true
		}
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
