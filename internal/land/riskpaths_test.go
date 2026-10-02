package land

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
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
