//go:build integration

package makegate

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// mixedMakeTargets returns, per Makefile target, the line number (1-based) of
// its first logical recipe line that holds $(MAKE) or ${MAKE} beside another
// command: a `;`, `&&`, `||` or `|` outside the call (gt-33u7jn). GNU make runs
// such a line even under `make -n` (gt-digv4n), so a dry run of the target is
// not a dry run of what it recurses into. A line that is only `@$(MAKE) args`
// or `$(MAKE) args` is not mixed.
//
// A logical line is joined from the physical lines it continues through: a
// trailing backslash removes the newline, as the shell removes it from a recipe
// line. A tab-indented logical line is a recipe line of the nearest rule line
// above it, found as a line starting in column one whose first colon is not
// part of an assignment; a conditional such as `ifeq` between two recipe lines
// leaves that rule line the nearest one, as it does for make.
func mixedMakeTargets(makefile string) map[string]int {
	mixed := map[string]int{}
	var targets []string
	for _, line := range mixedMakeLogicalLines(makefile) {
		if !strings.HasPrefix(line.text, "\t") {
			if names := mixedMakeRuleTargets(line.text); names != nil {
				targets = names
			}
			continue
		}
		if !mixedMakeLine(line.text) {
			continue
		}
		for _, target := range targets {
			if _, seen := mixed[target]; !seen {
				mixed[target] = line.number
			}
		}
	}
	return mixed
}

// mixedMakeLogical is one Makefile logical line: its text, and the 1-based
// number of the first physical line it was built from.
type mixedMakeLogical struct {
	text   string
	number int
}

// mixedMakeLogicalLines returns makefile's logical lines, each built from one
// physical line and every line it continues into through a trailing backslash.
func mixedMakeLogicalLines(makefile string) []mixedMakeLogical {
	physical := strings.Split(makefile, "\n")
	var lines []mixedMakeLogical
	for i := 0; i < len(physical); i++ {
		first := i
		text := physical[i]
		for strings.HasSuffix(text, "\\") && i+1 < len(physical) {
			text = strings.TrimSuffix(text, "\\") + physical[i+1]
			i++
		}
		lines = append(lines, mixedMakeLogical{text: text, number: first + 1})
	}
	return lines
}

// mixedMakeRuleTargets returns the target names of a Makefile rule line, or nil
// when line is not one. A rule line starts in column one; a comment, a
// space-indented line and an assignment are not rule lines. An assignment is a
// first colon that `:=` or `::=` follows, or an `=` before the first colon
// (`NAME = value`, which holds a colon only when value is a URL).
func mixedMakeRuleTargets(line string) []string {
	if line == "" || line[0] == ' ' || line[0] == '#' {
		return nil
	}
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return nil
	}
	if eq := strings.IndexByte(line, '='); eq >= 0 && eq < colon {
		return nil
	}
	if rest := line[colon:]; strings.HasPrefix(rest, ":=") || strings.HasPrefix(rest, "::=") {
		return nil
	}
	return strings.Fields(line[:colon])
}

// mixedMakeLine reports whether a recipe line runs $(MAKE) or ${MAKE} beside
// another command, meaning a `;`, `&&`, `||` or `|` outside the call. The call
// is the token itself: make takes the recipe arguments from outside it.
func mixedMakeLine(line string) bool {
	if !strings.Contains(line, "$(MAKE)") && !strings.Contains(line, "${MAKE}") {
		return false
	}
	rest := strings.NewReplacer("$(MAKE)", "", "${MAKE}", "").Replace(line)
	return strings.ContainsAny(rest, ";|") || strings.Contains(rest, "&&")
}

// TestIntegrationMixedMakeReader checks the reader the guarded-list comparison
// (gt-jiwfzd) is built on. Mutation: drop the separator check from
// mixedMakeLine, so a lone `@$(MAKE) args` line counts as mixed, and both the
// snippet and checkout cases fail, the checkout one naming polecat-image,
// check-forward-only and install-local.
func TestIntegrationMixedMakeReader(t *testing.T) {
	t.Run("source snippets", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			makefile string
			want     map[string]int
		}{
			{
				// The continued line holds the separator and the line it
				// continues into holds $(MAKE): only the joined text is mixed.
				name: "a continued line mixes across the break",
				makefile: "one:\n" +
					"\t@echo start; \\\n" +
					"\t$(MAKE) x\n" +
					"two:\n" +
					"\t@$(MAKE) a && b\n" +
					"\t@$(MAKE) a\n" +
					"\t$(MAKE) a; b\n",
				want: map[string]int{"one": 2, "two": 5},
			},
			{
				name:     "a lone $(MAKE) line is not mixed",
				makefile: "one:\n\t@$(MAKE) --no-print-directory build\n",
				want:     map[string]int{},
			},
			{
				name:     "a ${MAKE} call beside a command is mixed",
				makefile: "one:\n\t${MAKE} a; b\n",
				want:     map[string]int{"one": 2},
			},
			{
				name:     "a pipe beside the call is mixed",
				makefile: "one:\n\t@$(MAKE) a | tee log\n",
				want:     map[string]int{"one": 2},
			},
			{
				name:     "the first mixed line is the one recorded",
				makefile: "one:\n\t@$(MAKE) a\n\t$(MAKE) b; c\n",
				want:     map[string]int{"one": 3},
			},
			{
				name:     "the first rule block decides the line",
				makefile: "one:\n\t$(MAKE) a; b\none:\n\t$(MAKE) c; d\n",
				want:     map[string]int{"one": 2},
			},
			{
				name:     "an || beside the call is mixed",
				makefile: "one:\n\t@$(MAKE) a || exit 1\n",
				want:     map[string]int{"one": 2},
			},
			{
				name:     "a line without $(MAKE) is not mixed",
				makefile: "one:\n\techo done; exit 0\n",
				want:     map[string]int{},
			},
			{
				name:     "prerequisites are not part of the target name",
				makefile: "presubmit: lint\n\t$(MAKE) a; b\n",
				want:     map[string]int{"presubmit": 2},
			},
			{
				// A comment above a recipe line, as the Makefile writes the
				// one-recipe-line rule for ci-base-image.
				name:     "a comment line holding a colon keeps the target",
				makefile: "one:\n# note: x\n\t$(MAKE) a; b\n",
				want:     map[string]int{"one": 3},
			},
			{
				name:     "a space-indented line holding a colon is not a rule line",
				makefile: "one:\n  note: x\n\t$(MAKE) a; b\n",
				want:     map[string]int{"one": 3},
			},
			{
				name:     "a blank line keeps the target",
				makefile: "one:\n\n\t$(MAKE) a; b\n",
				want:     map[string]int{"one": 3},
			},
			{
				name:     "a line with no colon is not a rule line",
				makefile: "VAR ?= x\n\t$(MAKE) a; b\n",
				want:     map[string]int{},
			},
			{
				name:     "an assignment holding a colon is not a rule line",
				makefile: "URL = https://example.com\n\t$(MAKE) a; b\n",
				want:     map[string]int{},
			},
			{
				name:     "a := assignment is not a rule line",
				makefile: "X := y\n\t$(MAKE) a; b\n",
				want:     map[string]int{},
			},
			{
				name:     "a ::= assignment is not a rule line",
				makefile: "X ::= y\n\t$(MAKE) a; b\n",
				want:     map[string]int{},
			},
			{
				name:     "a recipe line with no rule line above it is not recorded",
				makefile: "\t$(MAKE) a; b\n",
				want:     map[string]int{},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := mixedMakeTargets(tc.makefile)
				if !maps.Equal(got, tc.want) {
					t.Fatalf("mixedMakeTargets(%q) = %v, want %v", tc.makefile, got, tc.want)
				}
			})
		}
	})

	t.Run("the checkout Makefile holds exactly the two mixed targets", func(t *testing.T) {
		// Every other $(MAKE) recipe line in the Makefile runs one command, so
		// make -n skips it.
		got := mixedMakeTargets(makefileText(t, repoRoot(t)))
		want := []string{"ci-base-image", "presubmit"}
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
			t.Fatalf("mixedMakeTargets(the Makefile) = %v, want exactly %v", got, want)
		}
	})

	t.Run("a recorded line number is a recipe line", func(t *testing.T) {
		text := makefileText(t, repoRoot(t))
		physical := strings.Split(text, "\n")
		for target, number := range mixedMakeTargets(text) {
			if number < 1 || number > len(physical) {
				t.Fatalf("%s: line %d is outside the Makefile, which has %d lines", target, number, len(physical))
			}
			if line := physical[number-1]; !strings.HasPrefix(line, "\t") {
				t.Fatalf("%s: line %d is not a recipe line: %q", target, number, line)
			}
		}
	})
}
