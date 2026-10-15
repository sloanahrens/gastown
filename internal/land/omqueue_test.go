package land

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestLandQueuesAnApprovedLandingsFindings: the queue runs off the landing
// itself, once the record is written, so the bead a landing closed carries the
// findings om raised on it for the next deep-review run (gt-qtj72).
func TestLandQueuesAnApprovedLandingsFindings(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictApprove, Score: 0.62, Findings: []Finding{
			{ID: "1", Severity: "critical", Path: "b.txt", Line: 1, Title: "drop the lock"},
			{ID: "2", Severity: "major", Path: "b.txt", Line: 2, Title: "swap the nil check"},
			{ID: "3", Severity: "minor", Path: "b.txt", Line: 3, Title: "name the error"},
		}}, nil
	}
	lander := f.lander()
	lander.OMQueue = true
	if _, err := lander.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}

	issue := f.bead()
	want := []string{
		omQueuedMarker + Fingerprint("b.txt", "drop the lock") + " P1 b.txt:1 drop the lock",
		omQueuedMarker + Fingerprint("b.txt", "swap the nil check") + " P2 b.txt:2 swap the nil check",
	}
	var got []string
	for _, line := range strings.Split(issue.Notes, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), omQueuedMarker) {
			got = append(got, strings.TrimSpace(line))
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("queued lines = %q, want %q", got, want)
	}
	if !beads.HasLabel(issue, OMFindingsLabel) {
		t.Errorf("labels = %v, want %s", issue.Labels, OMFindingsLabel)
	}
	// The queue runs after the record, so the landing's own note is still the
	// first thing the bead carries.
	if !strings.Contains(issue.Notes, LandingNoteMarker) {
		t.Errorf("the landing record is missing from the notes:\n%s", issue.Notes)
	}
}

// omQueueFixture is one landing whose finding queue runs: the beads store the
// work bead lives in, and the lander the queue writes through.
type omQueueFixture struct {
	t      *testing.T
	bd     *beadsfake.Fake
	lander *Lander
	work   Work
	result Result
}

func newOMQueueFixture(t *testing.T, verdict string, score float64, findings []Finding) *omQueueFixture {
	t.Helper()
	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: "gt-abc", Title: "land the thing", Status: "open", Type: "task"})
	return &omQueueFixture{
		t: t, bd: bd,
		lander: &Lander{Beads: bd, OMQueue: true},
		work:   Work{BeadID: "gt-abc", Rig: "gastown", Branch: fixtureBranch, Head: "aaa1111", Target: "main"},
		result: Result{LandedCommit: "0123456789abcdef0123456789abcdef01234567",
			Verdict: Verdict{Verdict: verdict, Score: score, Findings: findings}},
	}
}

// run queues the fixture's landing and returns what the queue logged.
func (f *omQueueFixture) run() string {
	f.t.Helper()
	var log strings.Builder
	f.lander.Out = &log
	f.lander.queueOMFindings(f.work, f.result)
	return log.String()
}

func (f *omQueueFixture) bead() *beads.Issue {
	f.t.Helper()
	issue, err := f.bd.Show("gt-abc")
	if err != nil {
		f.t.Fatalf("Show(gt-abc): %v", err)
	}
	return issue
}

// queuedLines is the queued: ledger lines on the work bead's notes, in order.
func (f *omQueueFixture) queuedLines() []string {
	var out []string
	for _, line := range strings.Split(f.bead().Notes, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), omQueuedMarker) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func (f *omQueueFixture) comments() []beads.Comment {
	f.t.Helper()
	comments, err := f.bd.Comments("gt-abc")
	if err != nil {
		f.t.Fatalf("Comments(gt-abc): %v", err)
	}
	return comments
}

// omQueuePriority is the priority field the ledger's reader requires on every
// queued line (scripts/deep-review.sh queued rejects a line whose second field
// is not one of these, and counts the whole ledger as corrupt when it does).
var omQueuePriority = regexp.MustCompile(`^(P1|P2|P3)$`)

// TestOMQueueWritesTheLedgerLineForMajorAndCritical: an approved landing with
// one major and one minor finding queues exactly the major one, in the format
// the next deep-review run reads, and labels and comments the bead once
// (gt-qtj72).
func TestOMQueueWritesTheLedgerLineForMajorAndCritical(t *testing.T) {
	t.Parallel()
	f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{
		majorFinding("swap the nil check"),
		minorFinding("name the error"),
	})
	f.run()

	lines := f.queuedLines()
	if len(lines) != 1 {
		t.Fatalf("queued lines = %q, want exactly the major finding", f.bead().Notes)
	}
	// queued: <fingerprint> <P1|P2> <file:line> <claim>
	want := omQueuedMarker + Fingerprint("internal/land/note.go", "swap the nil check") +
		" P2 internal/land/note.go:230 swap the nil check"
	if lines[0] != want {
		t.Errorf("queued line = %q, want %q", lines[0], want)
	}
	// The reader splits a queued line on spaces and rejects anything under
	// four fields, so the shape is part of the format, not cosmetics.
	fields := strings.Fields(lines[0])
	if len(fields) < 4 || !omQueuePriority.MatchString(fields[2]) {
		t.Errorf("queued line %q does not parse: %d fields, priority %q", lines[0], len(fields), fields[2])
	}
	issue := f.bead()
	if !beads.HasLabel(issue, OMFindingsLabel) {
		t.Errorf("labels = %v, want %s", issue.Labels, OMFindingsLabel)
	}
	comments := f.comments()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want exactly one", len(comments))
	}
	if want := "om raised 1 finding (major/critical) on this approved landing (0.62); queued for the next deep-review run."; comments[0].Text != want {
		t.Errorf("comment = %q, want %q", comments[0].Text, want)
	}
}

// TestOMQueueSeveritiesTheReviewActsOn: the queue is for the findings the
// deep-review verifier would spend a pass on, so critical is P1 and major is P2
// in any spelling, and info, minor, an empty severity and anything unrecognized
// are nothing (gt-qtj72).
func TestOMQueueSeveritiesTheReviewActsOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		severity string
		want     string
	}{
		{"critical", "P1"},
		{"Critical", "P1"},
		{"  CRITICAL  ", "P1"},
		{"major", "P2"},
		{"Major", "P2"},
		{"minor", ""},
		{"info", ""},
		{"", ""},
		{"high", ""},
	} {
		t.Run(tc.severity, func(t *testing.T) {
			t.Parallel()
			f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{
				{ID: "1", Severity: tc.severity, Path: "internal/land/note.go", Line: 230, Title: "swap the nil check"},
			})
			f.run()

			lines := f.queuedLines()
			if tc.want == "" {
				if len(lines) != 0 {
					t.Fatalf("queued lines = %q, want none for severity %q", lines, tc.severity)
				}
				if beads.HasLabel(f.bead(), OMFindingsLabel) || len(f.comments()) != 0 {
					t.Errorf("severity %q was labelled or commented", tc.severity)
				}
				return
			}
			want := omQueuedMarker + Fingerprint("internal/land/note.go", "swap the nil check") +
				" " + tc.want + " internal/land/note.go:230 swap the nil check"
			if len(lines) != 1 || lines[0] != want {
				t.Fatalf("queued lines = %q, want [%q]", lines, want)
			}
		})
	}
}

// TestOMQueueOnlyForVerdictsOmApproved: a landing om did not approve was not
// recorded as approved, and a skipped or error verdict is not a verdict om
// reached at all (gt-qtj72).
func TestOMQueueOnlyForVerdictsOmApproved(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{
		VerdictRequestChanges, VerdictSkipped, VerdictErrorPrefix + "timeout",
		VerdictOverseerPrefix + "abc1234", VerdictUnrecorded, "", "maybe",
	} {
		t.Run(verdict, func(t *testing.T) {
			t.Parallel()
			f := newOMQueueFixture(t, verdict, 0.62, []Finding{majorFinding("swap the nil check")})
			f.run()
			if got := f.bead().Notes; got != "" {
				t.Fatalf("notes = %q, want nothing queued for verdict %q", got, verdict)
			}
			if beads.HasLabel(f.bead(), OMFindingsLabel) || len(f.comments()) != 0 {
				t.Errorf("verdict %q was labelled or commented", verdict)
			}
		})
	}
}

// TestOMQueueIsIdempotent: a landing pass a second time writes nothing twice —
// the line, the label and the comment are all already on the bead (gt-qtj72).
func TestOMQueueIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{
		majorFinding("swap the nil check"),
		{ID: "2", Severity: "critical", Path: "internal/land/note.go", Line: 0, Title: "drop the lock"},
	})
	first := f.run()
	if first == "" {
		t.Fatal("the first pass logged nothing")
	}
	before := f.bead().Notes
	f.run()

	if got := f.bead().Notes; got != before {
		t.Errorf("the second pass changed the notes:\n first %q\nsecond %q", before, got)
	}
	if lines := f.queuedLines(); len(lines) != 2 {
		t.Errorf("queued lines = %q, want the two findings once each", lines)
	}
	if comments := f.comments(); len(comments) != 1 {
		t.Errorf("comments = %d, want one: a retried landing comments once", len(comments))
	}
	// A finding whose line om left at zero still names a line: the ledger's
	// third field is one token the reader counts.
	want := []string{
		omQueuedMarker + Fingerprint("internal/land/note.go", "swap the nil check") +
			" P2 internal/land/note.go:230 swap the nil check",
		// om named no line for the second, so it is the file's first.
		omQueuedMarker + Fingerprint("internal/land/note.go", "drop the lock") +
			" P1 internal/land/note.go:1 drop the lock",
	}
	if got := f.queuedLines(); !slices.Equal(got, want) {
		t.Errorf("queued lines = %q, want %q", got, want)
	}
}

// TestOMQueueSkipsAFindingTheLedgerAlreadyNames: the run's ledger remembers a
// finding by fingerprint, so a work bead already carrying one — as a queued:
// line this queue wrote, or a finding: line a run wrote once it filed a bead —
// is not queued a second time (gt-qtj72).
func TestOMQueueSkipsAFindingTheLedgerAlreadyNames(t *testing.T) {
	t.Parallel()
	fp := Fingerprint("internal/land/note.go", "swap the nil check")
	for _, tc := range []struct {
		name  string
		notes string
	}{
		{"a queued: line", "queued: " + fp + " P2 internal/land/note.go:230 swap the nil check"},
		{"a finding: line", "finding: " + fp + " gt-filed"},
		{"a line among others", "some note\nqueued: " + fp + " P2 internal/land/note.go:230 swap the nil check\nmore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{majorFinding("swap the nil check")})
			if err := f.bd.AppendNotes("gt-abc", tc.notes); err != nil {
				t.Fatal(err)
			}
			f.run()
			if got := f.bead().Notes; got != tc.notes {
				t.Errorf("notes = %q, want them unchanged", got)
			}
			if comments := f.comments(); len(comments) != 0 {
				t.Errorf("comments = %d, want none for a finding already queued", len(comments))
			}
		})
	}
}

// TestOMQueueIsOffByDefault: the queue writes to beads, so a lander that does
// not turn it on does nothing at all — no notes, no label, no comment.
func TestOMQueueIsOffByDefault(t *testing.T) {
	t.Parallel()
	f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{majorFinding("swap the nil check")})
	f.lander.OMQueue = false
	if log := f.run(); log != "" {
		t.Errorf("queue logged %q with the setting off", log)
	}
	if got := f.bead().Notes; got != "" {
		t.Errorf("notes = %q, want nothing queued with the setting off", got)
	}
	if beads.HasLabel(f.bead(), OMFindingsLabel) || len(f.comments()) != 0 {
		t.Error("the setting is off and the bead was labelled or commented")
	}
}

// TestOMQueueDropsItsFailures: the landing is already recorded, so a store that
// refuses the read, the notes or the label costs a log line and nothing else
// (gt-qtj72).
func TestOMQueueDropsItsFailures(t *testing.T) {
	t.Parallel()
	findings := []Finding{majorFinding("swap the nil check")}
	for _, tc := range []struct {
		name string
		// break returns the beads surface the queue runs against.
		break_ func(*beadsfake.Fake) Beads
		want   string
	}{
		{"the read", func(f *beadsfake.Fake) Beads { return &failingBeads{Fake: f, failShowNow: true} }, "could not read the work bead"},
		{"the notes", func(f *beadsfake.Fake) Beads { return &failingBeads{Fake: f, failNotes: true} }, "could not queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newOMQueueFixture(t, VerdictApprove, 0.62, findings)
			f.lander.Beads = tc.break_(f.bd)
			log := f.run()
			if !strings.Contains(log, tc.want) {
				t.Errorf("log = %q, want it to name %q", log, tc.want)
			}
			if got := f.bead().Notes; got != "" {
				t.Errorf("notes = %q, want nothing written when the store failed", got)
			}
		})
	}
}

// TestOMQueueSkipsAFindingWithNoClaim: a line with fewer than four fields is a
// corrupt ledger to the reader, which would cost the whole run every line it
// holds, so a finding om gave no title for is left out rather than written
// malformed (gt-qtj72).
func TestOMQueueSkipsAFindingWithNoClaim(t *testing.T) {
	t.Parallel()
	f := newOMQueueFixture(t, VerdictApprove, 0.62, []Finding{
		{ID: "1", Severity: "major", Path: "internal/land/note.go", Line: 230, Title: "   "},
		{ID: "2", Severity: "critical", Path: "internal/land/note.go", Line: 12, Title: "drop the lock"},
	})
	f.run()

	lines := f.queuedLines()
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "drop the lock") {
		t.Fatalf("queued lines = %q, want only the finding with a claim", lines)
	}
	if fields := strings.Fields(lines[0]); len(fields) < 4 {
		t.Errorf("queued line = %q, want at least four fields", lines[0])
	}
}
