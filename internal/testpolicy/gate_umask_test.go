package testpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/modroot"
)

// gateRecipe matches the gate-test target's recipe; it ends at the first
// non-blank line that does not start with a tab, so a blank line stays inside
// it. gateGoCommand finds a go command; gateQuoted, gateComment and
// gateContinued strip messages, the trap body, comments and a backslash
// continuation.
var (
	gateRecipe    = regexp.MustCompile(`(?m)^gate-test:\n((?:\t[^\n]*\n|[ \t]*\n)+)`)
	gateGoCommand = regexp.MustCompile(`\bgo\s`)
	gateQuoted    = regexp.MustCompile(`'[^']*'|"[^"]*"`)
	gateComment   = regexp.MustCompile(`(?m)^\t@#.*\n`)
	gateContinued = regexp.MustCompile(`\\\n\t?`)
)

// gateUmaskProblem returns why makefile's gate-test recipe breaks the umask rule, or "".
//
// Two cases are deliberately not covered. A recipe line that writes a file
// without a go command ahead of its umask is not checked: the rule looks for a
// go command ahead of umask 022, so a line with no go command passes. The
// gate-lint stage (lint and exec-tax-preflight) runs outside the umask and is
// not checked here.
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
	data, err := os.ReadFile(filepath.Join(modroot.Root(t), "Makefile"))
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
		{"blank line inside the recipe", "gate-test:\n\t@old=$$(umask); echo \"gate: umask was $$old, running under 0022\"\n\t@umask 022; go build ./...\n\n\t@go test ./...; umask 022\n"},
		{"no go command", "gate-test:\n\t@old=$$(umask); umask 022; echo \"gate: umask was $$old, running under 0022\"\n\t@echo done\n"},
		{"no recipe", "all:\n\t@true\n"},
	} {
		if p := gateUmaskProblem(tc.makefile); p == "" {
			t.Errorf("%s: the rule passed a Makefile it must fail", tc.name)
		}
	}
}
