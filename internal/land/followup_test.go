package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// followupFake is the feed's beads surface in memory: what a landing
// de-duplicates against, and what it filed.
type followupFake struct {
	existing  []ExistingFollowup
	created   []FollowupSpec
	listErr   error
	createErr error
}

func (f *followupFake) ListFollowups(string) ([]ExistingFollowup, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]ExistingFollowup(nil), f.existing...), nil
}

func (f *followupFake) CreateFollowup(spec FollowupSpec) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	// One landing can file several beads, and a bead's id is what a comment
	// names, so the fake numbers them past the first.
	id := "gt-filed"
	if n := len(f.created); n > 0 {
		id = fmt.Sprintf("gt-filed-%d", n+1)
	}
	f.created = append(f.created, spec)
	// A filed bead is one the rest of the run can see, so a second pass over
	// the same landing de-duplicates against it.
	f.existing = append(f.existing, ExistingFollowup{ID: id, Title: spec.Title,
		Description: spec.Body, Status: "open", Labels: spec.Labels})
	return id, nil
}

// followupFixture is one approved landing whose feed runs: a worktree carrying
// the rig's om threshold, the beads store the beads live in, and the fake the
// feed files through.
type followupFixture struct {
	t      *testing.T
	lander *Lander
	fake   *followupFake
	bd     *beadsfake.Fake
	dir    string
	work   Work
	result Result
}

const followupThreshold = 0.6

func newFollowupFixture(t *testing.T, score float64, findings []Finding) *followupFixture {
	t.Helper()
	dir := t.TempDir()
	// The band's floor is the rig's own .om.json threshold, read from the tree
	// the landing ran in.
	if err := os.WriteFile(filepath.Join(dir, ".om.json"), []byte(`{"threshold": 0.6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(
		beads.Issue{ID: "gt-abc", Title: "land the thing", Status: "open", Type: "task"},
		beads.Issue{ID: "gt-old", Title: "an older finding", Status: "open", Type: "task"},
	)
	fake := &followupFake{}
	return &followupFixture{
		t: t, bd: bd, fake: fake, dir: dir,
		lander: &Lander{Beads: bd, Followups: fake},
		work:   Work{BeadID: "gt-abc", Rig: "gastown", Branch: fixtureBranch, Head: "aaa1111", Target: "main"},
		result: Result{LandedCommit: "0123456789abcdef0123456789abcdef01234567",
			Verdict: Verdict{Verdict: VerdictApprove, Score: score, Findings: findings}},
	}
}

// run feeds the fixture's landing and returns what the feed logged.
func (f *followupFixture) run() string {
	f.t.Helper()
	var log strings.Builder
	f.lander.Out = &log
	f.lander.fileFollowups(f.dir, f.work, f.result)
	return log.String()
}

// majorFinding is a finding the follow-up feed does not file: a major refuses
// the landing it was raised on (gt-oi4je).
func majorFinding(title string) Finding {
	return Finding{ID: "1", Severity: "major", Path: "internal/land/note.go", Line: 230, Title: title}
}

func minorFinding(title string) Finding {
	return Finding{ID: "2", Severity: "minor", Path: "internal/land/note.go", Line: 260, Title: title}
}

// minorAt is a minor finding in a named file, for the cases that split a
// landing's findings by the package they land in.
func minorAt(fpath string, line int, title string) Finding {
	return Finding{ID: "3", Severity: "minor", Path: fpath, Line: line, Title: title}
}

// TestFollowupFeedHasNoUpperCap: a landing om approved at or above the rig's own
// threshold earns a follow-up whatever it scored. The review stays strict
// because every minor it raises is written down, not because a high score buys
// silence (gt-4ty22).
func TestFollowupFeedHasNoUpperCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		score float64
		want  int
	}{
		{"below the rig's floor", 0.59, 0},
		{"at the rig's floor", followupThreshold, 1},
		{"inside the band", 0.8, 1},
		{"at the score the band used to stop at", 0.9, 1},
		{"a landing approved at 0.93", 0.93, 1},
		{"above the old cap", 0.95, 1},
		{"a perfect score", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, tc.score, []Finding{minorFinding("name the error")})
			f.run()
			if len(f.fake.created) != tc.want {
				t.Fatalf("filed %d beads at score %.2f, want %d", len(f.fake.created), tc.score, tc.want)
			}
		})
	}
}

// TestFollowupFeedBeadShape: the bead one landing files names the finding and
// what raised it, carries the label the next landing de-duplicates on plus one
// fingerprint per finding, and sorts below the queue's own work (gt-s01pn).
func TestFollowupFeedBeadShape(t *testing.T) {
	t.Parallel()
	findings := []Finding{minorAt("internal/land/note.go", 230, "swap the nil check"), minorFinding("name the error")}
	f := newFollowupFixture(t, 0.8, findings)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one:\n%s", len(f.fake.created), log)
	}
	spec := f.fake.created[0]
	wantTitle := "Found: swap the nil check and 1 more (om minor on gt-abc approved at 0.80)"
	if spec.Title != wantTitle {
		t.Errorf("title = %q, want %q", spec.Title, wantTitle)
	}
	want := []string{FollowUpLabel,
		Fingerprint("internal/land/note.go", "swap the nil check"),
		Fingerprint("internal/land/note.go", "name the error")}
	if !slices.Equal(spec.Labels, want) {
		t.Errorf("labels = %v, want %v", spec.Labels, want)
	}
	if spec.Priority != followUpPriority {
		t.Errorf("priority = %d, want %d", spec.Priority, followUpPriority)
	}
	for _, want := range []string{"internal/land/note.go:230", "internal/land/note.go:260", "gt-abc", f.result.LandedCommit} {
		if !strings.Contains(spec.Body, want) {
			t.Errorf("body does not name %q:\n%s", want, spec.Body)
		}
	}
	if !strings.Contains(log, "filed gt-filed for 2 findings") {
		t.Errorf("log = %q, want the file named", log)
	}
}

// TestFollowupFeedCommentsTheFiledBeadsOntoTheWorkBead: the landed work bead is
// where a reader finds what its minors became, so the feed names the beads it
// filed there (gt-4ty22).
func TestFollowupFeedCommentsTheFiledBeadsOntoTheWorkBead(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	comments, err := f.bd.Comments("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "follow-up filed for this landing: gt-filed (om approved at 0.80)"
	if len(comments) != 1 || strings.TrimSpace(comments[0].Text) != want {
		t.Fatalf("comments = %v, want %q", commentTexts(comments), want)
	}
}

// TestFollowupFeedSplitsAFullLandingByPackage: a landing carrying findings in
// more than one package files one bead per package, each shaped for the
// package's own findings, because that is the work one worker takes (gt-4ty22).
func TestFollowupFeedSplitsAFullLandingByPackage(t *testing.T) {
	t.Parallel()
	findings := []Finding{
		minorAt("internal/land/note.go", 1, "a"),
		minorAt("internal/land/followup.go", 2, "b"),
		minorAt("internal/land/note.go", 3, "c"),
		minorAt("internal/daemon/landing_worker.go", 4, "d"),
		minorAt("internal/daemon/landing_worker.go", 5, "e"),
		minorAt("docs/reference.md", 6, "f"),
		minorAt("docs/reference.md", 7, "g"),
		minorAt("docs/reference.md", 8, "h"),
	}
	f := newFollowupFixture(t, 0.8, findings)
	log := f.run()
	if len(f.fake.created) != 3 {
		t.Fatalf("filed %d beads, want one per package:\n%s", len(f.fake.created), log)
	}
	// Packages are filed in sorted order, so the beads are stable run to run.
	for i, tc := range []struct {
		pkg      string
		findings []Finding
	}{
		{"docs", findings[5:]},
		{"internal/daemon", findings[3:5]},
		{"internal/land", findings[:3]},
	} {
		spec := f.fake.created[i]
		more := ""
		if n := len(tc.findings) - 1; n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		wantTitle := fmt.Sprintf("Found: %s%s in %s (om minor on gt-abc approved at 0.80)", tc.findings[0].Title, more, tc.pkg)
		if spec.Title != wantTitle {
			t.Errorf("bead %d title = %q, want %q", i, spec.Title, wantTitle)
		}
		wantLabels := []string{FollowUpLabel}
		for _, fd := range tc.findings {
			wantLabels = append(wantLabels, Fingerprint(fd.Path, fd.Title))
		}
		if !slices.Equal(spec.Labels, wantLabels) {
			t.Errorf("bead %d labels = %v, want %v", i, spec.Labels, wantLabels)
		}
		if !strings.Contains(spec.Body, "in "+tc.pkg) {
			t.Errorf("bead %d body does not name its package:\n%s", i, spec.Body)
		}
	}
	// Every filed bead is named on the work bead, once each.
	comments, err := f.bd.Comments("gt-abc")
	if err != nil {
		t.Fatal(err)
	}
	want := "follow-ups filed for this landing: gt-filed, gt-filed-2, gt-filed-3 (om approved at 0.80)"
	if len(comments) != 1 || strings.TrimSpace(comments[0].Text) != want {
		t.Fatalf("comments = %v, want %q", commentTexts(comments), want)
	}
}

// TestFollowupFeedTreatsARootFileAsHavingNoPackage: a finding in a file at the
// repository root has no package — path.Dir spells that "." — so its bead names
// the finding alone rather than a package called "." (gt-4ty22).
func TestFollowupFeedTreatsARootFileAsHavingNoPackage(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("README.md", 12, "name the error"),
		minorAt("internal/land/note.go", 230, "swap the nil check"),
	})
	f.run()
	if len(f.fake.created) != 2 {
		t.Fatalf("filed %d beads, want one per package", len(f.fake.created))
	}
	// Packages file in sorted order, and no package sorts before a named one.
	root := f.fake.created[0]
	if want := "Found: name the error (om minor on gt-abc approved at 0.80)"; root.Title != want {
		t.Errorf("title = %q, want %q", root.Title, want)
	}
	if strings.Contains(root.Title+root.Body, "in .") {
		t.Errorf("the bead names a package called \".\":\n%s\n%s", root.Title, root.Body)
	}
}

// TestFollowupFeedBodyLintsClean: the filed body is the shape the spec
// dispatcher dispatches, checked with its own lint rather than a copy of the
// shape kept here (gt-vpeaj, gt-s01pn).
func TestFollowupFeedBodyLintsClean(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		findings []Finding
		want     int
	}{
		{"one bead for the landing",
			[]Finding{minorAt("internal/land/note.go", 230, "swap the nil check"), minorFinding("name the error")}, 1},
		{"one bead per package",
			[]Finding{minorAt("internal/land/note.go", 230, "swap the nil check"), minorAt("docs/reference.md", 4, "name the error")}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, tc.findings)
			f.run()
			if len(f.fake.created) != tc.want {
				t.Fatalf("filed %d beads, want %d", len(f.fake.created), tc.want)
			}
			for i, filed := range f.fake.created {
				spec := specdispatch.Spec{ID: fmt.Sprintf("gt-filed-%d", i), Title: filed.Title, Type: "task",
					Status: "open", Description: filed.Body}
				if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
					t.Fatalf("the filed body does not lint clean: %s\n%s", v.Line(spec.ID), spec.Description)
				}
			}
		})
	}
}

// TestFollowupFeedFoldsAcceptancePastTheLintMaximum: a landing's findings all
// land in one bead's acceptance list, folded into the last item past the
// dispatcher's max, because no worker takes a bead with more (gt-s01pn).
func TestFollowupFeedFoldsAcceptancePastTheLintMaximum(t *testing.T) {
	t.Parallel()
	var findings []Finding
	for i := 0; i < specdispatch.MaxAcceptance+2; i++ {
		findings = append(findings, minorFinding(string(rune('a'+i))+" finding"))
	}
	f := newFollowupFixture(t, 0.8, findings)
	f.run()
	// One package, so the landing's findings stay in one bead however many
	// there are: the split is by package, not by count (gt-4ty22).
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	body := f.fake.created[0].Body
	acceptance := specdispatch.Sections(body)["acceptance"]
	if got := specdispatch.CountAcceptance(acceptance); got != specdispatch.MaxAcceptance {
		t.Errorf("acceptance items = %d, want the max %d:\n%s", got, specdispatch.MaxAcceptance, acceptance)
	}
	if !strings.Contains(acceptance, "- [ ] Finding resolved: the remaining 3 findings under Goal") {
		t.Errorf("the folded item does not name what it covers:\n%s", acceptance)
	}
	// The folded bead still lints as shaped.
	spec := specdispatch.Spec{ID: "gt-filed-0", Title: f.fake.created[0].Title, Type: "task",
		Status: "open", Description: body}
	if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
		t.Errorf("the folded body does not lint clean: %s\n%s", v.Line(spec.ID), body)
	}
	// Every finding is still named under Goal, so none is silently dropped.
	goal, _, _ := strings.Cut(body, "\n## Constraints")
	if got := strings.Count(goal, "\n- "); got != len(findings) {
		t.Errorf("Goal lists %d findings, want %d:\n%s", got, len(findings), body)
	}
}

// TestFollowupAcceptanceStatesTheFix: each acceptance box reads "No longer
// true: <title> (<file:line>)", so the criterion is the fix and a diff that
// resolves the finding satisfies it, where the finding's own headline in the
// present tense asked the worker to leave the defect in place (gt-twr9x).
func TestFollowupAcceptanceStatesTheFix(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("internal/land/note.go", 230, "swap the nil check"),
		minorAt("internal/land/note.go", 0, "name the error"),
		{ID: "4", Severity: "minor", Path: "internal/land/hook.go", Title: "document the hook"},
	})
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	acceptance := specdispatch.Sections(f.fake.created[0].Body)["acceptance"]
	want := "- [ ] Finding resolved: swap the nil check (internal/land/note.go:230)\n" +
		"- [ ] Finding resolved: name the error (internal/land/note.go)\n" +
		"- [ ] Finding resolved: document the hook (internal/land/hook.go)\n"
	if strings.TrimSpace(acceptance) != strings.TrimSpace(want) {
		t.Errorf("acceptance =\n%s\nwant\n%s", acceptance, want)
	}
}

// TestFollowupFingerprintIsUnchangedByTheAcceptanceWording: the om-fp label
// comes from the finding's path and raw title, not from the acceptance box, so
// a bead filed before the "Finding resolved:" wording and one filed after it are
// the same finding. The first half pins the label to the raw title; the second
// hands the feed a bead filed with the older, bare box and shows it is matched
// instead of filed again (gt-twr9x).
func TestFollowupFingerprintIsUnchangedByTheAcceptanceWording(t *testing.T) {
	t.Parallel()
	const path, title = "internal/land/note.go", "swap the nil check"

	filed := newFollowupFixture(t, 0.8, []Finding{minorAt(path, 230, title)})
	filed.run()
	if len(filed.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(filed.fake.created))
	}
	labels := filed.fake.created[0].Labels
	if !slices.Contains(labels, Fingerprint(path, title)) {
		t.Errorf("filed labels = %v, want them to carry the raw-title fingerprint %q", labels, Fingerprint(path, title))
	}
	if box := findingResolved + title; slices.Contains(labels, Fingerprint(path, box)) {
		t.Errorf("filed labels = %v carry the fingerprint of the acceptance box %q", labels, box)
	}

	repeat := newFollowupFixture(t, 0.8, []Finding{minorAt(path, 230, title)})
	repeat.fake.existing = []ExistingFollowup{{
		ID: "gt-old", Title: "Found: " + title, Status: "open",
		Description: "## Acceptance\n- [ ] " + title + " (" + path + ":230)\n",
		Labels:      []string{FollowUpLabel, Fingerprint(path, title)},
	}}
	repeat.run()
	if len(repeat.fake.created) != 0 {
		t.Errorf("filed %d beads for a finding a bead with the older acceptance wording already carries", len(repeat.fake.created))
	}
}

// TestFollowupFeedIgnoresInfoAndUnapprovedLandings: info is what om already
// decided is not worth fixing, and a verdict om did not reach is not an
// approval (gt-s01pn).
func TestFollowupFeedIgnoresInfoAndUnapprovedLandings(t *testing.T) {
	t.Parallel()
	t.Run("info only", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{{ID: "1", Severity: "info", Title: "consider a helper"}})
		f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads for info findings", len(f.fake.created))
		}
	})
	t.Run("a severity that is not a minor", func(t *testing.T) {
		t.Parallel()
		// A major refuses the landing rather than reaching a recorded one
		// (gt-oi4je), so this is the filter's own contract and not a case the
		// pipeline can produce.
		f := newFollowupFixture(t, 0.8, []Finding{{ID: "1", Severity: "major", Path: "b.txt", Line: 1, Title: "swap the nil check"}})
		f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads for a major finding", len(f.fake.created))
		}
	})
	for _, verdict := range []string{VerdictSkipped, VerdictErrorPrefix + "timeout", VerdictOverseerPrefix + "aaa1111"} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
			f.result.Verdict.Verdict = verdict
			f.run()
			if len(f.fake.created) != 0 {
				t.Fatalf("filed %d beads for the %q verdict", len(f.fake.created), verdict)
			}
		})
	}
}

// TestFollowupFeedSkipsAFollowUpLanding: a bead that is itself a follow-up
// files nothing, because a fix's own minors would feed the feed forever and it
// would never drain (gt-4ty22).
func TestFollowupFeedSkipsAFollowUpLanding(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	if err := f.bd.Update("gt-abc", beads.UpdateOptions{AddLabels: []string{FollowUpLabel}}); err != nil {
		t.Fatal(err)
	}
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads for a follow-up landing:\n%s", len(f.fake.created), log)
	}
	if !strings.Contains(log, "the bead is a "+FollowUpLabel+" follow-up itself") {
		t.Errorf("log = %q, want the rule named", log)
	}
}

// TestFollowupFeedCommentsARepeat: a finding an earlier landing already filed
// goes on that bead's history, and nothing new is filed for it (gt-s01pn).
func TestFollowupFeedCommentsARepeat(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
	f.fake.existing = []ExistingFollowup{{
		ID: "gt-old", Title: "swap the nil check", Status: "open",
		Description: "## Goal\nthe nil check is swapped\n",
		Labels:      []string{FollowUpLabel, Fingerprint("internal/land/note.go", "swap the nil check")},
	}}
	log := f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads for a finding already filed", len(f.fake.created))
	}
	comments, err := f.bd.Comments("gt-old")
	if err != nil {
		t.Fatal(err)
	}
	want := "seen again on gt-abc (om minor, 0.80, 01234567)"
	if len(comments) != 1 || strings.TrimSpace(comments[0].Text) != want {
		t.Fatalf("comments = %v, want %q", commentTexts(comments), want)
	}
	if strings.Contains(log, "filed") {
		t.Errorf("log claims a file: %q", log)
	}
}

// TestFollowupFeedKeepsFilingTheFreshFindingsOfARepeatLanding: one finding
// matching an existing bead does not carry the rest of the landing with it
// (gt-s01pn).
func TestFollowupFeedKeepsFilingTheFreshFindingsOfARepeatLanding(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check"), minorFinding("name the error")})
	f.fake.existing = []ExistingFollowup{{
		ID: "gt-old", Title: "swap the nil check", Status: "open",
		Labels: []string{Fingerprint("internal/land/note.go", "swap the nil check")},
	}}
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one for the fresh finding", len(f.fake.created))
	}
	want := []string{FollowUpLabel, Fingerprint("internal/land/note.go", "name the error")}
	if got := f.fake.created[0].Labels; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want only the filed finding's fingerprint", got)
	}
}

// openFollowupBeads is n open follow-up beads, which is what the backpressure
// bound counts.
func openFollowupBeads(n int) []ExistingFollowup {
	out := make([]ExistingFollowup, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ExistingFollowup{ID: "gt-open", Title: "an older finding", Status: "open"})
	}
	return out
}

// TestFollowupFeedBackpressure: the open follow-ups at the bound stop the feed,
// and the log says how many findings the landing lost to it. It is backpressure
// rather than a cap, so the count falling back under the bound resumes it, and a
// closed follow-up is not work still waiting (gt-s01pn, gt-4ty22).
func TestFollowupFeedBackpressure(t *testing.T) {
	t.Parallel()
	t.Run("at the default bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error"), minorAt("internal/land/note.go", 231, "swap the nil check")})
		f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
		}
		if !strings.Contains(log, fmt.Sprintf("%d %s beads are open, at the %d-bead backpressure bound; held back 2 findings for this landing",
			DefaultFollowupBackpressure, FollowUpLabel, DefaultFollowupBackpressure)) {
			t.Errorf("log = %q, want the bound and the held-back count named", log)
		}
	})
	t.Run("at a bound the rig configured", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.lander.FollowupBackpressure = 2
		f.fake.existing = openFollowupBeads(2)
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the configured bound", len(f.fake.created))
		}
		if !strings.Contains(log, "at the 2-bead backpressure bound") {
			t.Errorf("log = %q, want the configured bound named", log)
		}
	})
	t.Run("just under the bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.existing = append(openFollowupBeads(DefaultFollowupBackpressure-1), ExistingFollowup{ID: "gt-done", Title: "a fixed finding", Status: "closed"})
		f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads under the backpressure bound, want one", len(f.fake.created))
		}
	})
}

// TestFollowupBackpressureOrDefault: an unset bound is the feed's own default,
// and a rig that names one is read at it (gt-4ty22).
func TestFollowupBackpressureOrDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		configured int
		want       int
	}{
		{0, DefaultFollowupBackpressure},
		{-1, DefaultFollowupBackpressure},
		{1, 1},
		{7, 7},
	} {
		if got := FollowupBackpressureOrDefault(tc.configured); got != tc.want {
			t.Errorf("FollowupBackpressureOrDefault(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

// TestFollowupFeedBackpressuresEveryBead: the bound counts beads, so a landing
// that splits into one bead per package stops at it rather than carrying the
// open count past it. The findings it never filed for are named as the ones the
// bound held back (gt-4ty22).
func TestFollowupFeedBackpressuresEveryBead(t *testing.T) {
	t.Parallel()
	findings := []Finding{
		minorAt("docs/reference.md", 1, "name the error"),
		minorAt("internal/daemon/landing_worker.go", 2, "swap the nil check"),
		minorAt("internal/land/note.go", 3, "drop the dead branch"),
	}
	f := newFollowupFixture(t, 0.8, findings)
	f.lander.FollowupBackpressure = 2
	f.fake.existing = openFollowupBeads(1)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads at a 2-bead bound with one open, want one:\n%s", len(f.fake.created), log)
	}
	// Packages file in sorted order, so the bead the bound let through is docs.
	if !strings.Contains(f.fake.created[0].Body, "in docs") {
		t.Errorf("bead = %q, want the first package filed", f.fake.created[0].Body)
	}
	if !strings.Contains(log, "2 om-followup beads are open, at the 2-bead backpressure bound; held back 2 findings for this landing") {
		t.Errorf("log = %q, want the bound, the open count and the held-back findings", log)
	}
}

// TestFollowupFeedHandsTheHeldBackFindingsToTheSink: the bound holds findings
// back silently otherwise, so the feed hands the loss to the attention sink it
// was given with the counts that make it actionable. How often that reaches a
// human is the sink's business: the feed holds no state between landings to
// rate-limit it with (gt-4ty22).
func TestFollowupFeedHandsTheHeldBackFindingsToTheSink(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
	var alerts []string
	f.lander.FollowupAlert = func(message string) { alerts = append(alerts, message) }

	f.run()
	if len(alerts) != 1 {
		t.Fatalf("raised %d attention items, want one:\n%v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0], fmt.Sprintf("%d %s beads are open", DefaultFollowupBackpressure, FollowUpLabel)) ||
		!strings.Contains(alerts[0], "1 finding of this landing got no bead") {
		t.Errorf("alert = %q, want the open count and the findings lost named", alerts[0])
	}
}

// TestFollowupFeedRaisesNoAlertWithoutBackpressure: only the bound holding
// findings back raises the item, and a rig with nothing to raise it through
// loses nothing else (gt-4ty22).
func TestFollowupFeedRaisesNoAlertWithoutBackpressure(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	var alerts []string
	f.lander.FollowupAlert = func(message string) { alerts = append(alerts, message) }
	f.run()
	if len(alerts) != 0 {
		t.Fatalf("raised %d attention items with the feed running, want none:\n%v", len(alerts), alerts)
	}

	held := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	held.fake.existing = openFollowupBeads(DefaultFollowupBackpressure)
	log := held.run() // no FollowupAlert set: the line is still logged
	if !strings.Contains(log, "held back 1 finding for this landing") {
		t.Errorf("log = %q, want the held-back findings named", log)
	}
}

// TestFollowupFeedIsIdempotentPerLanding: a retry of the same landing attempt
// finds the bead the earlier attempt filed — by the fingerprint of the finding
// it carries — files no second one, and has nothing to say to it either: it is
// this landing's own work, not a finding another landing raised (gt-s01pn,
// gt-4ty22).
func TestFollowupFeedIsIdempotentPerLanding(t *testing.T) {
	t.Parallel()
	// filed is the shape the feed itself files: the finding's fingerprint label
	// and a body naming the landed commit.
	filed := []ExistingFollowup{{
		ID: "gt-filed", Title: "Found: name the error", Status: "open",
		Description: "## Goal\nFixed: the 1 finding om raised (landed commit 0123456789abcdef0123456789abcdef01234567).\n",
		Labels:      []string{FollowUpLabel, Fingerprint("internal/land/note.go", "name the error")},
	}}
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.fake.existing = filed
	// Seeded, so a comment the retry should not leave would land and be seen.
	f.bd.Seed(beads.Issue{ID: "gt-filed", Title: "Found: name the error", Status: "open", Type: "task"})
	f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("the retry filed %d beads for a landing already filed", len(f.fake.created))
	}
	comments, err := f.bd.Comments("gt-filed")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Errorf("comments = %v, want none on this landing's own bead", commentTexts(comments))
	}
}

// TestFollowupFeedRetryAfterAPartialFile: the per-package split files several
// beads, and an attempt that died after one of them leaves the rest to the
// retry. One bead naming the landing's commit must never stand for the whole
// landing, or the findings the attempt never reached are lost silently
// (gt-4ty22).
func TestFollowupFeedRetryAfterAPartialFile(t *testing.T) {
	t.Parallel()
	findings := []Finding{
		minorAt("docs/reference.md", 1, "name the error"),
		minorAt("internal/daemon/landing_worker.go", 2, "swap the nil check"),
		minorAt("internal/land/note.go", 3, "drop the dead branch"),
	}
	f := newFollowupFixture(t, 0.8, findings)
	// Docs is the package the interrupted attempt got to: its bead names the
	// commit and carries only its own finding's fingerprint.
	f.fake.existing = []ExistingFollowup{{
		ID: "gt-partial", Title: "Found: name the error in docs", Status: "open",
		Description: "## Goal\nFixed: the 1 finding om raised on gt-abc in docs (landed commit 0123456789abcdef0123456789abcdef01234567).\n",
		Labels:      []string{FollowUpLabel, Fingerprint("docs/reference.md", "name the error")},
	}}
	// Seeded, so a comment the retry should not leave would land and be seen.
	f.bd.Seed(beads.Issue{ID: "gt-partial", Title: "Found: name the error in docs", Status: "open", Type: "task"})
	log := f.run()
	if len(f.fake.created) != 2 {
		t.Fatalf("filed %d beads on the retry, want the two packages it never reached:\n%s", len(f.fake.created), log)
	}
	// Packages file in sorted order, so the two left are internal/daemon and
	// internal/land, and neither bead is a second one for docs.
	for i, want := range []string{"in internal/daemon", "in internal/land"} {
		if !strings.Contains(f.fake.created[i].Body, want) {
			t.Errorf("bead %d does not name %q:\n%s", i, want, f.fake.created[i].Body)
		}
	}
	comments, err := f.bd.Comments("gt-partial")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 0 {
		t.Errorf("comments = %v, want none on the bead the attempt filed", commentTexts(comments))
	}
}

// TestFollowupFeedFailuresAreLoggedAndDropped: the landing is recorded before
// the feed runs, so every failure here is a line and nothing else (gt-s01pn).
func TestFollowupFeedFailuresAreLoggedAndDropped(t *testing.T) {
	t.Parallel()
	t.Run("the work bead cannot be read", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.lander.Beads = &commentBeads{Fake: f.bd, failShow: true}
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads without reading the work bead", len(f.fake.created))
		}
		if !strings.Contains(log, "could not read the work bead") {
			t.Errorf("log = %q, want the failed read named", log)
		}
	})
	t.Run("the de-duplication read fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.listErr = errors.New("dolt: connection refused")
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads with no view of what exists", len(f.fake.created))
		}
		if !strings.Contains(log, "could not list the "+FollowUpLabel+" beads") {
			t.Errorf("log = %q, want the failed read named", log)
		}
	})
	t.Run("the file fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.fake.createErr = errors.New("database is locked")
		log := f.run()
		if !strings.Contains(log, "could not file the follow-up bead") {
			t.Errorf("log = %q, want the failed file named", log)
		}
	})
	t.Run("the comment fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorAt("internal/land/note.go", 230, "swap the nil check")})
		f.fake.existing = []ExistingFollowup{{ID: "gt-old", Title: "swap the nil check", Status: "open",
			Labels: []string{Fingerprint("internal/land/note.go", "swap the nil check")}}}
		f.lander.Beads = &commentBeads{Fake: f.bd, failComment: true}
		log := f.run()
		if !strings.Contains(log, "could not comment gt-old") {
			t.Errorf("log = %q, want the failed comment named", log)
		}
	})
	t.Run("the work bead comment fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
		f.lander.Beads = &commentBeads{Fake: f.bd, failComment: true}
		log := f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads, want the file to stand on its own", len(f.fake.created))
		}
		if !strings.Contains(log, "could not comment the filed follow-up onto gt-abc") {
			t.Errorf("log = %q, want the failed comment named", log)
		}
	})
}

// TestFollowupFeedIsOffWithoutABeadsSurface: the feed is off unless a rig's
// settings turned it on, and off is a nil surface rather than a flag the feed
// reads (gt-s01pn).
func TestFollowupFeedIsOffWithoutABeadsSurface(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{minorFinding("name the error")})
	f.lander.Followups = nil
	f.run()
	if len(f.fake.created) != 0 {
		t.Fatalf("filed %d beads with no follow-up surface", len(f.fake.created))
	}
}

// TestLandSurvivesAFollowupFeedFailure: the feed runs once the landing is
// recorded, so a beads store that cannot list, file or comment costs a log line
// and leaves the result exactly where it was (gt-s01pn).
func TestLandSurvivesAFollowupFeedFailure(t *testing.T) {
	t.Parallel()
	// The reviewer raises the finding the feed files on, at a score inside the
	// band: below the top of it and above the rig's own floor of 0. It is a
	// minor, because a major refuses the landing before the feed runs
	// (gt-oi4je), and the feed's own filters are not what this test is about.
	withFindings := func(f *landFixture) {
		f.review = &fakeReviewer{fn: func(string) (Verdict, error) {
			return Verdict{Verdict: VerdictApprove, Score: 0.8,
				Findings: []Finding{{ID: "1", Severity: "minor", Path: "b.txt", Line: 1, Title: "name the error"}}}, nil
		}}
	}
	control := newLandFixture(t)
	withFindings(control)
	controlRes, err := control.lander().Land(context.Background(), control.work)
	if err != nil {
		t.Fatalf("the control landing: %v", err)
	}
	for _, tc := range []struct {
		name     string
		feed     func(*Lander, *followupFake, *beadsfake.Fake)
		wantLog  string
		filedOne bool
	}{
		{
			name:     "a feed that files",
			feed:     func(l *Lander, fake *followupFake, _ *beadsfake.Fake) { l.Followups = fake },
			filedOne: true,
		},
		{
			name: "the file fails",
			feed: func(l *Lander, fake *followupFake, _ *beadsfake.Fake) {
				fake.createErr = errors.New("database is locked")
				l.Followups = fake
			},
			wantLog: "could not file the follow-up bead",
		},
		{
			name: "the de-duplication read fails",
			feed: func(l *Lander, fake *followupFake, _ *beadsfake.Fake) {
				fake.listErr = errors.New("dolt: connection refused")
				l.Followups = fake
			},
			wantLog: "could not list the " + FollowUpLabel + " beads",
		},
		{
			name: "the bead comment fails",
			feed: func(l *Lander, fake *followupFake, bd *beadsfake.Fake) {
				fake.existing = []ExistingFollowup{{ID: "gt-old", Title: "name the error", Status: "open",
					Labels: []string{Fingerprint("b.txt", "name the error")}}}
				l.Followups = fake
				l.Beads = &commentBeads{Fake: bd, failComment: true}
			},
			wantLog: "could not comment gt-old",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			withFindings(f)
			l := f.lander()
			fake := &followupFake{}
			tc.feed(l, fake, f.bd)
			var log strings.Builder
			l.Out = &log
			res, err := l.Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("Land: %v, want the landing to be unaffected", err)
			}
			if res.LandedCommit != controlRes.LandedCommit {
				t.Errorf("landed %s, want %s: the follow-up feed must not change the result",
					res.LandedCommit, controlRes.LandedCommit)
			}
			if res.Verdict.Verdict != VerdictApprove || res.Verdict.Score != 0.8 {
				t.Errorf("verdict = %+v, want the reviewer's approve at 0.80", res.Verdict)
			}
			if tc.filedOne != (len(fake.created) == 1) {
				t.Errorf("filed %d beads, want filedOne=%v", len(fake.created), tc.filedOne)
			}
			if tc.wantLog != "" && !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("log = %q, want the failure named (%q)", log.String(), tc.wantLog)
			}
		})
	}
}

// TestFollowupFeedRetryAtTheBackpressureBoundIsQuiet: a retry of a landing whose
// findings all have beads already — its own from the earlier attempt, and one
// another landing raised — has nothing for the bound to hold back. It raises no
// "got no bead" alert, and still leaves the seen-again comment on the other
// landing's bead (gt-r5uwc).
func TestFollowupFeedRetryAtTheBackpressureBoundIsQuiet(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorFinding("name the error"),
		minorAt("internal/land/other.go", 10, "swap the nil check"),
	})
	var alerts []string
	f.lander.FollowupAlert = func(message string) { alerts = append(alerts, message) }
	f.fake.existing = append(openFollowupBeads(DefaultFollowupBackpressure),
		ExistingFollowup{
			ID: "gt-filed", Title: "Found: name the error", Status: "open",
			Description: "## Goal\nFixed: the 1 finding om raised (landed commit 0123456789abcdef0123456789abcdef01234567).\n",
			Labels:      []string{FollowUpLabel, Fingerprint("internal/land/note.go", "name the error")},
		},
		ExistingFollowup{
			ID: "gt-other", Title: "Found: swap the nil check", Status: "open",
			Description: "## Goal\nFixed: the 1 finding om raised (landed commit fedcba9876543210fedcba9876543210fedcba98).\n",
			Labels:      []string{FollowUpLabel, Fingerprint("internal/land/other.go", "swap the nil check")},
		})
	f.bd.Seed(
		beads.Issue{ID: "gt-filed", Title: "Found: name the error", Status: "open", Type: "task"},
		beads.Issue{ID: "gt-other", Title: "Found: swap the nil check", Status: "open", Type: "task"},
	)
	log := f.run()
	if len(alerts) != 0 || strings.Contains(log, "held back") {
		t.Errorf("alerts = %v, log = %q; want a retry with nothing to hold back to stay quiet", alerts, log)
	}
	if len(f.fake.created) != 0 {
		t.Errorf("filed %d beads for findings that all have one", len(f.fake.created))
	}
	own, err := f.bd.Comments("gt-filed")
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		t.Errorf("comments on this landing's own bead = %v, want none", commentTexts(own))
	}
	other, err := f.bd.Comments("gt-other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || !strings.Contains(other[0].Text, "seen again on gt-abc") {
		t.Errorf("comments on the other landing's bead = %v, want one seen-again", commentTexts(other))
	}
}

// Every generated bead tells its polecat what to do when the fix removes a line
// an earlier landing added: the revert check will refuse it, so say so on the
// bead, name the deleted paths and ask for a deletes-by-spec waiver. The text
// never names the override flag (the refusal omits it so agents do not
// self-bypass), the bead keeps its required sections and still lints clean, and
// no label is added (gt-j0puq).
func TestFollowupBodyTellsThePolecatAboutTheRevertCheck(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt("internal/land/note.go", 230, "remove the duplicate helper an earlier landing added"),
	})
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	created := f.fake.created[0]
	constraints := specdispatch.Sections(created.Body)["constraints"]
	for _, want := range []string{"revert check will refuse", "say so in a comment on this bead", "naming each file", "deletes-by-spec waiver"} {
		if !strings.Contains(constraints, want) {
			t.Errorf("Constraints lacks %q:\n%s", want, constraints)
		}
	}
	if strings.Contains(strings.ToLower(created.Body), "allow-reverts") || strings.Contains(created.Body, "--allow") {
		t.Errorf("the body names an override flag:\n%s", created.Body)
	}
	for _, label := range created.Labels {
		if label == "deletes-by-spec" {
			t.Errorf("the bead was labelled deletes-by-spec automatically: %v", created.Labels)
		}
	}
	spec := specdispatch.Spec{ID: "gt-filed-0", Title: created.Title, Type: "task", Status: "open", Description: created.Body}
	if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
		t.Errorf("the body does not lint clean: %s\n%s", v.Line(spec.ID), created.Body)
	}
}
