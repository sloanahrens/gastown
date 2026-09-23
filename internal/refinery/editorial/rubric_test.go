package editorial

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/plugin"
)

// rubricBaseJSON is the rubric the base commit of a rubric fixture carries: two
// criteria, one of which (fail-open-branch) is the town's guard for the
// failure-serializes-to-success class that gt-2oi0 was filed about.
const rubricBaseJSON = `{
  "threshold": 0.6,
  "rubric": [
    {
      "name": "correctness",
      "weight": 3,
      "guidance": "Logic errors outrank all else."
    },
    {
      "name": "fail-open-branch",
      "weight": 2,
      "guidance": "Any gate whose failure path emits the success value is a finding."
    }
  ]
}`

// newRubricFixture is newReviewFixture with a real rubric: the base commit
// carries rubricBaseJSON, the head commit carries headRubric, origin/main
// tracks base, and the rig manifest records base's (the deployed rubric's)
// sha — what Run's rubric-touch routing checks a touching diff against
// (gt-7bvf) — so AssertVersion/AssertVersionWithRubric passes regardless of
// what headRubric proposes, and only the criterion guard is under test.
func newRubricFixture(t *testing.T, headRubric string) *reviewFixture {
	t.Helper()
	repoDir := initTestRepo(t)
	commitFileReview(t, repoDir, ".om.json", rubricBaseJSON, "add rubric")
	g := git.NewGit(repoDir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	cmd := exec.Command("git", "update-ref", "refs/remotes/origin/main", base)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref origin/main: %v\n%s", err, out)
	}

	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v\n%s", err, out)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}

	// An unchanged headRubric would leave nothing to commit, so the head
	// commit then touches an unrelated file instead: same rubric, still a
	// distinct head to diff.
	head := ""
	if headRubric == rubricBaseJSON {
		head = commitFileReview(t, repoDir, "untouched.txt", "rubric unchanged\n", "leave rubric alone")
	} else {
		head = commitFileReview(t, repoDir, ".om.json", headRubric, "change rubric")
	}

	rigDir := t.TempDir()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	binSum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	// The manifest pins the DEPLOYED rubric — base's content, since
	// origin/main tracks base above — never headRubric: a diff that touches
	// .om.json now asserts against origin/<target>'s committed content
	// (gt-7bvf), and stamping the branch's own proposed content here would
	// make AssertVersionWithRubric pass for the wrong reason.
	rubricSum := sha256.Sum256([]byte(rubricBaseJSON))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(binSum[:])
	m.OMBinary.Version = "1.4.0"
	m.Rubric.Path = ".om.json"
	m.Rubric.SHA256 = hex.EncodeToString(rubricSum[:])
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	return &reviewFixture{repoDir: repoDir, rigDir: rigDir, base: base, head: head}
}

// reviewDeps wires Run's collaborators against fixture's repo, counting gate
// script invocations so a test can assert the script never ran.
func reviewDeps(t *testing.T, fixture *reviewFixture) (Deps, *int) {
	t.Helper()
	fakeBDForReview(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			calls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}
	return deps, &calls
}

func criterion(name string, weight float64, guidance string) RubricCriterion {
	return RubricCriterion{Name: name, Weight: weight, Guidance: guidance}
}

func TestDiffRubric_ReportsRemovalWeightAndGuidance(t *testing.T) {
	base := []RubricCriterion{
		criterion("correctness", 3, "Logic errors outrank all else."),
		criterion("fail-open-branch", 2, "A gate whose failure path emits the success value is a finding."),
		criterion("tests", 2, "Changed behavior needs tests."),
	}

	tests := []struct {
		name string
		head []RubricCriterion
		want []RubricDeltaKind
	}{
		{
			name: "removed",
			head: []RubricCriterion{
				criterion("correctness", 3, "Logic errors outrank all else."),
				criterion("tests", 2, "Changed behavior needs tests."),
			},
			want: []RubricDeltaKind{RubricRemoved},
		},
		{
			name: "reweighted up",
			head: []RubricCriterion{
				criterion("correctness", 3, "Logic errors outrank all else."),
				criterion("fail-open-branch", 3, "A gate whose failure path emits the success value is a finding."),
				criterion("tests", 2, "Changed behavior needs tests."),
			},
			want: []RubricDeltaKind{RubricWeightChanged},
		},
		{
			name: "reworded",
			head: []RubricCriterion{
				criterion("correctness", 3, "Logic errors outrank all else."),
				criterion("fail-open-branch", 2, "Fail-open gates are a finding."),
				criterion("tests", 2, "Changed behavior needs tests."),
			},
			want: []RubricDeltaKind{RubricGuidanceChanged},
		},
		{
			name: "rewrapped guidance is not a change",
			head: []RubricCriterion{
				criterion("correctness", 3, "Logic errors outrank all else."),
				criterion("fail-open-branch", 2, "A gate whose failure path\n  emits the success value is a finding."),
				criterion("tests", 2, "Changed behavior needs tests."),
			},
			want: nil,
		},
		{
			name: "every criterion gone",
			head: nil,
			want: []RubricDeltaKind{RubricRemoved, RubricRemoved, RubricRemoved},
		},
		{
			name: "addition is not a finding",
			head: []RubricCriterion{
				criterion("correctness", 3, "Logic errors outrank all else."),
				criterion("fail-open-branch", 2, "A gate whose failure path emits the success value is a finding."),
				criterion("tests", 2, "Changed behavior needs tests."),
				criterion("docs-and-comments", 2, "Docs follow the writing rules."),
			},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deltas := DiffRubric(base, tc.head)
			if len(deltas) != len(tc.want) {
				t.Fatalf("DiffRubric returned %d deltas (%+v), want %d", len(deltas), deltas, len(tc.want))
			}
			for i, want := range tc.want {
				if deltas[i].Kind != want {
					t.Errorf("delta %d kind = %q, want %q", i, deltas[i].Kind, want)
				}
			}
		})
	}
}

func TestDiffRubric_RenamedCriterionIsRemoval(t *testing.T) {
	base := []RubricCriterion{criterion("fail-open-branch", 2, "Fail-open gates are a finding.")}
	head := []RubricCriterion{criterion("docs-and-comments", 2, "Docs follow the writing rules.")}

	deltas := DiffRubric(base, head)

	// The repro in gt-2oi0: a swap reads as an addition plus a removal, and it
	// is the removal that has to be refused.
	if len(deltas) != 1 || deltas[0].Name != "fail-open-branch" || deltas[0].Kind != RubricRemoved {
		t.Fatalf("DiffRubric = %+v, want one removal of fail-open-branch", deltas)
	}
	if !strings.Contains(deltas[0].Detail, "weight 2") {
		t.Errorf("delta detail %q does not name the lost weight", deltas[0].Detail)
	}
}

func TestParseRubricCriteria_EmptyAndMalformed(t *testing.T) {
	for _, data := range []string{"", "   \n", "\n"} {
		criteria, err := ParseRubricCriteria([]byte(data))
		if err != nil || criteria != nil {
			t.Errorf("ParseRubricCriteria(%q) = %v, %v; want nil, nil", data, criteria, err)
		}
	}

	if _, err := ParseRubricCriteria([]byte("{not json")); err == nil {
		t.Error("ParseRubricCriteria accepted malformed JSON")
	}

	// A document with no rubric key is a valid .om.json with nothing to lose.
	criteria, err := ParseRubricCriteria([]byte(`{"threshold": 0.6}`))
	if err != nil || len(criteria) != 0 {
		t.Errorf("ParseRubricCriteria(no rubric key) = %v, %v; want empty, nil", criteria, err)
	}
}

// TestRubricRelPath_ResolvesUnderSiblingClone is the gt-7bvf path-resolution
// fix: an absolute rubric path under a sibling clone of the same rig (mayor/rig,
// when repoDir is refinery/rig) resolves to the same repo-relative path as if
// it had been declared relative.
func TestRubricRelPath_ResolvesUnderSiblingClone(t *testing.T) {
	rigRoot := filepath.FromSlash("/gt/gastown")
	repoDir := filepath.Join(rigRoot, "refinery", "rig")

	cases := []struct {
		name         string
		rubricPath   string
		wantRel      string
		wantOK       bool
		wantResolved bool
	}{
		{"relative", ".om.json", ".om.json", true, true},
		{"absolute under repoDir itself", filepath.Join(repoDir, ".om.json"), ".om.json", true, true},
		{"absolute under sibling clone (mayor/rig)", filepath.Join(rigRoot, "mayor", "rig", ".om.json"), ".om.json", true, true},
		{"absolute under nested repo-relative dir of a sibling clone", filepath.Join(rigRoot, "mayor", "rig", "config", ".om.json"), "config/.om.json", true, true},
		{"unset", "", "", false, true},
		{"absolute outside every rig clone", filepath.FromSlash("/etc/other/.om.json"), "", true, false},
		{"absolute under a different rig entirely", filepath.FromSlash("/gt/otherrig/refinery/rig/.om.json"), "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, ok, resolved := rubricRelPath(repoDir, tc.rubricPath)
			if ok != tc.wantOK || resolved != tc.wantResolved {
				t.Fatalf("rubricRelPath(%q) = rel=%q ok=%v resolved=%v, want ok=%v resolved=%v", tc.rubricPath, rel, ok, resolved, tc.wantOK, tc.wantResolved)
			}
			if resolved && rel != tc.wantRel {
				t.Errorf("rubricRelPath(%q) rel = %q, want %q", tc.rubricPath, rel, tc.wantRel)
			}
		})
	}
}

// TestRubricTouched_FailsClosedOnUnresolvablePath is the gt-7bvf fail-closed
// requirement: a rubric path this cannot resolve to a repo-relative form must
// be reported as touched — never silently read as "untouched", which would
// disable every rubric protection built on RubricTouched.
func TestRubricTouched_FailsClosedOnUnresolvablePath(t *testing.T) {
	fixture := newRubricFixture(t, rubricBaseJSON)
	g := git.NewGit(fixture.repoDir)
	unresolvable := filepath.FromSlash("/etc/other/.om.json")

	touched, rel, err := RubricTouched(g, fixture.repoDir, unresolvable, fixture.base, fixture.head)
	if err == nil {
		t.Fatal("RubricTouched with an unresolvable rubric path returned nil error, want a fail-closed error")
	}
	if !touched {
		t.Error("RubricTouched with an unresolvable rubric path = false, want true (fail closed)")
	}
	if rel != "" {
		t.Errorf("RubricTouched rel = %q, want empty for an unresolvable path", rel)
	}
}

func TestHasRetirementLabel(t *testing.T) {
	if HasRetirementLabel(nil) {
		t.Error("HasRetirementLabel(nil) = true, want false")
	}
	if HasRetirementLabel([]string{"gt:merge-request", "rework"}) {
		t.Error("HasRetirementLabel without the label = true, want false")
	}
	if !HasRetirementLabel([]string{"gt:merge-request", RetirementLabel}) {
		t.Errorf("HasRetirementLabel with %q = false, want true", RetirementLabel)
	}
}

func TestDiffRubricAt_UntouchedRubricIsNoFinding(t *testing.T) {
	fixture := newRubricFixture(t, rubricBaseJSON)
	deltas, err := DiffRubricAt(git.NewGit(fixture.repoDir), fixture.repoDir, ".om.json", fixture.base, fixture.head)
	if err != nil {
		t.Fatalf("DiffRubricAt: %v", err)
	}
	if len(deltas) != 0 {
		t.Fatalf("DiffRubricAt = %+v, want no deltas for an identical rubric", deltas)
	}
}

func TestDiffRubricAt_NoRubricDeclaredIsNoFinding(t *testing.T) {
	fixture := newRubricFixture(t, `{"rubric": []}`)
	deltas, err := DiffRubricAt(git.NewGit(fixture.repoDir), fixture.repoDir, "", fixture.base, fixture.head)
	if err != nil {
		t.Fatalf("DiffRubricAt: %v", err)
	}
	if len(deltas) != 0 {
		t.Fatalf("DiffRubricAt with no rubric path = %+v, want no deltas", deltas)
	}
}

func TestDiffRubricAt_DeletedRubricRemovesEveryCriterion(t *testing.T) {
	fixture := newRubricFixture(t, rubricBaseJSON)
	if err := os.Remove(filepath.Join(fixture.repoDir, ".om.json")); err != nil {
		t.Fatalf("remove rubric: %v", err)
	}
	g := git.NewGit(fixture.repoDir)
	if err := g.Add(".om.json"); err != nil {
		t.Fatalf("stage deletion: %v", err)
	}
	if err := g.Commit("delete rubric"); err != nil {
		t.Fatalf("commit deletion: %v", err)
	}
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}

	deltas, err := DiffRubricAt(g, fixture.repoDir, ".om.json", fixture.base, head)
	if err != nil {
		t.Fatalf("DiffRubricAt: %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("DiffRubricAt on a deleted rubric = %+v, want both criteria removed", deltas)
	}
	for _, d := range deltas {
		if d.Kind != RubricRemoved {
			t.Errorf("delta %+v, want kind %q", d, RubricRemoved)
		}
	}
}

func TestRun_RefusesCriterionRemoval(t *testing.T) {
	// The gt-2oi0 repro: fail-open-branch replaced by docs-and-comments.
	const head = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`
	fixture := newRubricFixture(t, head)
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if *calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0: the guard must refuse before review", *calls)
	}
	if result.Exit != 2 || result.Class != RubricRegression {
		t.Fatalf("Exit/Class = %d/%q, want 2/%q", result.Exit, result.Class, RubricRegression)
	}
	if !strings.Contains(result.Stderr, "fail-open-branch") {
		t.Errorf("stderr %q does not name the removed criterion", result.Stderr)
	}
	if !strings.Contains(result.Stderr, RetirementLabel) {
		t.Errorf("stderr %q does not name the %s escape hatch", result.Stderr, RetirementLabel)
	}
}

func TestRun_RefusesGuidanceRewrite(t *testing.T) {
	const head = `{
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Prefer explicit errors."}
  ]
}`
	fixture := newRubricFixture(t, head)
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if *calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0", *calls)
	}
	if result.Exit != 2 || result.Class != RubricRegression {
		t.Fatalf("Exit/Class = %d/%q, want 2/%q", result.Exit, result.Class, RubricRegression)
	}
	if !strings.Contains(result.Stderr, string(RubricGuidanceChanged)) {
		t.Errorf("stderr %q does not report a guidance change", result.Stderr)
	}
}

func TestRun_RefusesWeightChange(t *testing.T) {
	const head = `{
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 5, "guidance": "Any gate whose failure path emits the success value is a finding."}
  ]
}`
	fixture := newRubricFixture(t, head)
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if *calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0", *calls)
	}
	if result.Exit != 2 || result.Class != RubricRegression {
		t.Fatalf("Exit/Class = %d/%q, want 2/%q", result.Exit, result.Class, RubricRegression)
	}
	if !strings.Contains(result.Stderr, string(RubricWeightChanged)) {
		t.Errorf("stderr %q does not report a weight change", result.Stderr)
	}
}

func TestRun_AllowsCriterionAddition(t *testing.T) {
	// The change gt-nj23.5 actually wanted: keep every criterion, append one.
	const head = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Any gate whose failure path emits the success value is a finding."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`
	fixture := newRubricFixture(t, head)
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if *calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (an addition is reviewed, not refused)", *calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note != nil && result.Note.RubricRetirement {
		t.Error("Note.RubricRetirement = true for an addition, want false")
	}
}

func TestRun_RetirementLabelLetsRemovalThrough(t *testing.T) {
	const head = `{
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."}
  ]
}`
	fixture := newRubricFixture(t, head)
	deps, calls := reviewDeps(t, fixture)
	req := fixture.request()
	req.RubricRetirement = true

	result := Run(context.Background(), req, deps)

	if *calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (a marked retirement is reviewed)", *calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if result.Note == nil || !result.Note.RubricRetirement {
		t.Fatalf("Note = %+v, want RubricRetirement recorded on the approved note", result.Note)
	}
	gotNote, err := ReadNote(deps.Git, fixture.head)
	if err != nil {
		t.Fatalf("ReadNote: %v", err)
	}
	if !gotNote.RubricRetirement {
		t.Error("git note RubricRetirement = false, want true (the retirement must survive the MR bead)")
	}
}

// TestRun_RefusesCriterionRemovalInRehearsedMR runs the guard the way the
// refinery does: a real origin, the MR branch pushed, no RehearsedHead, so Run
// rehearses it onto origin/main itself. The manifest pins main's rubric and the
// refinery clone sits on main, so AssertVersion passes — the exact shape in
// which gt-wisp-6vz's criterion deletion reached a green gate.
func TestRun_RefusesCriterionRemovalInRehearsedMR(t *testing.T) {
	fakeBDForReview(t)

	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	repoDir := initTestRepo(t)
	g := git.NewGit(repoDir)
	base := commitFileReview(t, repoDir, ".om.json", rubricBaseJSON, "add rubric")
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote: %v", err)
	}
	renameCmd := exec.Command("git", "branch", "-M", "main")
	renameCmd.Dir = repoDir
	if out, err := renameCmd.CombinedOutput(); err != nil {
		t.Fatalf("rename branch to main: %v\n%s", err, out)
	}
	if err := g.Push("origin", "main", false); err != nil {
		t.Fatalf("push main: %v", err)
	}

	branch := "polecat/marble/gt-repro"
	checkoutCmd := exec.Command("git", "checkout", "-b", branch, base)
	checkoutCmd.Dir = repoDir
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout %s: %v\n%s", branch, err, out)
	}
	// The swap gt-2oi0 was filed about: fail-open-branch out, docs-and-comments
	// in, weight for weight.
	const head = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`
	commitFileReview(t, repoDir, ".om.json", head, "replace a criterion")
	if err := g.Push("origin", branch, false); err != nil {
		t.Fatalf("push %s: %v", branch, err)
	}
	if err := g.Checkout("main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	rigDir := t.TempDir()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	binSum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	// main's rubric, not the MR's: this is the deployed rubric the rig's
	// manifest pins while the clone sits on main.
	rubricSum := sha256.Sum256([]byte(rubricBaseJSON))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(binSum[:])
	m.OMBinary.Version = "1.4.0"
	m.Rubric.Path = ".om.json"
	m.Rubric.SHA256 = hex.EncodeToString(rubricSum[:])
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store := newReviewStore(mrIssue("gt-mr-1", branch, "main", "gt-repro", "gastown", "marble"))
	calls := 0
	deps := Deps{
		Git:      g,
		Beads:    beads.NewWithStore(repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, _ string) (string, int, error) {
			calls++
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), ReviewRequest{
		RigDir:  rigDir,
		RepoDir: repoDir,
		MRID:    "gt-mr-1",
		Worker:  "marble",
		Rig:     "gastown",
		Target:  "main",
		Branch:  branch,
		Attempt: 1,
		Config:  config.EditorialConfig{Required: true},
	}, deps)

	if calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0: the guard must refuse before review", calls)
	}
	if result.Exit != 2 || result.Class != RubricRegression {
		t.Fatalf("Exit/Class = %d/%q, want 2/%q (stderr=%q)", result.Exit, result.Class, RubricRegression, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "fail-open-branch") {
		t.Errorf("stderr %q does not name the removed criterion", result.Stderr)
	}
}

// TestRun_RubricUntouchedIsNotRefused: an MR that leaves .om.json alone must
// reach the gate script exactly as before, including when the manifest declares
// a rubric the MR never touches.
func TestRun_RubricUntouchedIsNotRefused(t *testing.T) {
	fixture := newRubricFixture(t, rubricBaseJSON)
	commitFileReview(t, fixture.repoDir, "feature.txt", "hello\n", "add feature")
	g := git.NewGit(fixture.repoDir)
	head, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	deps, calls := reviewDeps(t, fixture)
	req := fixture.request()
	req.RehearsedHead = head

	result := Run(context.Background(), req, deps)

	if *calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1", *calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
}

// TestRun_RubricTouchDoesNotFalsePositiveVersionMismatch is the gt-7bvf
// repro: the single-review CLI path checks RepoDir out onto the reviewed
// head before invoking Run, so a branch that touches .om.json leaves
// RepoDir's working tree holding the branch's OWN proposed rubric, never
// what the manifest (correctly) pins — the deployed one. Before the fix,
// AssertVersion hashed that working tree and every such review failed
// version_mismatch before the gate ever ran, for a completely valid change.
func TestRun_RubricTouchDoesNotFalsePositiveVersionMismatch(t *testing.T) {
	const headRubric = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Any gate whose failure path emits the success value is a finding."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`
	fixture := newRubricFixture(t, headRubric)
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if result.Class == VersionMismatch {
		t.Fatalf("Class = %q (stderr=%q): a diff touching .om.json must assert against the target's committed rubric, not RepoDir's working tree", result.Class, result.Stderr)
	}
	if *calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (an addition is reviewed, not refused)", *calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
}

// TestRun_RubricTouchStillFailsClosedOnStaleManifest pins the other half of
// the fix: routing the assertion onto the target's own committed rubric must
// still fail closed when that content genuinely does not match what the
// manifest records (e.g. deploy.sh has not re-run since a legitimate rubric
// change landed) — AssertVersionWithRubric must refuse exactly like
// AssertVersion does, not defer silently to whatever origin/target holds.
func TestRun_RubricTouchStillFailsClosedOnStaleManifest(t *testing.T) {
	const headRubric = `{
  "threshold": 0.6,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Any gate whose failure path emits the success value is a finding."},
    {"name": "docs-and-comments", "weight": 2, "guidance": "Docs follow the writing rules."}
  ]
}`
	fixture := newRubricFixture(t, headRubric)
	m, err := LoadManifest(fixture.rigDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	m.Rubric.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	deps, calls := reviewDeps(t, fixture)

	result := Run(context.Background(), fixture.request(), deps)

	if *calls != 0 {
		t.Fatalf("gate script invoked %d times, want 0 (version_mismatch caught before invoke)", *calls)
	}
	if result.Exit != 2 || result.Class != VersionMismatch {
		t.Fatalf("Exit/Class = %d/%q, want 2/%q", result.Exit, result.Class, VersionMismatch)
	}
}

// TestRun_ThresholdLoweringMRIsReviewedAgainstTargetsThreshold is the other
// documented test for gt-7bvf: DiffRubricAt only compares the rubric
// CRITERIA array, so a branch that lowers "threshold" alone (leaving every
// criterion unchanged) carries no delta for that guard to refuse. The
// fail-closed guarantee has to come from where the review itself runs — the
// gate script's cwd must be the target's own rubric, never the branch's, so
// om grades against the OLD threshold regardless of what the branch's
// .om.json requests.
func TestRun_ThresholdLoweringMRIsReviewedAgainstTargetsThreshold(t *testing.T) {
	const headRubric = `{
  "threshold": 0.1,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Any gate whose failure path emits the success value is a finding."}
  ]
}`
	fixture := newRubricFixture(t, headRubric)
	fakeBDForReview(t)
	store := newReviewStore(mrIssue("gt-mr-1", fixture.request().Branch, "main", "gt-real", "gastown", "marble"))
	calls := 0
	var gotDir string
	var gotRubric []byte
	var readErr error
	deps := Deps{
		Git:      git.NewGit(fixture.repoDir),
		Beads:    beads.NewWithStore(fixture.repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, dir string) (string, int, error) {
			calls++
			gotDir = dir
			// Read here, not after Run returns: Run removes this worktree in
			// its own deferred cleanup before it returns.
			gotRubric, readErr = os.ReadFile(filepath.Join(dir, ".om.json"))
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}

	result := Run(context.Background(), fixture.request(), deps)

	if calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (the criteria are unchanged; the threshold alone is not this guard's own job to refuse)", calls)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if gotDir == fixture.repoDir {
		t.Fatal("gate script ran with cwd = req.RepoDir; a diff touching .om.json must never be reviewed against the branch's own working tree")
	}
	if readErr != nil {
		t.Fatalf("read reviewed cwd's rubric: %v", readErr)
	}
	if !strings.Contains(string(gotRubric), `"threshold": 0.6`) {
		t.Errorf("gate script's cwd carries .om.json %s, want the target's own (threshold 0.6), not the branch's lowered one", gotRubric)
	}
}

// TestRun_LandedRubricTouchGradesUnderPreChangeRubric is the landed/retro
// half of the mayor's gt-7bvf design decision. Once a rubric-touching commit
// lands, it is already on origin/<target>'s first-parent chain, so reading
// "the target's rubric" from origin/<target> itself — as the non-landed path
// correctly does — would grade the change under the very rubric it just
// introduced. A landed review must instead read the PRE-change rubric: the
// landed range's Base, the state before this diff landed. The manifest here
// still pins the base (pre-change) rubric, exactly what it holds right after
// this commit lands: post-merge no longer restamps automatically (see
// mq.go's detectRubricChangeAfterMerge), so AssertVersionWithRubric passing
// against Base is the expected steady state, not a coincidence of the test.
func TestRun_LandedRubricTouchGradesUnderPreChangeRubric(t *testing.T) {
	fakeBDForReview(t)
	repoDir := initTestRepo(t)
	commitFileReview(t, repoDir, ".om.json", rubricBaseJSON, "add rubric")
	g := git.NewGit(repoDir)
	base, err := g.Rev("HEAD")
	if err != nil {
		t.Fatalf("rev HEAD: %v", err)
	}
	updateRef := func(ref, commit string) {
		t.Helper()
		cmd := exec.Command("git", "update-ref", ref, commit)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("update-ref %s %s: %v\n%s", ref, commit, err, out)
		}
	}
	updateRef("refs/remotes/origin/main", base)

	// A real origin remote so the post-approve note push has somewhere to go.
	bareDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", bareDir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare origin: %v\n%s", err, out)
	}
	if _, err := g.AddRemote("origin", bareDir); err != nil {
		t.Fatalf("add remote origin: %v", err)
	}

	// The landed commit lowers the threshold alone, leaving every criterion
	// unchanged — DiffRubricAt has nothing to refuse here (it only compares
	// the criteria array), so the only thing that can catch a self-graded
	// threshold drop is where the review itself runs.
	const landedRubric = `{
  "threshold": 0.1,
  "rubric": [
    {"name": "correctness", "weight": 3, "guidance": "Logic errors outrank all else."},
    {"name": "fail-open-branch", "weight": 2, "guidance": "Any gate whose failure path emits the success value is a finding."}
  ]
}`
	landed := commitFileReview(t, repoDir, ".om.json", landedRubric, "lower threshold")

	// This commit already landed on main: move origin/main's ref onto it —
	// the shape gt mq review --landed sees for a fast-forward landing.
	updateRef("refs/remotes/origin/main", landed)

	rng, err := ResolveLandedRange(g, landed, "main")
	if err != nil {
		t.Fatalf("ResolveLandedRange: %v", err)
	}

	rigDir := t.TempDir()
	binPath := filepath.Join(rigDir, "om-stub-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho om\n"), 0755); err != nil {
		t.Fatalf("write om stub binary: %v", err)
	}
	binSum := sha256.Sum256([]byte("#!/bin/sh\necho om\n"))
	rubricSum := sha256.Sum256([]byte(rubricBaseJSON))
	var m Manifest
	m.OMBinary.Path = binPath
	m.OMBinary.SHA256 = hex.EncodeToString(binSum[:])
	m.OMBinary.Version = "1.4.0"
	m.Rubric.Path = ".om.json"
	m.Rubric.SHA256 = hex.EncodeToString(rubricSum[:])
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, manifestFileName), data, 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	store := newReviewStore() // no MR bead: a landed review has none
	var gotDir string
	var gotRubric []byte
	var readErr error
	calls := 0
	deps := Deps{
		Git:      g,
		Beads:    beads.NewWithStore(repoDir, store),
		Recorder: plugin.NewRecorder(t.TempDir()),
		Exec: func(_ context.Context, _ string, args []string, dir string) (string, int, error) {
			calls++
			gotDir = dir
			// Read here, not after Run returns: Run removes this worktree in
			// its own deferred cleanup before it returns.
			gotRubric, readErr = os.ReadFile(filepath.Join(dir, ".om.json"))
			writeVerdict(t, verdictPathFromArgs(args), verdictJSON{Score: 0.8, Verdict: "approve"})
			return "", 0, nil
		},
	}

	req := ReviewRequest{
		RigDir:  rigDir,
		RepoDir: repoDir,
		Rig:     "gastown",
		Target:  "main",
		Landed:  &rng,
		Attempt: 1,
		Config:  config.EditorialConfig{Required: true},
	}

	result := Run(context.Background(), req, deps)

	if result.Class == VersionMismatch {
		t.Fatalf("Class = %q (stderr=%q): the manifest's pre-change rubric sha must match the landed range's Base content", result.Class, result.Stderr)
	}
	if calls != 1 {
		t.Fatalf("gate script invoked %d times, want 1 (stderr=%q class=%q)", calls, result.Stderr, result.Class)
	}
	if result.Exit != 0 {
		t.Fatalf("Exit = %d, want 0 (stderr=%q class=%q)", result.Exit, result.Stderr, result.Class)
	}
	if readErr != nil {
		t.Fatalf("read reviewed cwd's rubric: %v", readErr)
	}
	if gotDir == repoDir {
		t.Fatal("gate script ran with cwd = req.RepoDir; a landed diff touching .om.json must run in a fresh worktree, never RepoDir's own checkout")
	}
	if !strings.Contains(string(gotRubric), `"threshold": 0.6`) {
		t.Errorf("gate script's cwd carries .om.json %s, want the PRE-change rubric (threshold 0.6) — origin/main already carries the landed (lowered) one, and reading it there would self-grade", gotRubric)
	}
}
