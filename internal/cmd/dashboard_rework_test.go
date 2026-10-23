package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/land"
)

var reworkReaderNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// The dashboard cannot import the landing package, so the loop cap it counts
// against is its own constant; this holds it to the worker's.
func TestReworkLoopCapIsTheLandingWorkersAttemptCap(t *testing.T) {
	t.Parallel()
	if dashboard.ReworkLoopCap != land.MaxReworkAttempts {
		t.Fatalf("dashboard.ReworkLoopCap = %d, land.MaxReworkAttempts = %d", dashboard.ReworkLoopCap, land.MaxReworkAttempts)
	}
}

// What the landing worker writes is what the table counts: lines appended
// through the worker's own writer, read back and folded, with the lines older
// than both windows left out.
func TestReworkReaderFoldsWhatTheWorkerWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	log := land.TownReviewsFile(root)
	at := func(h time.Duration) time.Time { return reworkReaderNow.Add(-h * time.Hour) }
	for _, rec := range []land.ReviewRecord{
		{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.5, Verdict: "request_changes", Findings: 2, Bar: 0.8, At: at(30)},
		{BeadID: "gt-a", Rig: "gastown", Attempt: 2, Score: 0.9, Verdict: "approve", Bar: 0.8, At: at(20)},
		{BeadID: "gt-old", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: at(24 * 30)},
	} {
		if err := log.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	got := newDashReworkReader(root).read(reworkReaderNow)
	if got.Unavailable || len(got.Windows) != 2 {
		t.Fatalf("read = %+v, want two windows", got)
	}
	bars := got.Windows[0].Bars
	if len(bars) != 1 || bars[0].Bar != 0.8 || bars[0].Reviews != 2 || bars[0].Rejected != 1 || bars[0].Beads != 1 ||
		bars[0].FirstTry != 0 || bars[0].Landed != 1 || bars[0].ReviewsPerLanded != 2 {
		t.Errorf("last 7 days = %+v, want gt-a's rejection and approval under 0.8", bars)
	}
	if len(got.Windows[1].Bars) != 0 {
		t.Errorf("the week before = %+v, want none: gt-old is a month old", got.Windows[1].Bars)
	}
}

// A log the landing worker has not created yet is a week with no reviews; one
// that cannot be read is an unavailable table, never an empty week.
func TestReworkReaderNamesAnUnreadableLog(t *testing.T) {
	t.Parallel()

	missing := newDashReworkReader(t.TempDir()).read(reworkReaderNow)
	if missing.Unavailable || len(missing.Windows) != 2 || len(missing.Windows[0].Bars) != 0 {
		t.Errorf("no log read = %+v, want two empty windows", missing)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".runtime", "landings", land.ReviewsFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newDashReworkReader(root).read(reworkReaderNow); !got.Unavailable {
		t.Errorf("a log that is a directory read = %+v, want unavailable", got)
	}
}

// A bounced approval the worker logs with Rejected reaches the table as a
// rejection, not a first-try landing (gt-vu18r).
func TestReworkReaderCountsAFindingsBounce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{BeadID: "gt-a", Rig: "gastown", Attempt: 1, Score: 0.93,
		Verdict: "approve", Rejected: true, Findings: 1, Bar: 0.8, At: reworkReaderNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	bars := newDashReworkReader(root).read(reworkReaderNow).Windows[0].Bars
	if len(bars) != 1 || bars[0].Rejected != 1 || bars[0].FirstTry != 0 || bars[0].Landed != 0 {
		t.Errorf("last 7 days = %+v, want one rejection and no first try", bars)
	}
}

// A line written before Rejected existed carries a findings bounce as a plain
// approve, and the bead's next attempt is what shows it was turned back. The
// rework panel and the agent counters both read that attempt from the same
// record, so both count it as rejected.
func TestReworkPanelAndAgentCountersAgreeOnAPreFieldApproval(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{BeadID: "gt-a", Rig: "gastown", Agent: "claude-haiku", Attempt: 1,
		Score: 0.93, Verdict: "approve", Findings: 2, Bar: 0.8, At: reworkReaderNow.Add(-3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{BeadID: "gt-a", Rig: "gastown", Agent: "claude-haiku", Attempt: 2,
		Score: 0.95, Verdict: "approve", Bar: 0.8, At: reworkReaderNow.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	bars := newDashReworkReader(root).read(reworkReaderNow).Windows[0].Bars
	if len(bars) != 1 || bars[0].Rejected != 1 || bars[0].FirstTry != 0 || bars[0].Landed != 1 {
		t.Errorf("rework panel = %+v, want the first attempt rejected and the second landed", bars)
	}

	r := &modelsReader{townRoot: root, omPath: filepath.Join(t.TempDir(), "missing.json"), now: func() time.Time { return reworkReaderNow }}
	activity, _ := r.agentActivity()
	if want := []dashboard.AgentActivity{{Agent: "claude-haiku", Rejected: 1}}; len(activity) != 1 || activity[0] != want[0] {
		t.Errorf("agent counters = %+v, want %+v", activity, want)
	}
}

// reworkStage is one landing's "stages:" line, stamped in local time the way
// the daemon writes it: the worker logs the line as a review ends, a moment
// before it writes the verdict.
func reworkStageLine(bead string, at time.Time, stages string) string {
	return at.Format("2006/01/02 15:04:05") + " landing_worker: [land] " + bead + ": stages: " + stages
}

// Each verdict is timed from the stages line its own attempt logged: the last
// line for the bead before the verdict, so a bead's three reviews carry their
// three lines and not one line three times. A verdict whose line named no om
// stage (the review did not run) and one whose line is missing from the log
// both reach the table with no duration, and the bar's mean is over the landed
// beads it has one for (gt-uiiml).
func TestReworkReaderTimesEachReviewFromItsStagesLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	ago := func(h time.Duration) time.Time { return now.Add(-h) }
	root := t.TempDir()
	log := land.TownReviewsFile(root)
	for _, rec := range []land.ReviewRecord{
		// gt-one landed on one review, 2m30s of om.
		{BeadID: "gt-one", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: ago(6 * time.Hour)},
		// gt-three took three reviews; the middle attempt's line names no om.
		{BeadID: "gt-three", Rig: "gastown", Attempt: 1, Score: 0.5, Verdict: "request_changes", Bar: 0.8, At: ago(5 * time.Hour)},
		{BeadID: "gt-three", Rig: "gastown", Attempt: 2, Score: 0.6, Verdict: "request_changes", Bar: 0.8, At: ago(4*time.Hour + 30*time.Minute)},
		{BeadID: "gt-three", Rig: "gastown", Attempt: 3, Score: 0.85, Verdict: "approve", Bar: 0.8, At: ago(4 * time.Hour)},
		// gt-lone landed with no stages line in the log at all.
		{BeadID: "gt-lone", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: ago(3 * time.Hour)},
		// gt-loop never landed, and 20 minutes of om is nobody's rework.
		{BeadID: "gt-loop", Rig: "gastown", Attempt: 1, Score: 0.4, Verdict: "request_changes", Bar: 0.8, At: ago(2 * time.Hour)},
		{BeadID: "gt-loop", Rig: "gastown", Attempt: 2, Score: 0.4, Verdict: "request_changes", Bar: 0.8, At: ago(90 * time.Minute)},
		{BeadID: "gt-loop", Rig: "gastown", Attempt: 3, Score: 0.4, Verdict: "request_changes", Bar: 0.8, At: ago(55 * time.Minute)},
	} {
		if err := log.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	lines := []string{
		reworkStageLine("gt-one", ago(6*time.Hour+2*time.Second), "lint 18s, gate 1m32s, om 2m30s"),
		reworkStageLine("gt-three", ago(5*time.Hour+2*time.Second), "lint 18s, gate 1m, om 1m"),
		// The review that did not run: the line is there and names no om stage.
		reworkStageLine("gt-three", ago(4*time.Hour+30*time.Minute+2*time.Second), "lint 19s, gate 1m10s"),
		reworkStageLine("gt-three", ago(4*time.Hour+2*time.Second), "lint 18s, gate 1m20s, om 2m30s"),
		// A line for a different bead an hour before gt-lone's verdict must not
		// be read as its own.
		reworkStageLine("gt-other", ago(4*time.Hour), "lint 18s, gate 1m20s, om 9m"),
		// The loop's lines: 20 minutes of om the table does not count.
		reworkStageLine("gt-loop", ago(2*time.Hour+2*time.Second), "lint 18s, gate 1m25s, om 20m"),
		reworkStageLine("gt-loop", ago(90*time.Minute+2*time.Second), "lint 18s, gate 1m25s, om 20m"),
		reworkStageLine("gt-loop", ago(55*time.Minute+2*time.Second), "lint 18s, gate 1m25s, om 20m"),
	}
	om := &omReader{logPath: omTestLogFile(t, lines...)}
	got := newDashReworkReader(root).withStages(om.stagesSince).read(now)
	if got.Unavailable || len(got.Windows) != 2 {
		t.Fatalf("read = %+v, want two windows", got)
	}
	bars := got.Windows[0].Bars
	if len(bars) != 1 {
		t.Fatalf("last 7 days = %+v, want one bar", bars)
	}
	b := bars[0]
	if b.Landed != 3 || b.LoopsOut != 1 {
		t.Fatalf("bar = %+v, want the three landed beads and the loop out", b)
	}
	if b.OMBeads != 2 || b.MeanOMSecs != 180 || b.MedianOMSecs != 180 || b.SlowestOMSecs != 210 || b.SlowestOMBead != "gt-three" {
		t.Errorf("om time = %+v, want a mean of 180s over gt-one's 150 and gt-three's 210, slowest gt-three", b)
	}
	if b.OMReviewsMissing != 2 {
		t.Errorf("om reviews missing = %d, want gt-three's second and gt-lone's only review", b.OMReviewsMissing)
	}
	if len(got.Windows[1].Bars) != 0 {
		t.Errorf("the week before = %+v, want none: every review is inside the last seven days", got.Windows[1].Bars)
	}
}

// A verdict's line is the last one at or before it, never a later one, and no
// two verdicts read the same line: a review whose own line the log has lost is
// left with no duration rather than handed the line of the review before it,
// which would count one om run twice. Two verdicts for one attempt (the landing
// retried before the merge) keep their own lines, the later pair being the one
// the table counts.
func TestWithReviewOMTakesOnlyTheLineBeforeTheVerdict(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	ctx := func(secs int) *time.Duration { d := time.Duration(secs) * time.Second; return &d }
	reviews := []dashboard.ReworkReview{
		{Bead: "gt-a", Attempt: 1, At: base.Add(10 * time.Minute)},
		{Bead: "gt-a", Attempt: 2, At: base.Add(50 * time.Minute)},
		{Bead: "gt-b", At: base.Add(10 * time.Minute)},
		{Bead: "gt-c", At: base.Add(10 * time.Minute)},
		{Bead: "gt-d", Attempt: 1, At: base.Add(20 * time.Minute)},
		{Bead: "gt-d", Attempt: 1, At: base.Add(20*time.Minute + 5*time.Second)},
	}
	withReviewOM(reviews, []omStage{
		{At: base.Add(9 * time.Minute), Bead: "gt-a", OM: ctx(60)},
		// gt-b's verdict is 50 minutes after its line: outside the join window.
		{At: base.Add(-40 * time.Minute), Bead: "gt-b", OM: ctx(400)},
		// gt-c has a line and names no om stage.
		{At: base.Add(9 * time.Minute), Bead: "gt-c"},
		{At: base.Add(19*time.Minute + 59*time.Second), Bead: "gt-d", OM: ctx(30)},
		{At: base.Add(20*time.Minute + 4*time.Second), Bead: "gt-d", OM: ctx(45)},
	})
	want := []*time.Duration{ctx(60), nil, nil, nil, ctx(30), ctx(45)}
	for i := range reviews {
		got, exp := reviews[i].OM, want[i]
		if (got == nil) != (exp == nil) || (got != nil && *got != *exp) {
			t.Errorf("review %+v = %v, want %v", reviews[i], got, exp)
		}
	}
}

// A review in the window's first minutes is timed by a stages line logged just
// before the window opened, so the lines are read from omStageJoinMin before
// its start and not from the start itself.
func TestReworkReaderTimesAReviewAtTheWindowStart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	start := now.Add(-dashboard.ReworkReach)
	root := t.TempDir()
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{
		BeadID: "gt-edge", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: start.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	om := &omReader{logPath: omTestLogFile(t, reworkStageLine("gt-edge", start.Add(-time.Minute), "lint 18s, gate 1m, om 2m"))}
	var asked time.Time
	stages := func(since time.Time) ([]omStage, error) {
		asked = since
		return om.stagesSince(since)
	}
	got := newDashReworkReader(root).withStages(stages).read(now)
	if want := start.Add(-omStageJoinMin); !asked.Equal(want) {
		t.Errorf("stages asked from %v, want %v: omStageJoinMin before the window", asked, want)
	}
	var timed *dashboard.ReworkBar
	for _, w := range got.Windows {
		for i := range w.Bars {
			if w.Bars[i].OMBeads > 0 {
				timed = &w.Bars[i]
			}
		}
	}
	if timed == nil || timed.MeanOMSecs != 120 {
		t.Fatalf("read = %+v, want gt-edge timed at 120s from the line before the window", got)
	}
}

// A daemon log that cannot be read is said apart from reviews the log has no
// line for: the table keeps its figures and carries OMUnavailable.
func TestReworkReaderSaysAnUnreadableDaemonLog(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	root := t.TempDir()
	if err := land.TownReviewsFile(root).Append(land.ReviewRecord{
		BeadID: "gt-one", Rig: "gastown", Attempt: 1, Score: 0.9, Verdict: "approve", Bar: 0.8, At: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	om := &omReader{logPath: filepath.Join(t.TempDir(), "absent.log")}
	got := newDashReworkReader(root).withStages(om.stagesSince).read(now)
	if got.Unavailable || !got.OMUnavailable || len(got.Windows[0].Bars) != 1 {
		t.Fatalf("read = %+v, want the review table with OMUnavailable", got)
	}

	// With the log readable and no line for the review, the table does not say so.
	readable := &omReader{logPath: omTestLogFile(t, reworkStageLine("gt-other", now.Add(-time.Hour), "om 1m"))}
	got = newDashReworkReader(root).withStages(readable.stagesSince).read(now)
	if got.OMUnavailable {
		t.Errorf("a readable log with no line for the review read as OMUnavailable: %+v", got)
	}
}
