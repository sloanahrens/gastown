package done

import (
	"fmt"
	"os/exec"
	"strings"
)

// gateListCommand runs one command under env and returns its combined output.
// The real one starts a process; a test substitutes a fake so a call's
// directory, environment and arguments are read without starting go.
type gateListCommand func(dir string, env []string, args ...string) ([]byte, error)

// execGateListCommand is the real gateListCommand.
func execGateListCommand(dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// checkGateStaticSelects refuses a Gate line whose -run pattern selects no test, reading the names
// from go list and the parser, so no test binary starts and no TestMain runs (gt-uhqzic). -run
// matches test function names, so a pattern naming none runs the gate green while it tests
// nothing. A lister that is not wired, or fails, is a refusal, never a pass.
func checkGateStaticSelects(list gateTestLister, dir, gate string) error {
	for _, gr := range gateRunLines(gate) {
		if list == nil {
			return fmt.Errorf("Gate line %q: no go test runner is configured, so its tests cannot be listed", gr.Line)
		}
		names, err := list(dir, gr)
		if err != nil {
			return fmt.Errorf("Gate line %q: its tests cannot be listed: %w", gr.Line, err)
		}
		if len(names) == 0 {
			return fmt.Errorf("Gate line %q selects no test in %s: -run matches test function names; name the tests so the pattern selects them", gr.Line, strings.Join(gr.Packages, " "))
		}
	}
	return nil
}

// firstLines returns up to n lines of out, so a caller can carry the start of
// a long output and not all of it.
func firstLines(out []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
