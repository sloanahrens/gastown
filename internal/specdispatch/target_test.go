package specdispatch

import (
	"fmt"
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

// spliceAt replaces the first anchor in s with repl. An anchor s does not hold
// is an error: a helper that spliced nothing would hand the check a spec
// without the section the test names, and the test would pass for the wrong
// reason.
func spliceAt(s, anchor, repl string) (string, error) {
	if !strings.Contains(s, anchor) {
		return "", fmt.Errorf("anchor %q is absent from the fixture description", anchor)
	}
	return strings.Replace(s, anchor, repl, 1), nil
}

// specNaming is goodSpec with body as its Constraints and goal as its Goal.
func specNaming(t *testing.T, goal, body string) Spec {
	t.Helper()
	s := goodSpec()
	desc, err := spliceAt(goodDescription, "A thing exists.", goal)
	if err != nil {
		t.Fatal(err)
	}
	desc, err = spliceAt(desc, "## Constraints\nGo only.", "## Constraints\n"+body)
	if err != nil {
		t.Fatal(err)
	}
	s.Description = desc
	return s
}

// specWithFiles is specNaming with a Files section carrying files, placed
// before "## Out of scope".
func specWithFiles(t *testing.T, goal, body, files string) Spec {
	t.Helper()
	s := specNaming(t, goal, body)
	desc, err := spliceAt(s.Description, "## Out of scope", "## Files\n"+files+"\n\n## Out of scope")
	if err != nil {
		t.Fatal(err)
	}
	s.Description = desc
	return s
}

// A fixture splice refuses an anchor the description does not hold, and
// replaces the first anchor it does hold, once (gt-yp3a4).
func TestSpliceAtRefusesAnAbsentAnchor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		desc    string
		anchor  string
		want    string
		wantErr bool
	}{
		{name: "anchor present", desc: "a ## Files b ## Files", anchor: "## Files", want: "a X b ## Files"},
		{name: "anchor absent", desc: "a b", anchor: "## Files", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := spliceAt(tc.desc, tc.anchor, "X")
			if (err != nil) != tc.wantErr {
				t.Fatalf("spliceAt(%q, %q) error = %v, want error %v", tc.desc, tc.anchor, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("spliceAt(%q, %q) = %q, want %q", tc.desc, tc.anchor, got, tc.want)
			}
		})
	}
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
			body:   "Change internal/cmd/spec.go and scripts/deploy.sh.",
			target: targetOf("internal/cmd/spec.go", "scripts/deploy.sh"),
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
			s := specNaming(t, goal, tc.body)
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
			for _, clean := range []string{"internal/cmd/spec.go:", "scripts/deploy.sh"} {
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
	s := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	s.Target = targetOf()
	if got := Lint(s, defaultTemplate()).Reason; !strings.Contains(got, "absent from main") {
		t.Errorf("Reason = %q, want the branch named", got)
	}
	s.Target = &TargetTree{Files: map[string]struct{}{}}
	if got := Lint(s, defaultTemplate()).Reason; !strings.Contains(got, "the target branch") {
		t.Errorf("Reason = %q, want an unnamed branch reported as the target branch", got)
	}
}

// A missing-path refusal ends with the rewrite that passes: both cues the lint
// reads, written with the refused path, and where the cue must sit (gt-hgt9j.1).
func TestTargetRefusalHintNamesBothFixes(t *testing.T) {
	t.Parallel()
	s := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	s.Target = targetOf("internal/cmd/spec.go")
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s, want a refusal naming ## Constraints: %s", v.Route, v.Line(s.ID))
	}
	for _, want := range []string{
		"create internal/cmd/bird.go",
		"internal/cmd/bird.go is created by bead <id>",
		"see internal/cmd/bird.go",
		"the cue must sit directly beside the path",
	} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
}

// The hint is appended to the sentence the dispatcher and the existing target
// tests read, never a replacement for it (gt-hgt9j.1).
func TestTargetRefusalHintKeepsFirstSentence(t *testing.T) {
	t.Parallel()
	s := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	s.Target = targetOf("internal/cmd/spec.go")
	reason := Lint(s, defaultTemplate()).Reason
	const first = "names internal/cmd/bird.go, absent from main"
	if !strings.HasPrefix(reason, first) {
		t.Fatalf("Reason = %q, want it to start with %q", reason, first)
	}
	if !strings.Contains(reason, "create internal/cmd/bird.go") {
		t.Errorf("Reason = %q, want the fix hint appended", reason)
	}
}

// The appended hint keeps the one-line report one line, for one absent path and
// for several; with several, the hint is an example written with the first of
// them (gt-hgt9j.1).
func TestTargetRefusalHintStaysOneLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		hint string
	}{
		{
			name: "one path",
			body: "Change internal/cmd/bird.go.",
			hint: "To fix, write `create internal/cmd/bird.go`",
		},
		{
			name: "three paths",
			body: "Change internal/cmd/bird.go, internal/cmd/owl.go and internal/cmd/hawk.go.",
			hint: "To fix each, for example, write `create internal/cmd/bird.go`",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := specNaming(t, "A thing exists.", tc.body)
			s.Target = targetOf("internal/cmd/spec.go")
			line := Lint(s, defaultTemplate()).Line(s.ID)
			if strings.Contains(line, "\n") {
				t.Errorf("Line = %q, want one line", line)
			}
			if !strings.Contains(line, tc.hint) {
				t.Errorf("Line = %q, want %q", line, tc.hint)
			}
		})
	}
}

// The rewrite the refusal prints is the rewrite that passes: each form the hint
// prints is printed, and a Constraints body holding only that form lints clean
// against the same target, so a filer can copy it and the next run clears. The
// body names no other form of the path, so each cue is read on its own: a body
// carrying the bare path next to the cue would pass through any one cue
// (gt-hgt9j.1).
func TestTargetRefusalHintIsTheRewriteThatPasses(t *testing.T) {
	t.Parallel()
	const path = "internal/cmd/bird.go"
	target := targetOf("internal/cmd/spec.go")
	s := specNaming(t, "A thing exists.", "Change "+path+".")
	s.Target = target
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("Lint = %s, want the refusal to copy the hint from: %s", v.Route, v.Line(s.ID))
	}
	forms := append(append([]string{}, addsRewrites...), readsRewrites...)
	for _, form := range forms {
		rewrite := fmt.Sprintf(form, path)
		t.Run(rewrite, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(v.Reason, "`"+rewrite+"`") {
				t.Errorf("Reason = %q, want the rewrite %q printed", v.Reason, rewrite)
			}
			fixed := specNaming(t, "A thing exists.", "Do the work: "+rewrite+".")
			fixed.Target = target
			if got := Lint(fixed, defaultTemplate()); !got.Clean() {
				t.Errorf("a bead carrying only %q was refused: %s", rewrite, got.Line(fixed.ID))
			}
		})
	}
}

// An inventory that could not be read is held with its error named, never read
// as one holding no files; a bead naming no repo file has nothing to check, so
// the failed read does not hold it (gt-7t1im).
func TestLintHoldsABeadWhenTheTargetTreeCouldNotBeRead(t *testing.T) {
	t.Parallel()
	failed := &TargetTree{Branch: "main", Err: "no git repo under /town/gastown"}

	named := specNaming(t, "A thing exists.", "Change internal/cmd/spec.go.")
	named.Target = failed
	v := Lint(named, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s, want a refusal naming ## Constraints", v.Line(named.ID))
	}
	if !strings.Contains(v.Reason, "internal/cmd/spec.go") || !strings.Contains(v.Reason, "no git repo under /town/gastown") {
		t.Errorf("Reason = %q, want the path and the read's error named", v.Reason)
	}

	plain := specNaming(t, "A thing exists.", "Go only.")
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
	named := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	v := Lint(named, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s, want a refusal naming ## Constraints: %s", v.Route, v.Line(named.ID))
	}
	if !strings.Contains(v.Reason, "internal/cmd/bird.go") || !strings.Contains(v.Reason, "no target inventory") {
		t.Errorf("Reason = %q, want the path and the missing inventory named", v.Reason)
	}

	plain := specNaming(t, "A thing exists.", "Go only.")
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
			name: "a leading dot is part of the name and ./ is the repo root",
			body: "Change .github/workflows/ci.yml, then ./internal/cmd/spec.go and (./scripts/x.sh).",
			want: []string{".github/workflows/ci.yml", "internal/cmd/spec.go", "scripts/x.sh"},
		},
		{
			name: "a parent path is never a repo file",
			body: "Not ../internal/cmd/spec.go and not ./../scripts/x.sh.",
			want: nil,
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
	s := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	if got := LintShape(s, defaultTemplate()); !got.Clean() {
		t.Errorf("LintShape = %s, want the shape verdict alone: %s", got.Route, got.Line(s.ID))
	}
	if got := Lint(s, defaultTemplate()); got.Clean() {
		t.Errorf("Lint passed a bead it had no inventory to check: %s", got.Line(s.ID))
	}
}

// A bead with a Files section is checked from that section alone: a path its
// Constraints names, with no cue and absent from the branch, is not refused
// when the Files section does not list it (gt-hgt9j.10).
func TestPathCheckUsesFilesWhenPresent(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Change internal/cmd/bird.go.", "create: internal/cli/foo.go")
	s.Target = targetOf("internal/cmd/spec.go")
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a bead with a Files section was refused for a Constraints path it does not list: %s", v.Line(s.ID))
	}
}

// A modify path the Files section lists is checked from that section, not from
// the Constraints body that also names it: the Constraints check would refuse
// bird.go as absent from main, and an open blocker that creates it is what
// makes the Files check pass it (gt-yp3a4).
func TestFilesSuppressesConstraintsCheckForModifyPath(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Change internal/cmd/bird.go.", "modify: internal/cmd/bird.go")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"internal/cmd/bird.go"}
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a modify path the Files section lists was refused by the Constraints check: %s", v.Line(s.ID))
	}
}

// Without the blocker, the same modify path is refused once, on the Files
// entry: the refusal names ## Files, not ## Constraints (gt-yp3a4).
func TestFilesRefusesModifyPathNamedInConstraints(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Change internal/cmd/bird.go.", "modify: internal/cmd/bird.go")
	s.Target = targetOf("internal/cmd/spec.go")
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(s.ID))
	}
	if !strings.Contains(v.Reason, "modify: internal/cmd/bird.go") {
		t.Errorf("Reason = %q, want the modify entry named", v.Reason)
	}
}

// A read path a bead declares must exist on the branch it lands on; a refusal
// names the section, the verb and the path (gt-hgt9j.10).
func TestPathCheckFilesReadMustExist(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "read: internal/cmd/bird.go")
	s.Target = targetOf("internal/cmd/spec.go")
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(s.ID))
	}
	for _, want := range []string{"read: internal/cmd/bird.go", "absent from main"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
	held := specWithFiles(t, "A thing exists.", "Go only.", "read: internal/cmd/spec.go")
	held.Target = targetOf("internal/cmd/spec.go")
	if got := Lint(held, defaultTemplate()); !got.Clean() {
		t.Errorf("a read path the branch holds was refused: %s", got.Line(held.ID))
	}
}

// A modify path a bead declares must exist on the branch it lands on; a
// refusal names the section, the verb and the path (gt-hgt9j.10).
func TestPathCheckFilesModifyMustExist(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "modify: internal/cmd/bird.go")
	s.Target = targetOf("internal/cmd/spec.go")
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(s.ID))
	}
	for _, want := range []string{"modify: internal/cmd/bird.go", "absent from main"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
	held := specWithFiles(t, "A thing exists.", "Go only.", "modify: internal/cmd/spec.go")
	held.Target = targetOf("internal/cmd/spec.go")
	if got := Lint(held, defaultTemplate()); !got.Clean() {
		t.Errorf("a modify path the branch holds was refused: %s", got.Line(held.ID))
	}
}

// A create path is the bead's own addition, so the branch need not hold it
// (gt-hgt9j.10).
func TestPathCheckFilesCreateNeedsNothing(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "create: internal/cli/foo.go")
	s.Target = targetOf("internal/cmd/spec.go")
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a create path absent from the branch was refused: %s", v.Line(s.ID))
	}
}

// A bead without a Files section keeps the cue grammar unchanged: an
// uncued path absent from the branch is still refused against ## Constraints,
// and the introduce and reference cues still pass (gt-hgt9j.10).
func TestPathCheckWithoutFilesUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		missing string // "" is a clean lint
	}{
		{name: "an uncued change is refused", body: "Change internal/cmd/bird.go.", missing: "internal/cmd/bird.go"},
		{name: "an introduced file passes", body: "Add internal/cli/foo.go.", missing: ""},
		{name: "a reference passes", body: "Keep it as in docs/guides/spec-dispatcher.md.", missing: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := specNaming(t, "A thing exists.", tc.body)
			s.Target = targetOf("internal/cmd/spec.go")
			v := Lint(s, defaultTemplate())
			if tc.missing == "" {
				if !v.Clean() {
					t.Fatalf("Lint = %s, want dispatch: %s", v.Route, v.Line(s.ID))
				}
				return
			}
			if v.Route != RouteRefuse || v.Field != "## Constraints" {
				t.Fatalf("Lint = %s (%s), want a refusal naming ## Constraints: %s", v.Route, v.Field, v.Line(s.ID))
			}
			if !strings.Contains(v.Reason, tc.missing) {
				t.Errorf("Reason = %q, want the path %q named", v.Reason, tc.missing)
			}
		})
	}
}

// A Files section that does not parse is refused with the parse error, which
// names the line and its text (gt-hgt9j.10).
func TestPathCheckFilesMalformedRefused(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "create: internal/cli/foo.go\nnot a verb line")
	s.Target = targetOf("internal/cmd/spec.go")
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(s.ID))
	}
	for _, want := range []string{"line 9", "not a verb line"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
}

// A modify or read path with no inventory behind it stays refused: a check
// that did not run is not a check that passed. A bead whose Files section only
// creates paths has nothing to check, so a missing inventory does not hold it
// (gt-hgt9j.10).
func TestPathCheckFilesNeedAnInventory(t *testing.T) {
	t.Parallel()
	named := specWithFiles(t, "A thing exists.", "Go only.", "modify: internal/cmd/spec.go")
	v := Lint(named, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(named.ID))
	}
	for _, want := range []string{"modify: internal/cmd/spec.go", "no target inventory"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}

	failed := specWithFiles(t, "A thing exists.", "Go only.", "read: internal/cmd/spec.go")
	failed.Target = &TargetTree{Branch: "main", Err: "no git repo under /town/gastown"}
	v = Lint(failed, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(failed.ID))
	}
	for _, want := range []string{"read: internal/cmd/spec.go", "no git repo under /town/gastown"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}

	created := specWithFiles(t, "A thing exists.", "Go only.", "create: internal/cli/foo.go")
	if got := Lint(created, defaultTemplate()); !got.Clean() {
		t.Errorf("a create-only bead was held for a missing inventory: %s", got.Line(created.ID))
	}
}

// A modify path absent from the branch passes when an open blocker bead
// declares it creates that path: a chain of beads where the second edits what
// the first adds is normal (gt-hgt9j.11).
func TestBlockerCreatesModifyPasses(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "modify: internal/cli/foo.go")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"internal/cli/foo.go"}
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a modify path an open blocker creates was refused: %s", v.Line(s.ID))
	}
}

// A read path absent from the branch passes on a blocker's create path too
// (gt-hgt9j.11).
func TestBlockerCreatesReadPasses(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "read: internal/cli/foo.go")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"internal/cli/foo.go"}
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a read path an open blocker creates was refused: %s", v.Line(s.ID))
	}
}

// A path neither the branch holds nor an open blocker creates is still
// refused, and the reason says why the blocker set did not rescue it
// (gt-hgt9j.11).
func TestBlockerCreatesRefusalNamesTheGap(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "modify: internal/cli/foo.go")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"internal/cli/bar.go"}
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Files" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Files: %s", v.Route, v.Field, v.Line(s.ID))
	}
	for _, want := range []string{"modify: internal/cli/foo.go", "absent from main", "no open blocker creates it"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
}

// The blocker set is read only for a bead with a Files section: a cue-grammar
// bead naming an absent path is refused exactly as before, even when the set
// holds that path (gt-hgt9j.11).
func TestBlockerCreatesIgnoredWithoutFiles(t *testing.T) {
	t.Parallel()
	s := specNaming(t, "A thing exists.", "Change internal/cmd/bird.go.")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"internal/cmd/bird.go"}
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("Lint = %s (%s), want a refusal naming ## Constraints: %s", v.Route, v.Field, v.Line(s.ID))
	}
	for _, want := range []string{"internal/cmd/bird.go", "absent from main"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("Reason = %q, want %q", v.Reason, want)
		}
	}
}

// Paths are compared exactly after path.Clean: a relative spelling in the
// blocker set names the same file as the Files entry's clean path
// (gt-hgt9j.11).
func TestBlockerCreatesCleansPaths(t *testing.T) {
	t.Parallel()
	s := specWithFiles(t, "A thing exists.", "Go only.", "modify: b.go")
	s.Target = targetOf("internal/cmd/spec.go")
	s.BlockerCreates = []string{"./a/../b.go"}
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("a clean path was refused against a relative blocker path: %s", v.Line(s.ID))
	}
}
