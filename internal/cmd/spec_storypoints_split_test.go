package cmd

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The split route (gt-5d9k2f.18): a bead whose estimate is at or over
// story_points.split_over_minutes on a rung of at least two inputs is labeled
// for splitting instead of slung. The route ships off.

// splitOutcome is the estimate a test's seam answers with.
func splitOutcome(action string, points int, minutes float64, rung, neighbors string) specEstimateOutcome {
	return specEstimateOutcome{ID: "gt-est", Action: action, Points: points, Minutes: minutes, Rung: rung, Neighbors: neighbors}
}

// splitHoldLines returns the tick's skipped lines the split route wrote.
func splitHoldLines(skipped []specDispatchEntry) []string {
	var out []string
	for _, e := range skipped {
		if strings.Contains(e.Line, specdispatch.SkipSplitOver) {
			out = append(out, e.Line)
		}
	}
	return out
}

// splitNotes returns the comments the split route wrote on a fake town's bead.
func splitNotes(f *fakeSpecTown, id string) []string {
	var out []string
	for _, n := range f.notes[id] {
		if strings.HasPrefix(n, specStoryPointSplitNotePrefix) {
			out = append(out, n)
		}
	}
	return out
}

// splitNotices returns the tick's notices that say the route could not judge a
// bead and slung it.
func splitNotices(notices []string) []string {
	var out []string
	for _, n := range notices {
		if strings.Contains(n, specdispatch.SkipSplitOver) {
			out = append(out, n)
		}
	}
	return out
}

// TestSpecDispatchSplitRouteOffChangesNothing: with the trigger 0 the tick runs
// no estimate, writes no label or comment, and slings the bead as before.
func TestSpecDispatchSplitRouteOffChangesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointShadow = false
	env.StoryPointSplitOver = 0
	calls := 0
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		calls++
		return splitOutcome("estimated", 13, 20, "all", "nearest beads: gt-b"), nil
	}
	r := runSpecDispatchCycle(env)

	if calls != 0 {
		t.Fatalf("estimated %d bead(s) with the route off, want none", calls)
	}
	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want gt-a", got)
	}
	if len(splitHoldLines(r.Skipped)) != 0 {
		t.Errorf("skipped = %+v, want no split hold with the route off", r.Skipped)
	}
	if slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
		t.Error("the route labeled a bead with the route off")
	}
	if notes := splitNotes(f, "gt-a"); len(notes) != 0 {
		t.Errorf("the route wrote comments with the route off: %q", notes)
	}
}

// TestSpecDispatchSplitRouteHoldsOverTrigger: at and over the trigger on a rung
// of at least two inputs the bead is not slung, gets the split label and a
// comment naming the estimate and the nearest beads, and one line is logged.
func TestSpecDispatchSplitRouteHoldsOverTrigger(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome specEstimateOutcome
	}{
		{"all", splitOutcome("estimated", 13, 20, "all", "nearest beads: gt-b (gastown) first, gt-c (gastown) second")},
		{"shape-jev", splitOutcome("estimated", 13, 15, "shape-jev", "nearest beads: gt-b (gastown) first")},
		{"shape-embed", splitOutcome("estimated", 13, 20, "shape-embed", "nearest beads: gt-b (gastown) first")},
		{"an unchanged estimate", splitOutcome("unchanged", 13, 20, "all", "nearest beads: gt-b (gastown) first")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
			env := f.env()
			env.StoryPointSplitOver = 15
			env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
				return tc.outcome, nil
			}
			r := runSpecDispatchCycle(env)

			if len(f.slung) != 0 {
				t.Fatalf("slung %v, want the bead held for splitting", f.slung)
			}
			if !slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
				t.Errorf("labels = %v, want %s", f.labels["gt-a"], specdispatch.SplitLabel)
			}
			notes := splitNotes(f, "gt-a")
			if len(notes) != 1 {
				t.Fatalf("split notes = %q, want one", notes)
			}
			for _, want := range []string{"sp:13", "rung " + tc.outcome.Rung, "at or over the 15-minute split trigger", "nearest beads: gt-b"} {
				if !strings.Contains(notes[0], want) {
					t.Errorf("split note %q does not carry %q", notes[0], want)
				}
			}
			lines := splitHoldLines(r.Skipped)
			if len(lines) != 1 {
				t.Fatalf("split hold lines = %q, want one", lines)
			}
			for _, want := range []string{"gt-a", specMinutesText(tc.outcome.Minutes) + " min", "(sp:13, rung " + tc.outcome.Rung + ")", "at or over the 15-minute trigger"} {
				if !strings.Contains(lines[0], want) {
					t.Errorf("hold line %q does not carry %q", lines[0], want)
				}
			}
		})
	}
}

// TestSpecDispatchSplitRouteSlingsUnderTrigger: an estimate below the trigger is
// a bead a seat can take, and the route writes nothing on it.
func TestSpecDispatchSplitRouteSlingsUnderTrigger(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 8, 10, "all", "nearest beads: none"), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want gt-a", got)
	}
	if slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
		t.Error("the route labeled a bead under the trigger")
	}
	if notes := splitNotes(f, "gt-a"); len(notes) != 0 {
		t.Errorf("the route wrote comments on a bead under the trigger: %q", notes)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
		t.Errorf("split hold lines = %q, want none", lines)
	}
}

// TestSpecDispatchSplitRouteSlingsShapeOnly: the shape-only rung is the weakest,
// and an estimate from it is slung however high it reads.
func TestSpecDispatchSplitRouteSlingsShapeOnly(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 21, 20, "shape", "nearest beads: none"), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want gt-a", got)
	}
	if slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
		t.Error("the route acted on a shape-only estimate")
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
		t.Errorf("split hold lines = %q, want none", lines)
	}
}

// TestSpecDispatchSplitRouteEstimateErrorSlingsAndRecordsNone: an estimate that
// fails is never read as a small bead. The estimate records sp-rung:none, the
// tick logs one line naming the bead and the reason, and the bead is slung
// because the route cannot judge it — not counted as under the trigger.
func TestSpecDispatchSplitRouteEstimateErrorSlingsAndRecordsNone(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-est", 1, "2026-09-29T10:00:00Z"))
	db := specEstimateSeed()
	stub := &specEstimateStub{err: errors.New("embedder unreachable")}
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = specStoryPointEstimateFor(
		func(string) specEstimateStore { return db },
		func(string) specEstimateRunner { return specEstimateRunnerFor(stub) })
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-est" {
		t.Fatalf("slung %q, want gt-est: a bead the route cannot judge is slung", got)
	}
	if !slices.Contains(specEstimateLabelsOn(t, db), "sp-rung:none") {
		t.Errorf("labels = %v, want sp-rung:none", specEstimateLabelsOn(t, db))
	}
	comments := specEstimateCommentsOn(t, db)
	if len(comments) != 1 || !strings.HasPrefix(comments[0], "SP ESTIMATE v1 sp:none rung:none") {
		t.Errorf("comments = %q, want the none note", comments)
	}
	notices := splitNotices(r.Notices)
	if len(notices) != 1 {
		t.Fatalf("split notices = %q, want exactly one line", notices)
	}
	for _, want := range []string{"gt-est", "embedder unreachable", "slung because the route cannot judge it"} {
		if !strings.Contains(notices[0], want) {
			t.Errorf("notice %q does not carry %q", notices[0], want)
		}
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
		t.Errorf("split hold lines = %q, want none: the route did not judge the bead", lines)
	}
}

// TestSpecDispatchSplitRouteTimeoutLogsAndSlings: an estimate past the tick's
// bound logs one line, is canceled, and the bead is slung.
func TestSpecDispatchSplitRouteTimeoutLogsAndSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.After = firedTimer
	handed := make(chan context.Context, 1)
	env.StoryPointEstimate = func(ctx context.Context, _ string) (specEstimateOutcome, error) {
		handed <- ctx
		<-ctx.Done()
		return specEstimateOutcome{}, ctx.Err()
	}
	r := runSpecDispatchCycle(env)

	if ctx := <-handed; ctx.Err() == nil {
		t.Fatal("the timed-out estimate's context was not canceled")
	}
	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q after a timeout, want gt-a", got)
	}
	notices := splitNotices(r.Notices)
	if len(notices) != 1 {
		t.Fatalf("split notices = %q, want exactly one line", notices)
	}
	if !strings.Contains(notices[0], "no estimate within") || !strings.Contains(notices[0], "slung because the route cannot judge it") {
		t.Errorf("notice %q does not say the estimate timed out and the bead was slung", notices[0])
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
		t.Errorf("split hold lines = %q, want none", lines)
	}
}

// TestSpecDispatchSplitRouteNoEstimatorSlingsAndLogs: with no estimator wired
// the route cannot judge the bead, so it says so and slings it.
func TestSpecDispatchSplitRouteNoEstimatorSlingsAndLogs(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = nil
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q with no estimator, want gt-a", got)
	}
	notices := splitNotices(r.Notices)
	if len(notices) != 1 || !strings.Contains(notices[0], "no estimator is wired") {
		t.Fatalf("split notices = %q, want one naming the missing estimator", notices)
	}
}

// TestSpecDispatchSplitRouteLabelFailureStillHolds: a split label that cannot
// be written is reported, and the bead is still held rather than spent on a
// seat. The next tick retries the label.
func TestSpecDispatchSplitRouteLabelFailureStillHolds(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 13, 20, "all", "nearest beads: none"), nil
	}
	env.AddLabel = func(string, string) error { return errors.New("bd refused") }
	r := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("slung %v after a failed split label, want the bead held", f.slung)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "gt-a") || !strings.Contains(r.Errors[0], "bd refused") {
		t.Errorf("errors = %q, want one naming the bead and the failed write", r.Errors)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 1 {
		t.Fatalf("split hold lines = %q, want the bead still reported held", lines)
	}
}

// TestSpecDispatchSplitRouteAlreadyLabeledNotTouched: a bead already carrying
// an estimate and the split label is left as it is. The ready query excludes it
// before the candidate loop, so this drives the guard that keeps a label landed
// after the board read from writing a second comment.
func TestSpecDispatchSplitRouteAlreadyLabeledNotTouched(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown()
	env := f.env()
	env.StoryPointSplitOver = 15
	calls := 0
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		calls++
		return splitOutcome("estimated", 13, 20, "all", ""), nil
	}
	latest := specdispatch.Spec{ID: "gt-a", Labels: []string{"sp:13", "sp-rung:all", "sp-minutes:20", specdispatch.SplitLabel}}
	step := specStoryPointStep(env, "gt-a", latest)

	if calls != 0 {
		t.Errorf("the route estimated an already-labeled bead %d time(s), want none", calls)
	}
	if step.Hold == "" {
		t.Error("the route did not hold an already-labeled bead")
	}
	if len(step.Notices) != 0 || len(step.Errors) != 0 {
		t.Errorf("step wrote notices %q and errors %q, want none", step.Notices, step.Errors)
	}
	if len(f.labels["gt-a"]) != 0 {
		t.Errorf("labels = %v, want none written", f.labels["gt-a"])
	}
	if notes := splitNotes(f, "gt-a"); len(notes) != 0 {
		t.Errorf("split notes = %q, want none written", notes)
	}
}

// TestSpecDispatchSplitRouteEstimateFromLabels: an estimate that is already
// current has no fresh numbers, so the route reads the labels the command
// wrote. Two labels that disagree leave the value unset, and the route then
// treats the bead as one it cannot judge.
func TestSpecDispatchSplitRouteEstimateFromLabels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		labels      []string
		wantPoints  int
		wantRung    string
		wantMinutes float64
	}{
		{"a complete estimate", []string{"sp:13", "sp-rung:all", "sp-minutes:20"}, 13, "all", 20},
		{"a none estimate", []string{"sp-rung:none"}, 0, "none", 0},
		{"two rungs disagree", []string{"sp:13", "sp-rung:all", "sp-rung:shape", "sp-minutes:20"}, 13, "", 20},
		{"two minutes disagree", []string{"sp:13", "sp-rung:all", "sp-minutes:20", "sp-minutes:10"}, 13, "all", 0},
		{"two points disagree", []string{"sp:13", "sp:8", "sp-rung:all", "sp-minutes:20"}, 0, "all", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o specEstimateOutcome
			specEstimateFromLabels(&o, tc.labels)
			if o.Points != tc.wantPoints || o.Rung != tc.wantRung || o.Minutes != tc.wantMinutes {
				t.Errorf("outcome = sp:%d rung:%q minutes:%v, want sp:%d rung:%q minutes:%v",
					o.Points, o.Rung, o.Minutes, tc.wantPoints, tc.wantRung, tc.wantMinutes)
			}
		})
	}
}

// TestSpecDispatchSplitRouteUnknownRungSlings: a label with a rung the route
// does not know is not read as a rung of two inputs; the bead is slung with the
// line naming the reason.
func TestSpecDispatchSplitRouteUnknownRungSlings(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 21, 20, "bogus", "nearest beads: none"), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want gt-a", got)
	}
	notices := splitNotices(r.Notices)
	if len(notices) != 1 || !strings.Contains(notices[0], `rung "bogus"`) {
		t.Fatalf("split notices = %q, want one naming the unknown rung", notices)
	}
}

// TestSpecDispatchSplitRouteHoldsAnUnchangedEstimate: a bead whose estimate is
// already current is still judged. The route reads the labels and the note the
// command left, holds the bead, and writes no second estimate note.
func TestSpecDispatchSplitRouteHoldsAnUnchangedEstimate(t *testing.T) {
	t.Parallel()
	db := specEstimateSeed("sp:13", "sp-rung:all", "sp-minutes:20")
	if err := db.AddComment("gt-est", "SP ESTIMATE v1 sp:13 rung:all hash:"+specEstimateHashOf(t, db)+
		"\nminutes: 20.0\ninputs: shape, jev\nnearest beads: gt-b (gastown) first"); err != nil {
		t.Fatal(err)
	}
	stub := specEstimateOK()
	f := newFakeSpecTown(cleanSpec("gt-est", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = specStoryPointEstimateFor(
		func(string) specEstimateStore { return db },
		func(string) specEstimateRunner { return specEstimateRunnerFor(stub) })
	r := runSpecDispatchCycle(env)

	if stub.calls != 0 {
		t.Errorf("estimated a bead whose estimate was current %d time(s), want none", stub.calls)
	}
	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want the bead held", f.slung)
	}
	if !slices.Contains(f.labels["gt-est"], specdispatch.SplitLabel) {
		t.Errorf("labels = %v, want %s", f.labels["gt-est"], specdispatch.SplitLabel)
	}
	notes := splitNotes(f, "gt-est")
	if len(notes) != 1 || !strings.Contains(notes[0], "nearest beads: gt-b (gastown) first") {
		t.Fatalf("split notes = %q, want one quoting the current estimate's nearest beads", notes)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 1 {
		t.Fatalf("split hold lines = %q, want one", lines)
	}
}

// TestSpecDispatchSplitRouteNoteFailureStillHolds: a split comment that cannot
// be written is reported, and the bead is still held.
func TestSpecDispatchSplitRouteNoteFailureStillHolds(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 13, 20, "all", "nearest beads: none"), nil
	}
	env.Annotate = func(string, string, string) error { return errors.New("bd refused") }
	r := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("slung %v after a failed split note, want the bead held", f.slung)
	}
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "gt-a") || !strings.Contains(r.Errors[0], "bd refused") {
		t.Errorf("errors = %q, want one naming the bead and the failed write", r.Errors)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 1 {
		t.Fatalf("split hold lines = %q, want the bead still reported held", lines)
	}
}

// TestSpecDispatchSplitRouteVerdictRefusals: every estimate the route cannot
// judge returns a reason, and the caller says so and slings; the shape-only
// rung is a verdict the route reads and leaves alone.
func TestSpecDispatchSplitRouteVerdictRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		answer     specStoryPointAnswer
		wantRung   string
		wantReason string
	}{
		{name: "timeout", answer: specStoryPointAnswer{timedOut: true}, wantReason: "no estimate within"},
		{name: "error", answer: specStoryPointAnswer{err: errors.New("embedder refused")}, wantReason: "embedder refused"},
		{name: "none with no reason", answer: specStoryPointAnswer{outcome: splitOutcome("none", 0, 0, "", "")}, wantReason: "the estimate records no reason"},
		{name: "no rung", answer: specStoryPointAnswer{outcome: splitOutcome("unchanged", 0, 0, "", "")}, wantReason: "records no rung"},
		{name: "rung none", answer: specStoryPointAnswer{outcome: splitOutcome("unchanged", 0, 0, "none", "")}, wantReason: "records no rung"},
		{name: "unknown rung", answer: specStoryPointAnswer{outcome: splitOutcome("estimated", 13, 20, "bogus", "")}, wantReason: "not one the route reads"},
		{name: "no minutes", answer: specStoryPointAnswer{outcome: splitOutcome("estimated", 13, 0, "all", "")}, wantReason: "records no minutes"},
		{name: "nan minutes", answer: specStoryPointAnswer{outcome: splitOutcome("estimated", 13, math.NaN(), "all", "")}, wantReason: "records no minutes"},
		{name: "shape only", answer: specStoryPointAnswer{outcome: splitOutcome("estimated", 21, 20, "shape", "")}, wantRung: "shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rung, reason := specSplitVerdict(tc.answer)
			if rung != tc.wantRung || !strings.Contains(reason, tc.wantReason) {
				t.Errorf("verdict = rung %q reason %q, want rung %q and reason %q", rung, reason, tc.wantRung, tc.wantReason)
			}
		})
	}
}
