package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// The stages of gate-test's red-on-base check (gt-kelgqa), as the Makefile
// writes them, with each recipe line's continuation backslash removed.
const (
	gateRedbaseErrbranch = `if [ $$eb -ne 0 ]; then echo "gate: FAILED at errbranch" >&2; exit 1; fi;`
	gateRedbaseGuard     = `rb=0; if [ $$go_rc -eq 0 ] && [ $$vet_rc -eq 0 ] && [ $$eb -eq 0 ]; then t0=$$(date +%s); go run ./internal/presubmit/cmd/redbase -base $(PRESUBMIT_BASE) || rb=$$?;`
	gateRedbaseSeconds   = `echo "gate: check redbase $$(( $$(date +%s) - t0 ))s" >&2; fi;`
	gateRedbaseExit      = `if [ $$rb -ne 0 ]; then echo "gate: FAILED at redbase" >&2; exit 1; fi;`
	gateRedbaseRun       = `go run ./internal/presubmit/cmd/redbase -base $(PRESUBMIT_BASE)`
	gateRedbaseVerdict   = `go run ./internal/testpolicy/cmd/gateverdict -unit $$go_rc -vet $$vet_rc -wall $$wall`
	gateVerdictCall      = `go run ./internal/testpolicy/cmd/gateverdict`
)

// gateRedbaseRecipe is a gate-test recipe whose lines are the given stages, in order, joined as
// the Makefile joins them.
func gateRedbaseRecipe(lines ...string) string {
	return "gate-test:\n\t" + strings.Join(lines, " \\\n\t") + "\n"
}

// gateRedbaseProblem returns why makefile's gate-test recipe does not run the red-on-base check
// after the unit tier, the vet and the error-branch check have passed, and before the verdict, or
// "". A failed earlier stage keeps its own verdict and runs no redbase, so the check is gated on
// all three. A redbase failure prints the FAILED line and exits non-zero.
func gateRedbaseProblem(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return "the Makefile has no gate-test recipe"
	}
	sh := strings.Join(strings.Fields(gateContinued.ReplaceAllString(gateComment.ReplaceAllString(m[1], ""), " ")), " ")
	if !strings.Contains(sh, gateRedbaseRun) {
		return "gate-test does not run the red-on-base check"
	}
	guard := strings.Index(sh, gateRedbaseGuard)
	if guard < 0 {
		return "the red-on-base check runs when an earlier stage may have failed"
	}
	if !strings.Contains(sh, gateRedbaseSeconds) {
		return "the red-on-base check prints no gate: check redbase <seconds>s line"
	}
	exit := strings.Index(sh, gateRedbaseExit)
	if exit < 0 {
		return "the red-on-base failure does not exit non-zero with gate: FAILED at redbase"
	}
	errbranch, verdict := strings.Index(sh, gateRedbaseErrbranch), strings.Index(sh, gateVerdictCall)
	if errbranch < 0 || verdict < 0 || errbranch > guard || guard > exit || exit > verdict {
		return "the red-on-base check is not between the error-branch check and the verdict"
	}
	return ""
}

// TestGateRedbase pins the red-on-base check in the gate-test recipe (gt-kelgqa).
func TestGateRedbase(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if p := gateRedbaseProblem(string(data)); p != "" {
		t.Fatalf("gate-test: %s\n%s", p, data)
	}
}

// TestGateRedbaseRejects fails each refusal gateRedbaseProblem makes, each from one changed stage
// of an otherwise passing recipe, and asserts the message that refusal returns.
func TestGateRedbaseRejects(t *testing.T) {
	t.Parallel()
	mutate := func(s, old, repl string) string {
		t.Helper()
		if !strings.Contains(s, old) {
			t.Fatalf("the fixture has no %q to change", old)
		}
		return strings.Replace(s, old, repl, 1)
	}
	runLine := gateRedbaseGuard + " " + gateRedbaseSeconds
	valid := gateRedbaseRecipe(gateRedbaseErrbranch, runLine, gateRedbaseExit, gateRedbaseVerdict)
	const (
		noRecipe  = "the Makefile has no gate-test recipe"
		noRun     = "gate-test does not run the red-on-base check"
		unguarded = "the red-on-base check runs when an earlier stage may have failed"
		noSeconds = "the red-on-base check prints no gate: check redbase <seconds>s line"
		noExit    = "the red-on-base failure does not exit non-zero with gate: FAILED at redbase"
		misplaced = "the red-on-base check is not between the error-branch check and the verdict"
	)
	for _, tc := range []struct{ name, want, makefile string }{
		{"the fixture passes", "", valid},
		{"no recipe", noRecipe, "all:\n\t@true\n"},
		{"no redbase run", noRun, mutate(valid, "cmd/redbase", "cmd/other")},
		{"guard drops the unit tier", unguarded, mutate(valid, "[ $$go_rc -eq 0 ] && ", "")},
		{"guard drops the vet", unguarded, mutate(valid, "[ $$vet_rc -eq 0 ] && ", "")},
		{"guard drops the error-branch check", unguarded, mutate(valid, " && [ $$eb -eq 0 ]", "")},
		{"no seconds line", noSeconds, mutate(valid, gateRedbaseSeconds, "fi;")},
		{"seconds line names another check", noSeconds, mutate(valid, "gate: check redbase", "gate: check rb")},
		{"failure does not exit", noExit, mutate(valid, `at redbase" >&2; exit 1; fi;`, `at redbase" >&2; fi;`)},
		{"failure line names another stage", noExit, mutate(valid, `at redbase" >&2`, `at rb" >&2`)},
		{"no error-branch check before it", misplaced, gateRedbaseRecipe(runLine, gateRedbaseExit, gateRedbaseVerdict)},
		{"verdict before the redbase run", misplaced, gateRedbaseRecipe(gateRedbaseErrbranch, gateRedbaseVerdict, runLine, gateRedbaseExit)},
		{"redbase run before the error-branch check", misplaced, gateRedbaseRecipe(runLine, gateRedbaseErrbranch, gateRedbaseExit, gateRedbaseVerdict)},
		{"failure exit after the verdict", misplaced, gateRedbaseRecipe(gateRedbaseErrbranch, runLine, gateRedbaseVerdict, gateRedbaseExit)},
	} {
		if got := gateRedbaseProblem(tc.makefile); got != tc.want {
			t.Errorf("%s: gateRedbaseProblem = %q, want %q", tc.name, got, tc.want)
		}
	}
}
