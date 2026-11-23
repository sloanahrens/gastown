//go:build integration

package makegate

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
//
// A text that is empty or only whitespace, or one holding no rule line, is an
// error: a set computed from nothing must not read as a clean tree (gt-3hdoli).
func mixedMakeTargets(makefile string) (map[string]int, error) {
	if strings.TrimSpace(makefile) == "" {
		return nil, errors.New("the Makefile is empty")
	}
	mixed := map[string]int{}
	var targets []string
	ruleFound := false
	for _, line := range mixedMakeLogicalLines(makefile) {
		if !strings.HasPrefix(line.text, "\t") {
			if names := mixedMakeRuleTargets(line.text); names != nil {
				targets = names
				ruleFound = true
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
	if !ruleFound {
		return nil, errors.New("no rule found in the Makefile")
	}
	return mixed, nil
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

// mixedGuardFindings returns one alarm per disagreement between the mixed
// targets in root's Makefile and guarded, bead gt-az0vxj's list. A computed
// target missing from the list names itself and its line; a listed name with no
// mixed line is stale; a Makefile that cannot be read is an error (gt-jiwfzd),
// and so is one the reader finds empty or rule-less (gt-3hdoli).
func mixedGuardFindings(root string, guarded []string) ([]string, error) {
	path := filepath.Join(root, "Makefile")
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	mixed, err := mixedMakeTargets(string(text))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var alarms []string
	for _, target := range slices.Sorted(maps.Keys(mixed)) {
		if !slices.Contains(guarded, target) {
			alarms = append(alarms, fmt.Sprintf("Makefile:%d: %s holds $(MAKE) beside another command, so make -n runs it for real: keep the target's tests off make -n on the real tree and list it with the guarded dry-run targets, or split the $(MAKE) call onto its own line", mixed[target], target))
		}
	}
	for _, target := range guarded {
		if _, ok := mixed[target]; !ok {
			alarms = append(alarms, fmt.Sprintf("%s is on the guarded dry-run list but no recipe line under it mixes $(MAKE) with another command: remove the stale name from the list", target))
		}
	}
	return alarms, nil
}

// scratchMakefile writes text as root/Makefile under a fresh temporary
// directory and returns it, so a case reads a copy of the checkout.
func scratchMakefile(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(text), 0o644); err != nil {
		t.Fatalf("write the scratch Makefile: %v", err)
	}
	return root
}

// checkMixedGuard fails unless root's Makefile yields exactly want.
func checkMixedGuard(t *testing.T, root string, want []string) {
	t.Helper()
	alarms, err := mixedGuardFindings(root, dryRunTargets)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(alarms, want) {
		t.Fatalf("mixedGuardFindings(%s) = %q, want %q", root, alarms, want)
	}
}

// TestIntegrationMixedMakeTargetsAreGuarded requires the computed mixed-target
// set to equal the guarded list exactly (gt-jiwfzd). Mutation: add a mixed
// recipe line to any target and it fails naming it and its line; remove one
// from a listed target and it fails naming that name as stale.
func TestIntegrationMixedMakeTargetsAreGuarded(t *testing.T) {
	checkMixedGuard(t, repoRoot(t), nil)
}

// TestIntegrationMixedMakeScratchCopy drives both disagreements on scratch
// Makefiles (gt-jiwfzd). Mutation: drop either check and its case passes.
func TestIntegrationMixedMakeScratchCopy(t *testing.T) {
	base := makefileText(t, repoRoot(t))
	t.Run("an added mixed rule is named with its line", func(t *testing.T) {
		root := scratchMakefile(t, base+"zz-mixed:\n\t@$(MAKE) y && true\n")
		want := fmt.Sprintf("Makefile:%d: zz-mixed holds $(MAKE) beside another command, so make -n runs it for real: keep the target's tests off make -n on the real tree and list it with the guarded dry-run targets, or split the $(MAKE) call onto its own line",
			len(strings.Split(base, "\n"))+1)
		checkMixedGuard(t, root, []string{want})
	})
	t.Run("a listed name no longer mixed is stale", func(t *testing.T) {
		// Presubmit stays mixed; ci-base-image's call is alone on its line.
		root := scratchMakefile(t, "presubmit:\n\t@$(MAKE) a && b\nci-base-image:\n\t@$(MAKE) --no-print-directory build\n")
		checkMixedGuard(t, root, []string{"ci-base-image is on the guarded dry-run list but no recipe line under it mixes $(MAKE) with another command: remove the stale name from the list"})
	})
}

// TestIntegrationMixedMakeUnreadable requires a Makefile the guard cannot read
// to fail it instead of passing it (gt-3hdoli). Mutation: return an empty set
// and no error from mixedMakeTargets for either cause and its row passes.
func TestIntegrationMixedMakeUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write bool
		text  string
		want  string
	}{
		{name: "no Makefile", want: "no such file"},
		{name: "a zero-byte Makefile", write: true, want: "the Makefile is empty"},
		{name: "a Makefile of only comments and blank lines", write: true, text: "# a comment\n\n# another\n", want: "no rule found in the Makefile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.write {
				root = scratchMakefile(t, tc.text)
			}
			path := filepath.Join(root, "Makefile")
			alarms, err := mixedGuardFindings(root, dryRunTargets)
			if err == nil {
				t.Fatalf("mixedGuardFindings(%s) returned %q and no error: a guard that read nothing has cleared the tree", path, alarms)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("mixedGuardFindings(%s) error = %q, want one holding %q", path, err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("mixedGuardFindings(%s) error = %q, want the path in it", path, err)
			}
		})
	}
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
			wantErr  string
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
				name:     "an empty Makefile is an error",
				makefile: "",
				wantErr:  "the Makefile is empty",
			},
			{
				name:     "a whitespace-only Makefile is an error",
				makefile: " \n\t\n\n",
				wantErr:  "the Makefile is empty",
			},
			{
				name:     "a line with no colon is not a rule line, so no rule is found",
				makefile: "VAR ?= x\n\t$(MAKE) a; b\n",
				wantErr:  "no rule found in the Makefile",
			},
			{
				name:     "an assignment holding a colon is not a rule line, so no rule is found",
				makefile: "URL = https://example.com\n\t$(MAKE) a; b\n",
				wantErr:  "no rule found in the Makefile",
			},
			{
				name:     "a := assignment is not a rule line, so no rule is found",
				makefile: "X := y\n\t$(MAKE) a; b\n",
				wantErr:  "no rule found in the Makefile",
			},
			{
				name:     "a ::= assignment is not a rule line, so no rule is found",
				makefile: "X ::= y\n\t$(MAKE) a; b\n",
				wantErr:  "no rule found in the Makefile",
			},
			{
				name:     "a recipe line with no rule line above it is an error",
				makefile: "\t$(MAKE) a; b\n",
				wantErr:  "no rule found in the Makefile",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := mixedMakeTargets(tc.makefile)
				if tc.wantErr != "" {
					if err == nil {
						t.Fatalf("mixedMakeTargets(%q) = %v and no error, want the error %q", tc.makefile, got, tc.wantErr)
					}
					if !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("mixedMakeTargets(%q) error = %q, want one holding %q", tc.makefile, err, tc.wantErr)
					}
					if got != nil {
						t.Fatalf("mixedMakeTargets(%q) = %v beside an error, want no set", tc.makefile, got)
					}
					return
				}
				if err != nil {
					t.Fatalf("mixedMakeTargets(%q) failed: %v", tc.makefile, err)
				}
				if !maps.Equal(got, tc.want) {
					t.Fatalf("mixedMakeTargets(%q) = %v, want %v", tc.makefile, got, tc.want)
				}
			})
		}
	})

	t.Run("the checkout Makefile holds exactly the two mixed targets", func(t *testing.T) {
		// Every other $(MAKE) recipe line in the Makefile runs one command, so
		// make -n skips it.
		got, err := mixedMakeTargets(makefileText(t, repoRoot(t)))
		if err != nil {
			t.Fatalf("mixedMakeTargets(the Makefile): %v", err)
		}
		want := []string{"ci-base-image", "presubmit"}
		if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
			t.Fatalf("mixedMakeTargets(the Makefile) = %v, want exactly %v", got, want)
		}
	})

	t.Run("a recorded line number is a recipe line", func(t *testing.T) {
		text := makefileText(t, repoRoot(t))
		physical := strings.Split(text, "\n")
		mixed, err := mixedMakeTargets(text)
		if err != nil {
			t.Fatalf("mixedMakeTargets(the Makefile): %v", err)
		}
		for target, number := range mixed {
			if number < 1 || number > len(physical) {
				t.Fatalf("%s: line %d is outside the Makefile, which has %d lines", target, number, len(physical))
			}
			if line := physical[number-1]; !strings.HasPrefix(line, "\t") {
				t.Fatalf("%s: line %d is not a recipe line: %q", target, number, line)
			}
		}
	})
}
