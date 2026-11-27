package testpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// gateBranchChecks are gate-lint's three presubmit checks (gt-y1z02o), in
// recipe order, and whether a failure fails the target (unwired is advisory,
// as in presubmit).
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
// line, failing only for testwaits and gomodrationale behind the rc guard, or
// "".
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
				line = strings.TrimRight(l, " \t")
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
			if strings.Contains(line, "exit ") {
				return "the advisory " + c.name + " check fails the target"
			}
			continue
		}
		want := `[ $$rc -eq 0 ] || { echo "gate: FAILED at ` + c.name + `" >&2; exit 1; }`
		if !strings.HasSuffix(line, want) {
			return "a failing " + c.name + " does not end with " + want
		}
	}
	return ""
}

// TestGateBranchChecks pins the real Makefile, then checks that each mutation
// of it is rejected (gt-y1z02o): the test is red when either failure exit goes
// unguarded, changes code or is removed, when the advisory check exits, or when
// a seconds line, -base or the prerequisite order goes.
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
		{"testwaits exit unguarded", `[ $$rc -eq 0 ] || { echo "gate: FAILED at testwaits"`, `{ echo "gate: FAILED at testwaits"`},
		{"gomodrationale exit unguarded", `[ $$rc -eq 0 ] || { echo "gate: FAILED at gomodrationale"`, `{ echo "gate: FAILED at gomodrationale"`},
		{"testwaits exit removed", `"gate: FAILED at testwaits" >&2; exit 1; }`, `"gate: FAILED at testwaits" >&2; }`},
		{"gomodrationale exit removed", `"gate: FAILED at gomodrationale" >&2; exit 1; }`, `"gate: FAILED at gomodrationale" >&2; }`},
		{"testwaits exit 10", `"gate: FAILED at testwaits" >&2; exit 1; }`, `"gate: FAILED at testwaits" >&2; exit 10; }`},
		{"unwired fails", `[ $$rc -eq 0 ] || echo "presubmit: WARNING: the unwired export check could not judge the branch; it is advisory and presubmit continues" >&2`, `[ $$rc -eq 0 ] || { echo "gate: FAILED at unwired" >&2; exit 1; }`},
		{"unwired exit 10", `it is advisory and presubmit continues" >&2`, `it is advisory and presubmit continues" >&2; exit 10`},
	} {
		if got := strings.Replace(makefile, tc.from, tc.to, 1); got == makefile {
			t.Errorf("%s: the mutation string is not in the Makefile", tc.name)
		} else if p := gateBranchChecksProblem(got); p == "" {
			t.Errorf("%s: the checker passed a Makefile it must reject", tc.name)
		}
	}
}
