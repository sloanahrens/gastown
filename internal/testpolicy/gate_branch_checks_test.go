package testpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// gateBranchChecks are gate-lint's three presubmit checks (gt-y1z02o): the
// command each runs and whether a failure fails the target (unwired is
// advisory, as in presubmit).
var gateBranchChecks = []struct {
	name     string
	advisory bool
}{
	{"testwaits", false},
	{"gomodrationale", false},
	{"unwired", true},
}

// gateBranchChecksProblem returns why makefile does not run the three branch
// checks first in gate-lint, each against $(PRESUBMIT_BASE) with a seconds
// line, failing only for testwaits and gomodrationale, or "".
func gateBranchChecksProblem(makefile string) string {
	if !regexp.MustCompile(`(?m)^gate-lint:[ \t]+exec-tax-preflight[ \t]+gate-branch-checks[ \t]+lint[ \t]*$`).MatchString(makefile) {
		return "gate-lint does not run exec-tax-preflight gate-branch-checks lint, in that order"
	}
	m := regexp.MustCompile(`(?m)^gate-branch-checks:\n((?:\t[^\n]*\n|[ \t]*\n)+)`).FindStringSubmatch(makefile)
	if m == nil {
		return "there is no gate-branch-checks recipe"
	}
	for _, c := range gateBranchChecks {
		cmd := "go run ./internal/presubmit/cmd/" + c.name + " -base $(PRESUBMIT_BASE)"
		line := ""
		for _, l := range strings.Split(m[1], "\n") {
			if strings.Contains(l, cmd) {
				line = l
				break
			}
		}
		if line == "" {
			return "the recipe does not run " + cmd
		}
		if !regexp.MustCompile(`gate: check ` + c.name + ` [^"]*date \+%s[^"]*s"`).MatchString(line) {
			return "the " + c.name + " check prints no gate: check <name> <seconds>s line"
		}
		if c.advisory {
			if strings.Contains(line, "exit 1") {
				return "the advisory " + c.name + " check fails the target"
			}
		} else if !strings.Contains(line, "gate: FAILED at "+c.name) || !strings.Contains(line, "exit 1") {
			return "a failing " + c.name + " does not print its FAILED line and exit non-zero"
		}
	}
	return ""
}

// TestGateBranchChecks pins the real Makefile, then checks that each mutation
// of it is rejected (gt-y1z02o): the test is red when either failure exit, a
// seconds line, -base or the prerequisite order goes.
func TestGateBranchChecks(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	makefile := string(data)
	if p := gateBranchChecksProblem(makefile); p != "" {
		t.Fatalf("gate-lint's branch checks: %s", p)
	}
	for _, tc := range []struct{ name, from, to string }{
		{"no prerequisite", "exec-tax-preflight gate-branch-checks lint", "exec-tax-preflight lint"},
		{"wrong order", "exec-tax-preflight gate-branch-checks lint", "gate-branch-checks exec-tax-preflight lint"},
		{"no recipe", "gate-branch-checks:", "gate-branch-lint:"},
		{"no -base", "cmd/testwaits -base $(PRESUBMIT_BASE)", "cmd/testwaits"},
		{"no seconds", `echo "gate: check testwaits $$(( $$(date +%s) - $$start ))s" >&2; `, ""},
		{"testwaits exit removed", `"gate: FAILED at testwaits" >&2; exit 1; `, ""},
		{"gomodrationale exit removed", `"gate: FAILED at gomodrationale" >&2; exit 1; `, ""},
		{"unwired fails", `[ $$rc -eq 0 ] || echo "presubmit: WARNING: the unwired export check could not judge the branch; it is advisory and presubmit continues" >&2`, `[ $$rc -eq 0 ] || { echo "gate: FAILED at unwired" >&2; exit 1; }`},
	} {
		if got := strings.Replace(makefile, tc.from, tc.to, 1); got == makefile {
			t.Errorf("%s: the mutation string is not in the Makefile", tc.name)
		} else if p := gateBranchChecksProblem(got); p == "" {
			t.Errorf("%s: the checker passed a Makefile it must reject", tc.name)
		}
	}
}
