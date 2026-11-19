package testpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// gateRecipe matches the gate-test target's recipe. gateGoCommand finds a go
// command; gateQuoted, gateComment and gateContinued strip messages, the trap
// body, comments and a backslash continuation.
var (
	gateRecipe    = regexp.MustCompile(`(?m)^gate-test:\n((?:\t[^\n]*\n)+)`)
	gateGoCommand = regexp.MustCompile(`\bgo\s`)
	gateQuoted    = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	gateComment   = regexp.MustCompile(`(?m)^\t@#.*\n`)
	gateContinued = regexp.MustCompile(`\\\n\t?`)
)

// gateUmaskProblem returns why makefile's gate-test recipe breaks the umask rule, or "".
func gateUmaskProblem(makefile string) string {
	m := gateRecipe.FindStringSubmatch(makefile)
	if m == nil {
		return "the Makefile has no gate-test recipe"
	}
	recipe := m[1]
	if !strings.Contains(recipe, "gate: umask was") || !strings.Contains(recipe, "running under 0022") {
		return "the recipe does not print the umask it replaced"
	}
	shell := gateQuoted.ReplaceAllString(gateComment.ReplaceAllString(recipe, ""), "")
	if !gateGoCommand.MatchString(shell) {
		return "the recipe has no go command"
	}
	for _, line := range strings.Split(gateContinued.ReplaceAllString(shell, " "), "\n") {
		cmd := strings.TrimPrefix(strings.TrimSpace(line), "@")
		loc, u := gateGoCommand.FindStringIndex(cmd), strings.Index(cmd, "umask 022")
		if loc != nil && (u < 0 || u > loc[0]) {
			return "a go command runs before umask 022 in its shell line: " + cmd
		}
	}
	return ""
}

// TestGateUmask holds the Makefile's gate-test recipe to gateUmaskProblem's rule (gt-nmd2xg).
func TestGateUmask(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no path to the Makefile")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	if p := gateUmaskProblem(string(data)); p != "" {
		t.Fatalf("gate-test: %s\n%s", p, data)
	}
}

// TestGateUmaskRejects fails every refusal: a umask on a separate shell line,
// after a go command, absent, unprinted, with no go command, or no recipe.
func TestGateUmaskRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, makefile string }{
		{"umask on a line of its own", "gate-test:\n\t@umask 022\n\t@old=$$(umask); echo \"gate: umask was $$old, running under 0022\"\n\t@go build ./...\n"},
		{"umask after the go command", "gate-test:\n\t@old=$$(umask); echo \"gate: umask was $$old, running under 0022\"\n\t@go build ./...; umask 022\n"},
		{"no umask", "gate-test:\n\t@echo \"gate: umask was 0022, running under 0022\"\n\t@go test ./...\n"},
		{"umask not printed", "gate-test:\n\t@umask 022; go test ./...\n"},
		{"no go command", "gate-test:\n\t@old=$$(umask); umask 022; echo \"gate: umask was $$old, running under 0022\"\n\t@echo done\n"},
		{"no recipe", "all:\n\t@true\n"},
	} {
		if p := gateUmaskProblem(tc.makefile); p == "" {
			t.Errorf("%s: the rule passed a Makefile it must fail", tc.name)
		}
	}
}
