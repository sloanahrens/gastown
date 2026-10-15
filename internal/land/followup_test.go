package land

import (
	"context"
	"errors"
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
	f.created = append(f.created, spec)
	// A filed bead is one the rest of the run can see, so a second pass over
	// the same landing de-duplicates against it.
	f.existing = append(f.existing, ExistingFollowup{ID: "gt-filed", Title: spec.Title,
		Description: spec.Body, Status: "open", Labels: spec.Labels})
	return "gt-filed", nil
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

func majorFinding(title string) Finding {
	return Finding{ID: "1", Severity: "major", Path: "internal/land/note.go", Line: 230, Title: title}
}

func minorFinding(title string) Finding {
	return Finding{ID: "2", Severity: "minor", Path: "internal/land/note.go", Line: 260, Title: title}
}

// TestFollowupFeedFilesOnlyInTheBand: only an approved landing scored at or
// above the rig's own threshold and below followUpMaxScore is worth a follow-up
// bead (gt-s01pn).
func TestFollowupFeedFilesOnlyInTheBand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		score float64
		want  int
	}{
		{"below the rig's floor", 0.59, 0},
		{"at the rig's floor", followupThreshold, 1},
		{"inside the band", 0.8, 1},
		{"just under the top", 0.89, 1},
		{"at the top", followUpMaxScore, 0},
		{"above the top", 0.95, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, tc.score, []Finding{majorFinding("swap the nil check"), minorFinding("name the error")})
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
	findings := []Finding{majorFinding("swap the nil check"), minorFinding("name the error")}
	f := newFollowupFixture(t, 0.8, findings)
	log := f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one:\n%s", len(f.fake.created), log)
	}
	spec := f.fake.created[0]
	wantTitle := "Found: swap the nil check and 1 more (om major on gt-abc approved at 0.80)"
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

// TestFollowupFeedBodyLintsClean: the filed body is the shape the spec
// dispatcher dispatches, checked with its own lint rather than a copy of the
// shape kept here (gt-vpeaj, gt-s01pn).
func TestFollowupFeedBodyLintsClean(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check"), minorFinding("name the error")})
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	spec := specdispatch.Spec{ID: "gt-filed", Title: f.fake.created[0].Title, Type: "task",
		Status: "open", Description: f.fake.created[0].Body}
	if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
		t.Fatalf("the filed body does not lint clean: %s\n%s", v.Line(spec.ID), spec.Description)
	}
}

// TestFollowupFeedFoldsAcceptancePastTheLintMaximum: a landing's findings all
// land in one bead's acceptance list, folded into the last item past the
// dispatcher's max, because no worker takes a bead with more (gt-s01pn).
func TestFollowupFeedFoldsAcceptancePastTheLintMaximum(t *testing.T) {
	t.Parallel()
	var findings []Finding
	for i := 0; i < specdispatch.MaxAcceptance+2; i++ {
		findings = append(findings, majorFinding(string(rune('a'+i))+" finding"))
	}
	f := newFollowupFixture(t, 0.8, findings)
	f.run()
	if len(f.fake.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(f.fake.created))
	}
	body := f.fake.created[0].Body
	acceptance := specdispatch.Sections(body)["acceptance"]
	if got := specdispatch.CountAcceptance(acceptance); got != specdispatch.MaxAcceptance {
		t.Errorf("acceptance items = %d, want the max %d:\n%s", got, specdispatch.MaxAcceptance, acceptance)
	}
	if !strings.Contains(acceptance, "the remaining 3 findings under Goal are fixed") {
		t.Errorf("the folded item does not name what it covers:\n%s", acceptance)
	}
	// Every finding is still named under Goal, so none is silently dropped.
	goal, _, _ := strings.Cut(body, "\n## Constraints")
	if got := strings.Count(goal, "\n- "); got != len(findings) {
		t.Errorf("Goal lists %d findings, want %d:\n%s", got, len(findings), body)
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
	for _, verdict := range []string{VerdictSkipped, VerdictErrorPrefix + "timeout", VerdictOverseerPrefix + "aaa1111"} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
			f.result.Verdict.Verdict = verdict
			f.run()
			if len(f.fake.created) != 0 {
				t.Fatalf("filed %d beads for the %q verdict", len(f.fake.created), verdict)
			}
		})
	}
}

// TestFollowupFeedCommentsARepeat: a finding an earlier landing already filed
// goes on that bead's history, and nothing new is filed for it (gt-s01pn).
func TestFollowupFeedCommentsARepeat(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
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
	want := "seen again on gt-abc (om major, 0.80, 01234567)"
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
	f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check"), minorFinding("name the error")})
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

// TestFollowupFeedBackpressure: 30 open follow-ups stop the feed. It is
// backpressure rather than a cap, so the count falling back under the bound
// resumes it, and a closed follow-up is not work still waiting (gt-s01pn).
func TestFollowupFeedBackpressure(t *testing.T) {
	t.Parallel()
	open := func(n int) []ExistingFollowup {
		out := make([]ExistingFollowup, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ExistingFollowup{ID: "gt-open", Title: "an older finding", Status: "open"})
		}
		return out
	}
	t.Run("at the bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.existing = open(followUpBackpressure)
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("filed %d beads at the backpressure bound", len(f.fake.created))
		}
		if !strings.Contains(log, "backpressure") {
			t.Errorf("log = %q, want the bound named", log)
		}
	})
	t.Run("just under the bound", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.existing = append(open(followUpBackpressure-1), ExistingFollowup{ID: "gt-done", Title: "a fixed finding", Status: "closed"})
		f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads under the backpressure bound, want one", len(f.fake.created))
		}
	})
}

// TestFollowupFeedIsIdempotentPerLanding: a retry of the same landing attempt
// finds the bead it already filed — keyed on the landed commit — and files no
// second one (gt-s01pn).
func TestFollowupFeedIsIdempotentPerLanding(t *testing.T) {
	t.Parallel()
	filed := func(status string) []ExistingFollowup {
		return []ExistingFollowup{{
			ID: "gt-filed", Title: "Found: swap the nil check", Status: status,
			Description: "## Goal\nlanded commit 0123456789abcdef0123456789abcdef01234567\n",
			Labels:      []string{FollowUpLabel},
		}}
	}
	t.Run("the filed bead stops the retry", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.existing = filed("open")
		log := f.run()
		if len(f.fake.created) != 0 {
			t.Fatalf("the retry filed %d beads for a landing already filed", len(f.fake.created))
		}
		if !strings.Contains(log, "gt-filed already carries this landing's follow-up") {
			t.Errorf("log = %q, want the existing bead named", log)
		}
	})
	t.Run("a closed bead for the commit does not stop a new landing", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.existing = filed("closed")
		f.run()
		if len(f.fake.created) != 1 {
			t.Fatalf("filed %d beads where the commit's bead is closed, want one", len(f.fake.created))
		}
	})
}

// TestFollowupFeedFailuresAreLoggedAndDropped: the landing is recorded before
// the feed runs, so every failure here is a line and nothing else (gt-s01pn).
func TestFollowupFeedFailuresAreLoggedAndDropped(t *testing.T) {
	t.Parallel()
	t.Run("the de-duplication read fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
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
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.createErr = errors.New("database is locked")
		log := f.run()
		if !strings.Contains(log, "could not file the follow-up bead") {
			t.Errorf("log = %q, want the failed file named", log)
		}
	})
	t.Run("the comment fails", func(t *testing.T) {
		t.Parallel()
		f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
		f.fake.existing = []ExistingFollowup{{ID: "gt-old", Title: "swap the nil check", Status: "open",
			Labels: []string{Fingerprint("internal/land/note.go", "swap the nil check")}}}
		f.lander.Beads = &commentBeads{Fake: f.bd, failComment: true}
		log := f.run()
		if !strings.Contains(log, "could not comment gt-old") {
			t.Errorf("log = %q, want the failed comment named", log)
		}
	})
}

// TestFollowupFeedIsOffWithoutABeadsSurface: the feed is off unless a rig's
// settings turned it on, and off is a nil surface rather than a flag the feed
// reads (gt-s01pn).
func TestFollowupFeedIsOffWithoutABeadsSurface(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{majorFinding("swap the nil check")})
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
