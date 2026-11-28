package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// Why gateMachinecheck rejects a Makefile. The checker and its table share
// these, so a row can only pass when it reaches that refusal's own message.
const (
	gateNoRecipe      = "no gate-test recipe"
	gateNoLine        = "no machinecheck line"
	gatePlacement     = "the check is not between the nested-module build and the unit tier"
	gateUmask         = "the machinecheck line does not set umask 022"
	gateBuildThenRun  = "the machinecheck line does not build gt into the temp dir, then run the check"
	gateSeconds       = "the machinecheck line does not print its seconds line to stderr"
	gateRefusalCount  = "the machinecheck line does not refuse with the FAILED line after the mktemp, the build and the check"
	gateRefusalRM     = "a refusal does not remove the temp dir"
	gateRefusalFailed = "a refusal does not print the FAILED line"
	gateRefusalExit   = "a refusal does not exit 1 at its closing brace"
	gatePassRM        = "the temp dir is not removed on the passing path"
)

// gateMachinecheckLines drops the recipe's comment lines, joins its backslash
// continuations, and returns one entry per shell line.
func gateMachinecheckLines(recipe string) []string {
	joined := gateContinued.ReplaceAllString(gateComment.ReplaceAllString(recipe, ""), "")
	var lines []string
	for _, l := range strings.Split(joined, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// gateMachinecheck returns why the gate-test recipe does not build gt into a
// temp dir and run machinecheck on it before the unit tier, or "".
// It holds the line to umask 022, to removing the dir on every path, and to
// exiting 1 with the FAILED line at each refusal (gt-fmaalx).
func gateMachinecheck(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return gateNoRecipe
	}
	lines := gateMachinecheckLines(m[1])
	at, nested, unit := -1, -1, -1
	for i, l := range lines {
		if at < 0 && strings.Contains(l, "gtdir=$$(mktemp") {
			at = i
		}
		if nested < 0 && strings.Contains(l, "for m in $(NESTED_MODULES)") {
			nested = i
		}
		if unit < 0 && strings.Contains(l, `echo "gate: unit tier`) {
			unit = i
		}
	}
	if at < 0 {
		return gateNoLine
	}
	if nested < 0 || unit < 0 || at <= nested || at >= unit {
		return gatePlacement
	}
	cmd := strings.TrimPrefix(strings.TrimSpace(lines[at]), "@")
	if !strings.HasPrefix(cmd, "umask 022;") {
		return gateUmask
	}
	build := `$(CPU_CLASS) make --no-print-directory build BUILD_DIR="$$gtdir"`
	check := `go run ./cmd/machinecheck -gt "$$gtdir/$(BINARY)"`
	if !strings.Contains(cmd, build) || !strings.Contains(cmd, check) || strings.Index(cmd, build) > strings.Index(cmd, check) {
		return gateBuildThenRun
	}
	if !strings.Contains(cmd, `echo "gate: check machinecheck $$(( $$(date +%s) - start ))s" >&2`) {
		return gateSeconds
	}
	parts := strings.Split(cmd, "|| {")
	if len(parts) != 4 {
		return gateRefusalCount
	}
	for i, p := range parts[1:] {
		body, _, _ := strings.Cut(p, "}")
		if i > 0 && !strings.Contains(body, `rm -rf "$$gtdir"`) {
			return gateRefusalRM
		}
		if !strings.Contains(body, "gate: FAILED at machinecheck") {
			return gateRefusalFailed
		}
		if !strings.HasSuffix(strings.TrimSpace(body), "exit 1;") {
			return gateRefusalExit
		}
	}
	after := cmd[strings.LastIndex(cmd, "}")+1:]
	rm, secs := strings.Index(after, `rm -rf "$$gtdir"`), strings.Index(after, `echo "gate: check machinecheck`)
	if rm < 0 || secs < 0 || rm > secs {
		return gatePassRM
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

// The machinecheck recipe block and its neighbours, as the Makefile writes them.
const (
	gateBuildLine  = "\t$(CPU_CLASS) make --no-print-directory build BUILD_DIR=\"$$gtdir\" >/dev/null"
	gateCheckLine  = "\tgo run ./cmd/machinecheck -gt \"$$gtdir/$(BINARY)\""
	gateRefusal    = " || { rm -rf \"$$gtdir\"; echo \"gate: FAILED at machinecheck\" >&2; exit 1; }; \\\n"
	gateBlockStart = "\t@umask 022; start=$$(date +%s); gtdir=$$(mktemp"
	gateBlock      = gateBlockStart + " -d) || { echo \"gate: FAILED at machinecheck\" >&2; exit 1; }; \\\n" +
		gateBuildLine + gateRefusal + gateCheckLine + gateRefusal +
		"\trm -rf \"$$gtdir\"; \\\n" +
		"\techo \"gate: check machinecheck $$(( $$(date +%s) - start ))s\" >&2\n"
	gateNested = "\t@umask 022; out=$$(mktemp -d); for m in $(NESTED_MODULES); do $(CPU_CLASS) go -C \"$$m\" build -o \"$$out/\" ./... || { rm -rf \"$$out\"; echo \"gate: FAILED at build ($$m)\" >&2; exit 1; }; done; rm -rf \"$$out\"\n"
	gateNote   = "\t@# The machine-readable stdout check (gt-fmaalx), as presubmit runs it:\n" +
		"\t@# the build's binary lands in a temp dir, so the check never leaves a gt\n" +
		"\t@# in the tree. Plain `make`, not $(MAKE): a line holding $(MAKE) runs\n" +
		"\t@# for real under `make -n`, and a dry run of the gate must start nothing\n" +
		"\t@# (gt-digv4n).\n"
)

// TestGateMachinecheckRejects edits the real Makefile text one way per row and
// asserts the checker reaches that row's own refusal (gt-fmaalx).
func TestGateMachinecheckRejects(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	makefile := string(data)
	for _, tc := range []struct{ name, old, new, want string }{
		{"no gate-test recipe", "gate-test:\n\t@old=$$(umask)", "gate-test-renamed:\n\t@old=$$(umask)", gateNoRecipe},
		{"no machinecheck line", "gtdir=$$(mktemp", "tempdir=$$(mktemp", gateNoLine},
		{"before the nested-module build", gateNested + gateNote + gateBlock, gateBlock + gateNested + gateNote, gatePlacement},
		{"after the unit tier", gateBlockStart, "\t@echo \"gate: unit tier (moved)\" >&2\n" + gateBlockStart, gatePlacement},
		{"no build", gateBuildLine + gateRefusal, "", gateBuildThenRun},
		{"check before build", gateBuildLine + gateRefusal + gateCheckLine + gateRefusal, gateCheckLine + gateRefusal + gateBuildLine + gateRefusal, gateBuildThenRun},
		{"no seconds line", "\techo \"gate: check machinecheck $$(( $$(date +%s) - start ))s\" >&2\n", "\techo done >&2\n", gateSeconds},
		{"build failure keeps the dir", `BUILD_DIR="$$gtdir" >/dev/null || { rm -rf "$$gtdir"; `, `BUILD_DIR="$$gtdir" >/dev/null || { `, gateRefusalRM},
		{"check failure keeps the dir", `-gt "$$gtdir/$(BINARY)" || { rm -rf "$$gtdir"; `, `-gt "$$gtdir/$(BINARY)" || { `, gateRefusalRM},
		{"refusal exits zero", `{ echo "gate: FAILED at machinecheck" >&2; exit 1; }`, `{ echo "gate: FAILED at machinecheck" >&2; exit 0; }`, gateRefusalExit},
		{"refusal exits 10", `{ echo "gate: FAILED at machinecheck" >&2; exit 1; }`, `{ echo "gate: FAILED at machinecheck" >&2; exit 10; }`, gateRefusalExit},
		{"refusal without the FAILED line", `{ echo "gate: FAILED at machinecheck" >&2; exit 1; }`, "{ exit 1; }", gateRefusalFailed},
		{"no build refusal", gateBuildLine + ` || { rm -rf "$$gtdir"; echo "gate: FAILED at machinecheck" >&2; exit 1; }`, gateBuildLine, gateRefusalCount},
		{"passing path keeps the dir", "\trm -rf \"$$gtdir\"; \\\n", "", gatePassRM},
		{"no umask", gateBlockStart, "\t@start=$$(date +%s); gtdir=$$(mktemp", gateUmask},
	} {
		if !strings.Contains(makefile, tc.old) {
			t.Errorf("%s: the Makefile no longer holds %q", tc.name, tc.old)
			continue
		}
		if got := gateMachinecheck(strings.Replace(makefile, tc.old, tc.new, 1)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
