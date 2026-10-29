package specdispatch

import (
	"strings"
	"testing"
)

// targetOf is a target tree on main holding files.
func targetOf(files ...string) *TargetTree {
	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		set[f] = struct{}{}
	}
	return &TargetTree{Branch: "main", Files: set}
}

// specNaming is goodSpec with body as its Constraints and goal as its Goal.
func specNaming(goal, body string) Spec {
	s := goodSpec()
	s.Description = strings.Replace(goodDescription, "A thing exists.", goal, 1)
	s.Description = strings.Replace(s.Description, "## Constraints\nGo only.", "## Constraints\n"+body, 1)
	return s
}

// The check reads the files it checks from the Constraints section, and passes
// any token it cannot read as a repo file: a directory, a symbol, a glob and a
// URL are not files to change. It also passes a path the spec's own words
// introduce as a new file, in the Goal or the Constraints: a file the spec adds
// is not one the branch is expected to hold (gt-7t1im).
func TestLintChecksTheConstraintsPathsAgainstTheTargetBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		goal    string
		body    string
		target  *TargetTree
		missing string // "" is a clean lint
	}{
		{
			name:   "a path the branch holds",
			body:   "Change internal/cmd/spec.go and scripts/tier-sweep.sh.",
			target: targetOf("internal/cmd/spec.go", "scripts/tier-sweep.sh"),
		},
		{
			name:    "a path the branch does not hold",
			body:    "Change internal/cmd/bird.go.",
			target:  targetOf("internal/cmd/spec.go"),
			missing: "internal/cmd/bird.go",
		},
		{
			name:   "a path only the Goal introduces",
			goal:   "Add internal/cmd/bird.go, which does not exist yet.",
			body:   "Go only.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a new file the Constraints introduces",
			body:   "Add a new file internal/cli/foo.go holding the parser.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a new file the Goal introduces and the Constraints names",
			goal:   "Adds internal/cli/foo.go, the parser.",
			body:   "internal/cli/foo.go holds the parser; do not touch internal/cmd/spec.go.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a new file the add verb introduces",
			body:   "Add internal/cli/foo.go with the parser.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			// The evidence bead's own Constraints (gt-08chr): the file it
			// changes is named mid-sentence, beside no introduction, and the
			// bead was slung against a base that never landed.
			name:    "the evidence bead's Constraints",
			body:    "internal/config: one field for the real claude path; internal/cmd/bird.go birdClaudePath reads it, and bird.Runner.Claude stays the injected seam.",
			target:  targetOf("internal/config/config.go"),
			missing: "internal/cmd/bird.go",
		},
		{
			name:    "a file changed beside the add verb",
			body:    "Adds the check to internal/cmd/bird.go.",
			target:  targetOf("internal/cmd/spec.go"),
			missing: "internal/cmd/bird.go",
		},
		{
			name:   "a reference the spec only cites",
			body:   "Keep the wording as in docs/guides/spec-dispatcher.md.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			// A path the spec cites and also changes is checked: one mention
			// that is not a reference cue is enough to make it a file to change.
			name:    "a reference the spec also changes",
			body:    "Keep it as in internal/cmd/bird.go, and change internal/cmd/bird.go.",
			target:  targetOf("internal/cmd/spec.go"),
			missing: "internal/cmd/bird.go",
		},
		{
			// The new-file phrase exempts the path it sits beside, not the other
			// path on its line.
			name:    "a new file beside a changed path on one line",
			body:    "Add a new file internal/cli/foo.go and change internal/cmd/bird.go.",
			target:  targetOf("internal/cmd/spec.go"),
			missing: "internal/cmd/bird.go",
		},
		{
			name:   "a read-only path beside a new file",
			body:   "internal/cmd/spec.go is read-only; add internal/cli/foo.go with the parser.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a directory",
			body:   "Add the check under internal/specdispatch.",
			target: targetOf("internal/specdispatch/lint.go"),
		},
		{
			name:   "a symbol",
			body:   "Call bird.Runner.Claude from the lint.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a glob",
			body:   "Change internal/cmd/*.go only.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a URL",
			body:   "See https://github.com/steveyegge/gastown/internal/cmd/spec.go.",
			target: targetOf("internal/cmd/spec.go"),
		},
		{
			name:   "a citation with a line number",
			body:   "The check lives in `internal/specdispatch/lint.go:473`.",
			target: targetOf("internal/specdispatch/lint.go"),
		},
		{
			name:    "one of two paths missing",
			body:    "Change internal/cmd/spec.go and internal/cmd/bird.go.",
			target:  targetOf("internal/cmd/spec.go"),
			missing: "internal/cmd/bird.go",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			goal := "A thing exists."
			if tc.goal != "" {
				goal = tc.goal
			}
			s := specNaming(goal, tc.body)
			s.Target = tc.target
			v := Lint(s, defaultTemplate())
			if tc.missing == "" {
				if v.Route != RouteDispatch {
					t.Fatalf("Lint = %s, want dispatch: %s", v.Route, v.Line(s.ID))
				}
				return
			}
			if v.Route != RouteRefuse || v.Field != "## Constraints" {
				t.Fatalf("Lint = %s (%s), want a refusal naming ## Constraints", v.Route, v.Line(s.ID))
			}
			if !strings.Contains(v.Reason, tc.missing) {
				t.Errorf("Reason = %q, want the path %q named", v.Reason, tc.missing)
			}
			for _, clean := range []string{"internal/cmd/spec.go:", "scripts/tier-sweep.sh"} {
				if clean != tc.missing && strings.Contains(v.Reason, clean) {
					t.Errorf("Reason = %q, names %q, which the branch holds", v.Reason, clean)
				}
			}
			if got := v.UnshapedReason(); got != "unshaped: ## Constraints" {
				t.Errorf("UnshapedReason = %q", got)
			}
		})
	}
}

// A refusal names the branch, so a reader knows which base the path is missing
// from.
func TestLintNamesTheTargetBranchInTheRefusal(t *testing.T) {
	t.Parallel()
	s := specNaming("A thing exists.", "Change internal/cmd/bird.go.")
	s.Target = targetOf()
	if got := Lint(s, defaultTemplate()).Reason; !strings.Contains(got, "absent from main") {
		t.Errorf("Reason = %q, want the branch named", got)
	}
	s.Target = &TargetTree{Files: map[string]struct{}{}}
	if got := Lint(s, defaultTemplate()).Reason; !strings.Contains(got, "the target branch") {
		t.Errorf("Reason = %q, want an unnamed branch reported as the target branch", got)
	}
}

// An inventory that could not be read is held with its error named, never read
// as one holding no files; a bead naming no repo file has nothing to check, so
// the failed read does not hold it (gt-7t1im).
func TestLintHoldsABeadWhenTheTargetTreeCouldNotBeRead(t *testing.T) {
	t.Parallel()
	failed := &TargetTree{Branch: "main", Err: "no git repo under /town/gastown"}

	named := specNaming("A thing exists.", "Change internal/cmd/spec.go.")
	named.Target = failed
	v := Lint(named, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s, want a refusal naming ## Constraints", v.Line(named.ID))
	}
	if !strings.Contains(v.Reason, "internal/cmd/spec.go") || !strings.Contains(v.Reason, "no git repo under /town/gastown") {
		t.Errorf("Reason = %q, want the path and the read's error named", v.Reason)
	}

	plain := specNaming("A thing exists.", "Go only.")
	plain.Target = failed
	if got := Lint(plain, defaultTemplate()); !got.Clean() {
		t.Errorf("a bead naming no file was held: %s", got.Line(plain.ID))
	}
}

// A caller that read no inventory asked no path question, and the lint refuses
// a bead naming a file for it rather than passing it: a check that did not run
// is not a check that passed. A bead naming no file has nothing to check, so
// the missing inventory does not hold it (gt-7t1im).
func TestLintRefusesANamedFileWithoutATargetTree(t *testing.T) {
	t.Parallel()
	named := specNaming("A thing exists.", "Change internal/cmd/bird.go.")
	v := Lint(named, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s, want a refusal naming ## Constraints: %s", v.Route, v.Line(named.ID))
	}
	if !strings.Contains(v.Reason, "internal/cmd/bird.go") || !strings.Contains(v.Reason, "no target inventory") {
		t.Errorf("Reason = %q, want the path and the missing inventory named", v.Reason)
	}

	plain := specNaming("A thing exists.", "Go only.")
	if got := Lint(plain, defaultTemplate()); !got.Clean() {
		t.Errorf("a bead naming no file was held: %s", got.Line(plain.ID))
	}
}

// A Constraints body with no repo file in it never reaches the inventory.
func TestConstraintFilePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "prose paths",
			body: "Change internal/cmd/bird.go and internal/cmd/bird_test.go; leave docs/plans and the scripts/ directory alone.",
			want: []string{"internal/cmd/bird.go", "internal/cmd/bird_test.go"},
		},
		{
			name: "wrapping punctuation and citations",
			body: "Change (`internal/cmd/spec.go:42`), and (internal/specdispatch/lint.go:10:4).",
			want: []string{"internal/cmd/spec.go", "internal/specdispatch/lint.go"},
		},
		{
			name: "duplicates keep the first sighting",
			body: "internal/cmd/spec.go is where it ends; internal/cmd/spec.go again.",
			want: []string{"internal/cmd/spec.go"},
		},
		{
			name: "not repo files",
			body: "The internal/cmd package, gt spec lint, bird.Runner.Claude, ~/.local/bin and .om.json.",
			want: nil,
		},
		{
			name: "empty",
			body: "",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := constraintFilePaths(tc.body)
			want := tc.want
			if len(want) == 0 {
				want = nil
			}
			if len(got) == 0 {
				got = nil
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("constraintFilePaths(%q) = %v, want %v", tc.body, got, want)
			}
		})
	}
}

// The display lint asks the shape alone: a caller with no inventory — the
// dashboard's queue badge, the sweep's reshape — reads a file-naming bead as
// shape-clean, which is why the dispatcher asks Lint instead (gt-7t1im).
func TestLintShapeAsksNoPathQuestion(t *testing.T) {
	t.Parallel()
	s := specNaming("A thing exists.", "Change internal/cmd/bird.go.")
	if got := LintShape(s, defaultTemplate()); !got.Clean() {
		t.Errorf("LintShape = %s, want the shape verdict alone: %s", got.Route, got.Line(s.ID))
	}
	if got := Lint(s, defaultTemplate()); got.Clean() {
		t.Errorf("Lint passed a bead it had no inventory to check: %s", got.Line(s.ID))
	}
}
