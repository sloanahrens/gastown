package land

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// riskFixture is an origin whose main carries a riskpaths list and a work
// branch that changes files, in a gitfake world (the unit tier refuses git on
// PATH).
type riskFixture struct {
	t      *testing.T
	git    *gitfake.Fake
	origin string
	base   string
	head   string
}

// newRiskFixture builds main with a.txt and the risk list, and a branch from
// it that changes the files named.
func newRiskFixture(t *testing.T, list string, changed map[string]string) *riskFixture {
	t.Helper()
	root := t.TempDir()
	f := &riskFixture{t: t, git: gitfake.New(), origin: filepath.Join(root, "origin.git")}
	f.git.InitBare(t, f.origin)
	main := map[string]string{"a.txt": "one\n"}
	if list != "" {
		main[RiskPathsFile] = list
	}
	f.base = f.git.Commit(t, f.origin, "main", "main: seed", main)
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	f.head = f.git.Commit(t, f.origin, fixtureBranch, "feat: work", changed)
	return f
}

func (f *riskFixture) repo() Repo { return f.git.Open(f.origin) }

// RiskPaths returns the changed paths from main that the base's list matches:
// a subtree glob, an exact path and a per-segment wildcard, sorted and
// without the paths the list does not name.
func TestRiskPathsMatchesBaseList(t *testing.T) {
	t.Parallel()
	f := newRiskFixture(t, "internal/daemon/**\nMakefile\ninternal/cmd/tap_guard*.go\n", map[string]string{
		"internal/daemon/attention.go":   "x\n",
		"internal/daemon/deep/other.go":  "x\n",
		"Makefile":                       "y\n",
		"internal/cmd/tap_guard_test.go": "z\n",
		"README.md":                      "not on the list\n",
	})
	got, err := RiskPaths(f.repo(), f.base, f.head)
	if err != nil {
		t.Fatalf("RiskPaths: %v", err)
	}
	want := []string{"Makefile", "internal/cmd/tap_guard_test.go", "internal/daemon/attention.go", "internal/daemon/deep/other.go"}
	if !slices.Equal(got, want) {
		t.Errorf("RiskPaths = %q, want %q", got, want)
	}
}

// The list is read from the base commit, so a submission cannot unlist its own
// paths: a head that empties riskpaths.txt while changing a risk path is still
// labelled.
func TestRiskPathsReadsTheBaseListNotTheHead(t *testing.T) {
	t.Parallel()
	f := newRiskFixture(t, "internal/daemon/**\n", map[string]string{
		"internal/daemon/attention.go": "x\n",
		RiskPathsFile:                  "# emptied by the submission\n",
	})
	got, err := RiskPaths(f.repo(), f.base, f.head)
	if err != nil {
		t.Fatalf("RiskPaths: %v", err)
	}
	if !slices.Equal(got, []string{"internal/daemon/attention.go"}) {
		t.Errorf("RiskPaths = %q, want the base's list to still flag the change", got)
	}
}

// A base with no riskpaths.txt at all labels nothing, and neither does a
// change the list does not name.
func TestRiskPathsMissingFileAndUnmatchedChange(t *testing.T) {
	t.Parallel()
	f := newRiskFixture(t, "", map[string]string{"internal/daemon/attention.go": "x\n"})
	if got, err := RiskPaths(f.repo(), f.base, f.head); err != nil || len(got) != 0 {
		t.Fatalf("RiskPaths with no file = %q, %v; want none", got, err)
	}

	g := newRiskFixture(t, "internal/daemon/**\n", map[string]string{"docs/readme.md": "x\n"})
	if got, err := RiskPaths(g.repo(), g.base, g.head); err != nil || len(got) != 0 {
		t.Fatalf("RiskPaths with an unmatched change = %q, %v; want none", got, err)
	}
}

// Matching is path.Match per segment with a trailing /** for a subtree: '*'
// stops at a separator, '**' is only a subtree at the end, and a bare name is
// the whole path.
func TestRiskPathsGlobMatching(t *testing.T) {
	t.Parallel()
	cases := []struct {
		glob    string
		path    string
		matched bool
	}{
		{"internal/daemon/**", "internal/daemon/a.go", true},
		{"internal/daemon/**", "internal/daemon/sub/deep/a.go", true},
		{"internal/daemon/**", "internal/dolt/a.go", false},
		{"internal/cmd/tap_guard*.go", "internal/cmd/tap_guard.go", true},
		{"internal/cmd/tap_guard*.go", "internal/cmd/tap_guard_x_test.go", true},
		{"internal/cmd/tap_guard*.go", "internal/cmd/slot_test.go", false},
		// '*' does not cross a separator: the pattern's last segment cannot
		// absorb another directory.
		{"internal/cmd/*.go", "internal/cmd/a.go", true},
		{"internal/cmd/*.go", "internal/cmd/sub/a.go", false},
		{"Makefile", "Makefile", true},
		{"Makefile", "docs/Makefile", false},
		// A malformed glob matches nothing rather than failing the landing.
		{"internal/[daemon/**", "internal/daemon/a.go", false},
	}
	for _, c := range cases {
		if got := matchRiskGlob(c.glob, c.path); got != c.matched {
			t.Errorf("matchRiskGlob(%q, %q) = %v, want %v", c.glob, c.path, got, c.matched)
		}
	}
}

// ParseRiskPathGlobs keeps one glob per line and drops comments and blanks.
func TestRiskPathsParseCommentsAndBlanks(t *testing.T) {
	t.Parallel()
	got := ParseRiskPathGlobs("# a comment\n\n  internal/daemon/**  \nMakefile\n")
	want := []string{"internal/daemon/**", "Makefile"}
	if !slices.Equal(got, want) {
		t.Errorf("ParseRiskPathGlobs = %q, want %q", got, want)
	}
}

// RiskPathsRecorded drops the paths of a bead the review machinery filed —
// and only those, and only when the bead touched a risk path: the labels
// decide, and a title that reads like a review's finding is not a label.
func TestRiskPathsRecorded(t *testing.T) {
	t.Parallel()
	paths := []string{"internal/daemon/x.go"}
	for _, tc := range []struct {
		name       string
		title      string
		labels     []string
		paths      []string
		wantPaths  []string
		wantSkipBy string
		nilIssue   bool
	}{
		{name: "an ordinary bead keeps its paths", labels: []string{LabelReadyToLand}, paths: paths, wantPaths: paths},
		{name: "review-deep skips", title: "fix: the finding", labels: []string{LabelReadyToLand, reviewDeepLabel}, paths: paths, wantSkipBy: reviewDeepLabel},
		{name: "om-followup skips", labels: []string{FollowUpLabel}, paths: paths, wantSkipBy: FollowUpLabel},
		{name: "a review-generated bead that touched no risk path has no review to skip", labels: []string{reviewDeepLabel}},
		{name: "no issue keeps its paths", nilIssue: true, paths: paths, wantPaths: paths},
		{name: "a title that reads review-generated is not a label", title: "fix: the finding the deep review raised", paths: paths, wantPaths: paths},
		{name: "another label is not review-generated", labels: []string{LabelRework, LabelNeedsHuman}, paths: paths, wantPaths: paths},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := &beads.Issue{ID: "gt-abc", Title: tc.title, Labels: tc.labels}
			if tc.nilIssue {
				issue = nil
			}
			got, skippedBy := RiskPathsRecorded(issue, tc.paths)
			if !slices.Equal(got, tc.wantPaths) || skippedBy != tc.wantSkipBy {
				t.Errorf("RiskPathsRecorded = %q, %q; want %q, %q", got, skippedBy, tc.wantPaths, tc.wantSkipBy)
			}
		})
	}
}

// Since returns the records landed in the window, oldest first.
func TestLandingsSinceWindow(t *testing.T) {
	t.Parallel()
	lf, err := RigLandingsFile(t.TempDir(), "gastown")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{now.Add(-8 * 24 * time.Hour), now.Add(-time.Hour), now} {
		rec := LandingRecord{BeadID: "gt-" + string(rune('a'+i)), LandedAt: at, RiskPaths: []string{"internal/daemon/a.go"}}
		if err := lf.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := lf.Since(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].LandedAt.After(got[1].LandedAt) {
		t.Fatalf("Since = %+v, want the two records inside the window, oldest first", got)
	}
}

// A risk-path landing still lands: it carries the label and writes the paths
// to its record, and the ready label comes off in the same update.
func TestRiskPathsLandingStillLandsAndLabels(t *testing.T) {
	t.Parallel()
	f := newLandFixtureAt(t)
	f.git = gitfake.New()
	f.git.InitBare(t, f.origin)
	f.base = f.git.Commit(t, f.origin, "main", "main: seed", map[string]string{
		"a.txt": "one\n", RiskPathsFile: "internal/daemon/**\n",
	})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	head := f.git.Commit(t, f.origin, fixtureBranch, "feat: touch a risk path", map[string]string{"internal/daemon/x.go": "x\n"})
	f.git.Clone(t, f.origin, f.repo)
	f.ready(head)

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !slices.Equal(res.RiskPaths, []string{"internal/daemon/x.go"}) {
		t.Errorf("Result.RiskPaths = %q, want the changed risk path", res.RiskPaths)
	}
	bead := f.bead()
	if !beads.HasLabel(bead, LabelOverseerReviewWanted) {
		t.Errorf("labels = %v, want %s", bead.Labels, LabelOverseerReviewWanted)
	}
	if beads.HasLabel(bead, LabelReadyToLand) {
		t.Errorf("labels = %v, want no %s", bead.Labels, LabelReadyToLand)
	}
	if strings.Contains(bead.Notes, RiskPathReviewSkippedLine) {
		t.Errorf("notes carry %q on a bead nothing review-generated filed:\n%s", RiskPathReviewSkippedLine, bead.Notes)
	}
	if bead.Status != "closed" {
		t.Errorf("status = %q, want the landing to close it", bead.Status)
	}
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %v, want one", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.RiskPaths, []string{"internal/daemon/x.go"}) {
		t.Errorf("record risk_paths = %q, want the changed risk path", rec.RiskPaths)
	}
}

// A landing of a bead the review machinery filed records no risk path, so
// neither the attention queue's landing.risk_path item nor the deep review's
// landings mode reads it as one, and the note says the review the paths would
// have asked for was skipped (gt-g349w).
func TestReviewGeneratedLandingSkipsRiskPathReview(t *testing.T) {
	t.Parallel()
	for _, label := range []string{reviewDeepLabel, FollowUpLabel} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			f := reviewGeneratedRiskPathFixture(t, label)

			res, err := f.lander().Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("Land: %v", err)
			}
			assertRiskPathReviewSkipped(t, f, res, label)
		})
	}
}

// The recovery that finishes a landing whose record was never written reads
// the bead's labels too: a review-generated bead whose merge took but whose
// pass died before the record still lands with no risk path (gt-g349w).
func TestReviewGeneratedUnrecordedLandingSkipsRiskPathReview(t *testing.T) {
	t.Parallel()
	f := reviewGeneratedRiskPathFixture(t, reviewDeepLabel)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	f.merger.fn = nil

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the landing: %v", err)
	}
	if res.LandedCommit != merged || res.Verdict.Verdict != VerdictUnrecorded {
		t.Fatalf("Result = %s, verdict %q; want the recovery of merge %s", res.LandedCommit, res.Verdict.Verdict, merged)
	}
	assertRiskPathReviewSkipped(t, f, res, reviewDeepLabel)
}

// reviewGeneratedRiskPathFixture is a fixture whose work touches a risk path
// and whose bead carries label.
func reviewGeneratedRiskPathFixture(t *testing.T, label string) *landFixture {
	t.Helper()
	f := newLandFixtureAt(t)
	f.git = gitfake.New()
	f.git.InitBare(t, f.origin)
	f.base = f.git.Commit(t, f.origin, "main", "main: seed", map[string]string{
		"a.txt": "one\n", RiskPathsFile: "internal/daemon/**\n",
	})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	head := f.git.Commit(t, f.origin, fixtureBranch, "fix: the finding the review raised",
		map[string]string{"internal/daemon/tap_guard.go": "x\n"})
	f.git.Clone(t, f.origin, f.repo)
	f.ready(head)
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{label}}); err != nil {
		t.Fatalf("labelling the bead %s: %v", label, err)
	}
	return f
}

// assertRiskPathReviewSkipped checks a landing of a review-generated risk-path
// bead: no risk path in the result or the landings file, no
// gt:overseer-review-wanted, and the skip and the label behind it named in the
// result, the note and the landings file.
func assertRiskPathReviewSkipped(t *testing.T, f *landFixture, res Result, label string) {
	t.Helper()
	if len(res.RiskPaths) != 0 || res.RiskPathReviewSkippedBy != label {
		t.Errorf("Result = %q, skipped by %q; want no risk path, skipped by %q", res.RiskPaths, res.RiskPathReviewSkippedBy, label)
	}
	bead := f.bead()
	if beads.HasLabel(bead, LabelOverseerReviewWanted) {
		t.Errorf("labels = %v, want no %s", bead.Labels, LabelOverseerReviewWanted)
	}
	wantLine := RiskPathReviewSkippedLine + " (label " + label + ")"
	if !strings.Contains(bead.Notes, wantLine) {
		t.Errorf("notes lack %q:\n%s", wantLine, bead.Notes)
	}
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landing records = %v, want one", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.RiskPaths) != 0 || rec.RiskPathReviewSkippedBy != label {
		t.Errorf("record = %q, skipped by %q; want no risk path, skipped by %q", rec.RiskPaths, rec.RiskPathReviewSkippedBy, label)
	}
}

// A landing that touches nothing on the list is unlabelled.
func TestOrdinaryLandingIsNotLabelled(t *testing.T) {
	t.Parallel()
	f := newLandFixtureAt(t)
	f.git = gitfake.New()
	f.git.InitBare(t, f.origin)
	f.base = f.git.Commit(t, f.origin, "main", "main: seed", map[string]string{
		"a.txt": "one\n", RiskPathsFile: "internal/daemon/**\n",
	})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	head := f.git.Commit(t, f.origin, fixtureBranch, "feat: docs", map[string]string{"docs/x.md": "x\n"})
	f.git.Clone(t, f.origin, f.repo)
	f.ready(head)

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(res.RiskPaths) != 0 {
		t.Errorf("Result.RiskPaths = %q, want none", res.RiskPaths)
	}
	if beads.HasLabel(f.bead(), LabelOverseerReviewWanted) {
		t.Errorf("labels = %v, want no %s on an ordinary landing", f.bead().Labels, LabelOverseerReviewWanted)
	}
}

// A non-ASCII path under a subtree glob is a risk path. git C-quotes such a
// name in diff --name-only output; the quoted form begins with a quote and so
// never matched internal/daemon/** (the real-git half is in
// riskpaths_integration_test.go).
func TestRiskPathsMatchesNonASCIIPath(t *testing.T) {
	t.Parallel()
	f := newRiskFixture(t, "internal/daemon/**\n", map[string]string{
		"internal/daemon/café.go": "x\n",
		"README.md":               "y\n",
	})
	got, err := RiskPaths(f.repo(), f.base, f.head)
	if err != nil {
		t.Fatalf("RiskPaths: %v", err)
	}
	if !slices.Equal(got, []string{"internal/daemon/café.go"}) {
		t.Errorf("RiskPaths = %q, want the non-ASCII path", got)
	}
}
