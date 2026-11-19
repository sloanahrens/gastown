package testpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// gateQuotaCommand is the command the gate unit tier's width comes from (gt-g9fnsa).
const gateQuotaCommand = "go run ./internal/testpolicy/cmd/cpuquota"

var (
	gateQuotaAssignment = regexp.MustCompile(`(\w+)=\$\$?\(` + regexp.QuoteMeta(gateQuotaCommand) + `\)`)
	gateLiteralWidth    = regexp.MustCompile(`-p[= ]+[0-9]+|GOMAXPROCS=[0-9]+`)
)

// gateQuotaProblem returns why makefile's gate-test recipe does not read the CPU quota once and take both GOMAXPROCS and -p from it, or "".
func gateQuotaProblem(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return "the Makefile has no gate-test recipe"
	}
	shell := gateQuoted.ReplaceAllString(gateContinued.ReplaceAllString(gateComment.ReplaceAllString(m[1], ""), ""), "")
	found := gateQuotaAssignment.FindAllStringSubmatchIndex(shell, -1)
	if len(found) != 1 {
		return "the recipe does not read the CPU quota into a variable exactly once"
	}
	q := shell[found[0][2]:found[0][3]]
	if w := gateLiteralWidth.FindString(shell); w != "" {
		return "the recipe carries a literal width (" + w + ")"
	}
	if strings.Count(shell, q+"=") != 1 {
		return "the recipe assigns the quota variable more than once"
	}
	d, cmd := `\$\$?`, regexp.QuoteMeta(gateQuotaCommand)
	unit := regexp.MustCompile(`\b` + q + `=` + d + `\(` + cmd + `\)[^\n]*GOMAXPROCS=` + d + q + `[^\n]*go test -p ` + d + q)
	if !unit.MatchString(shell) {
		return "the gate-test recipe does not take both GOMAXPROCS and -p from one cpuquota read"
	}
	return ""
}

// TestGateRecipe pins one cpuquota read whose output is the unit tier's -p
// width and GOMAXPROCS (gt-4fkvjg).
func TestGateRecipe(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no path to the Makefile")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if p := gateQuotaProblem(string(data)); p != "" {
		t.Fatalf("gate-test: %s\n%s", p, data)
	}
}

// TestGateRecipeRejectsALiteral fails each refusal gateQuotaProblem makes.
func TestGateRecipeRejectsALiteral(t *testing.T) {
	t.Parallel()
	const quota = "q=$$(go run ./internal/testpolicy/cmd/cpuquota)"
	for _, tc := range []struct{ name, makefile string }{
		{"literal -p 16", "gate-test:\n\t@" + quota + "; GOMAXPROCS=$$q go test -p 16 ./...\n"},
		{"no quota read", "gate-test:\n\t@GOMAXPROCS=$$q go test -p $$q ./...\n"},
		{"q reassigned", "gate-test:\n\t@" + quota + "; q=4; GOMAXPROCS=$$q go test -p $$q ./...\n"},
		{"GOMAXPROCS not from q", "gate-test:\n\t@" + quota + "; GOMAXPROCS=$$n go test -p $$q ./...\n"},
		{"quota after the go test", "gate-test:\n\t@GOMAXPROCS=$$q go test -p $$q ./...; " + quota + "\n"},
		{"no recipe", "all:\n\t@true\n"},
	} {
		if p := gateQuotaProblem(tc.makefile); p == "" {
			t.Errorf("%s: the rule passed a Makefile it must fail", tc.name)
		}
	}
}
