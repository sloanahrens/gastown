package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// gateMachinecheck returns why the gate-test recipe does not build gt into a
// temp dir and run machinecheck on it between the nested-module build and the
// unit tier, failing with the FAILED line and removing the dir where it exists, or "" (gt-fmaalx).
func gateMachinecheck(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return "no gate-test recipe"
	}
	r := strings.ReplaceAll(strings.ReplaceAll(m[1], "\\\n", ""), "\n", " ")
	nested, unit := strings.Index(r, "for m in $(NESTED_MODULES)"), strings.Index(r, `echo "gate: unit tier`)
	start, end := strings.Index(r, "gtdir=$$(mktemp"), strings.Index(r, `echo "gate: check machinecheck`)
	if start < 0 || end < start {
		return "no machinecheck line"
	}
	if nested < 0 || unit < 0 || start < nested || start > unit {
		return "the check is not between the nested-module build and the unit tier"
	}
	b := r[start:end]
	build, run := `$(CPU_CLASS) make --no-print-directory build BUILD_DIR="$$gtdir"`, `go run ./cmd/machinecheck -gt "$$gtdir/$(BINARY)"`
	if !strings.Contains(b, build) || !strings.Contains(b, run) || strings.Index(b, build) > strings.Index(b, run) {
		return "the machinecheck line does not build gt into the temp dir, then run the check"
	}
	if !strings.Contains(r[end:], `echo "gate: check machinecheck $$(( $$(date +%s) - start ))s" >&2`) {
		return "the machinecheck line does not print its seconds line to stderr"
	}
	groups := strings.Split(b, "|| {")
	if len(groups) < 4 || strings.Count(b, `rm -rf "$$gtdir"`) < 3 {
		return "the temp dir is not removed on the build failure, the check failure and the passing path"
	}
	for i, body := range groups[1:] {
		body, _, _ = strings.Cut(body, "}")
		if !strings.Contains(body, "gate: FAILED at machinecheck") || !strings.Contains(body, "exit 1") || (i > 0 && !strings.Contains(body, `rm -rf "$$gtdir"`)) {
			return "a refusal does not remove the temp dir and exit non-zero with the FAILED line"
		}
	}
	return ""
}

// TestGateMachinecheck holds the Makefile's gate-test recipe to gateMachinecheck's rule (gt-fmaalx).
func TestGateMachinecheck(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if p := gateMachinecheck(string(data)); p != "" {
		t.Fatalf("gate-test: %s\n%s", p, data)
	}
}
