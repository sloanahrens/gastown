package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// gateErrbranchProblem returns why makefile's gate-test recipe does not write
// the unit tier's cover profile and judge it for added error branches after a
// passing unit tier and integration vet, in that order, or "" (gt-2jcpvh). The
// cover file is made before the stream file, so a failed cover file leaks
// nothing, and the unit tier carries no -count flag, which would turn go test's
// result cache off.
func gateErrbranchProblem(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return "the Makefile has no gate-test recipe"
	}
	sh := strings.Join(strings.Fields(gateContinued.ReplaceAllString(gateComment.ReplaceAllString(m[1], ""), " ")), " ")
	pos := 0
	for _, want := range []string{
		`[ -n "$$cover" ] && rm -f "$$cover"`,
		`cover=$$(mktemp) || { echo "gate: FAILED at cover profile" >&2; exit 1; }`,
		`stream=$$(mktemp)`,
		`go test -p $$q -timeout 20m ./... -ldflags='-s -w' -coverprofile="$$cover"`,
		`wait $$vet; vet_rc=$$?`,
		`if [ $$go_rc -eq 0 ] && [ $$vet_rc -eq 0 ]; then t0=$$(date +%s); go run ./internal/presubmit/cmd/errbranch -base $(PRESUBMIT_BASE) -cover "$$cover" || eb=$$?`,
		`echo "gate: check errbranch`,
		`rm -f "$$cover"; if [ $$eb -ne 0 ]; then echo "gate: FAILED at errbranch" >&2; exit 1; fi`,
		`cmd/gateverdict`,
	} {
		j := strings.Index(sh[pos:], want)
		if j < 0 {
			return "the gate-test recipe lacks, or misorders: " + want
		}
		pos += j + len(want)
	}
	if strings.Contains(sh, "-count=") {
		return "the unit tier carries -count, which turns go test's result cache off"
	}
	return ""
}

// TestGateErrbranch pins the gate's own error-branch check and fails each refusal.
func TestGateErrbranch(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if p := gateErrbranchProblem(string(data)); p != "" {
		t.Fatalf("gate-test: %s\n%s", p, data)
	}
	fixture := "gate-test:\n\t@[ -n \"$$cover\" ] && rm -f \"$$cover\"; cover=$$(mktemp) || { echo \"gate: FAILED at cover profile\" >&2; exit 1; }; stream=$$(mktemp); go test -p $$q -timeout 20m ./... -ldflags='-s -w' -coverprofile=\"$$cover\" -json; wait $$vet; vet_rc=$$?; if [ $$go_rc -eq 0 ] && [ $$vet_rc -eq 0 ]; then t0=$$(date +%s); go run ./internal/presubmit/cmd/errbranch -base $(PRESUBMIT_BASE) -cover \"$$cover\" || eb=$$?; echo \"gate: check errbranch $$(date +%s)s\" >&2; rm -f \"$$cover\"; if [ $$eb -ne 0 ]; then echo \"gate: FAILED at errbranch\" >&2; exit 1; fi; fi; go run ./internal/testpolicy/cmd/gateverdict -unit $$go_rc -vet $$vet_rc -wall $$wall\n"
	for name, edit := range map[string]func(string) string{
		"no recipe":        func(string) string { return "all:\n\t@true\n" },
		"no cover profile": func(s string) string { return strings.Replace(s, ` -coverprofile="$$cover"`, "", 1) },
		"cache disabled": func(s string) string {
			return strings.Replace(s, `-coverprofile="$$cover"`, `-coverprofile="$$cover" -count=1`, 1)
		},
		"stream leaks": func(s string) string {
			return strings.Replace(s,
				`cover=$$(mktemp) || { echo "gate: FAILED at cover profile" >&2; exit 1; }; stream=$$(mktemp); `,
				`stream=$$(mktemp); cover=$$(mktemp) || { echo "gate: FAILED at cover profile" >&2; exit 1; }; `, 1)
		},
		"no cover failure line":  func(s string) string { return strings.Replace(s, "at cover profile", "at cvr", 1) },
		"trap keeps the profile": func(s string) string { return strings.Replace(s, `&& rm -f "$$cover"`, "&& true", 1) },
		"no errbranch":           func(s string) string { return strings.Replace(s, "cmd/errbranch", "cmd/other", 1) },
		"no vet wait":            func(s string) string { return strings.Replace(s, "wait $$vet; vet_rc=$$?; ", "", 1) },
		"not gated":              func(s string) string { return strings.Replace(s, "&& [ $$vet_rc -eq 0 ]", "", 1) },
		"no seconds line":        func(s string) string { return strings.Replace(s, "check errbranch", "errbranch", 1) },
		"failure does not exit":  func(s string) string { return strings.Replace(s, "; exit 1; fi", "; fi", 1) },
		"no verdict":             func(s string) string { return strings.Replace(s, "cmd/gateverdict", "cmd/other", 1) },
		"profile not removed":    func(s string) string { return strings.Replace(s, `rm -f "$$cover"; if`, "if", 1) },
	} {
		if p := gateErrbranchProblem(edit(fixture)); p == "" {
			t.Errorf("%s: the rule passed a Makefile it must fail", name)
		}
	}
}
