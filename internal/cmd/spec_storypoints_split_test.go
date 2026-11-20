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

// TestSpecDispatchSplitRouteHoldsOverTrigger: at and over the trigger on the
// `all` or `shape-jev` rung the bead is not slung, gets the split label and a
// comment naming the estimate and the nearest beads, and one line is logged.
func TestSpecDispatchSplitRouteHoldsOverTrigger(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		outcome specEstimateOutcome
	}{
		{"all", splitOutcome("estimated", 13, 20, "all", "nearest beads: gt-b (gastown) first, gt-c (gastown) second")},
		{"shape-jev", splitOutcome("estimated", 13, 15, "shape-jev", "nearest beads: gt-b (gastown) first")},
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

// TestSplitRungShapeEmbedDoesNotAct: the route acts on `all` and `shape-jev`
// only, and the verdict for a shape-embed estimate names the rung and carries
// no minutes, so the caller slings the bead (gt-s93y6n).
func TestSplitRungShapeEmbedDoesNotAct(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rung string
		want bool
	}{
		{"all", true},
		{"shape-jev", true},
		{"shape-embed", false},
		{"shape", false},
		{"", false},
	} {
		t.Run(tc.rung, func(t *testing.T) {
			t.Parallel()
			if got := specSplitRungActs(tc.rung); got != tc.want {
				t.Errorf("specSplitRungActs(%q) = %v, want %v", tc.rung, got, tc.want)
			}
		})
	}
	minutes, rung, reason := specSplitVerdict(specStoryPointAnswer{outcome: splitOutcome("estimated", 13, 12.8, "shape-embed", "")})
	if minutes != 0 || rung != "" || !strings.Contains(reason, "shape-embed") || strings.Contains(reason, "12.8") {
		t.Errorf("verdict = %v min, rung %q, reason %q; want no minutes, no rung, and a reason naming the rung", minutes, rung, reason)
	}
}

// TestSplitRungShapeEmbedIsNotHeld: a bead estimated at shape-embed over the
// trigger is slung with the notice saying the route cannot judge it, and gets
// no split label or note (gt-s93y6n).
func TestSplitRungShapeEmbedIsNotHeld(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 13, 20, "shape-embed", ""), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-a" {
		t.Fatalf("slung %q, want gt-a: a shape-embed estimate never holds a bead", got)
	}
	if slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
		t.Errorf("labels = %v, want no split label", f.labels["gt-a"])
	}
	if notes := splitNotes(f, "gt-a"); len(notes) != 0 {
		t.Errorf("split notes = %q, want none", notes)
	}
	notices := splitNotices(r.Notices)
	if len(notices) != 1 || !strings.Contains(notices[0], "shape-embed") || !strings.Contains(notices[0], "cannot judge it") {
		t.Errorf("notices = %q, want one naming the rung and saying the route cannot judge it", notices)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
		t.Errorf("split hold lines = %q, want none", lines)
	}
}

// TestSplitRungShapeEmbedStaleLabelReleased: specSplitLabelStale is the other
// caller of the acting-rung set. A split label recorded at shape-embed is
// stale whatever minutes it carries, because the weak-rung rule fires before
// the acting set is consulted (gt-s93y6n, gt-ie2ehm).
func TestSplitRungShapeEmbedStaleLabelReleased(t *testing.T) {
	t.Parallel()
	held := cleanSpec("gt-split", 1, "2026-09-29T10:00:00Z")
	held.Labels = []string{"sp:8", "sp-rung:shape-embed", "sp-minutes:12.8", specdispatch.SplitLabel}
	f := newFakeSpecTown(held)
	env := f.env()
	env.Candidates = f.productionCandidates(t, 10, false)
	env.StoryPointSplitOver = 10
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 8, 12.8, "shape-embed", ""), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-split" {
		t.Fatalf("slung %q, want gt-split: the stale label is dropped and the bead taken", got)
	}
	if slices.Contains(f.specs["gt-split"].Labels, specdispatch.SplitLabel) || len(f.unlabels) != 1 {
		t.Errorf("labels = %v removals = %v, want the split label taken off once", f.specs["gt-split"].Labels, f.unlabels)
	}
	notes := splitStaleNotes(f, "gt-split")
	if len(notes) != 1 || !strings.Contains(notes[0], "rung shape-embed") || strings.Contains(notes[0], "12.8") {
		t.Fatalf("stale notes = %q, want one naming rung shape-embed rather than a trigger", notes)
	}
	if notices := splitStaleNotices(r.Notices); len(notices) != 1 {
		t.Errorf("notices = %q, want one saying the bead is offered again", notices)
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
	// The hold names what the label records, as the board's own split line
	// does: one wording for one decision (gt-4yq0js).
	for _, want := range []string{"gt-a: split: ", "estimated 20.0 min (sp:13, rung all)", "at or over the 15-minute trigger"} {
		if !strings.Contains(step.Hold, want) {
			t.Errorf("hold %q lacks %q", step.Hold, want)
		}
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

// A bead sent back for rework is never held for splitting: its work is already
// on a branch, and the findings a rejection appends raise its estimate over the
// trigger the first pass stayed under (gt-bj22wp). The exemption reads the
// label before the estimate, so the route spends no estimate on the bead, and
// a bead without the label is still judged and held. Its SpecStoryPointStep
// prefix, like its sibling's, is what the gate line's -run
// StoryPointStep|SpecStoryPoint filter selects; a rename that drops the prefix
// hides both tests from that filter (gt-g9gpv0).
func TestSpecStoryPointStepNeverHoldsAReworkBead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		labels    []string
		wantCalls int
		wantHeld  bool
	}{
		{name: "a rework bead", labels: []string{reworkLabel}, wantCalls: 0},
		{name: "a bead without the label", wantCalls: 1, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
			spec.Labels = tc.labels
			f := newFakeSpecTown(spec)
			env := f.env()
			env.After = neverTimer
			env.StoryPointSplitOver = 11
			calls := 0
			env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
				calls++
				return splitOutcome("estimated", 13, 11.2, "all", "nearest beads: gt-b"), nil
			}
			r := runSpecDispatchCycle(env)

			if calls != tc.wantCalls {
				t.Errorf("estimated the bead %d time(s), want %d", calls, tc.wantCalls)
			}
			if tc.wantHeld {
				if !slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
					t.Errorf("labels = %v, want %s", f.labels["gt-a"], specdispatch.SplitLabel)
				}
				if notes := splitNotes(f, "gt-a"); len(notes) != 1 {
					t.Errorf("split notes = %q, want one", notes)
				}
				if lines := splitHoldLines(r.Skipped); len(lines) != 1 {
					t.Errorf("split hold lines = %q, want one", lines)
				}
				if len(f.slung) != 0 {
					t.Errorf("slung %v, want the bead held", f.slung)
				}
				return
			}
			if slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel) {
				t.Errorf("labels = %v, want no %s", f.labels["gt-a"], specdispatch.SplitLabel)
			}
			if notes := splitNotes(f, "gt-a"); len(notes) != 0 {
				t.Errorf("split notes = %q, want none", notes)
			}
			if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
				t.Errorf("split hold lines = %q, want none", lines)
			}
			if got := strings.Join(f.slung, " "); got != "gt-a" {
				t.Errorf("slung %q, want gt-a: a rework bead is never held for splitting", got)
			}
		})
	}
}

// The rework exemption covers every hold the route writes, including the one
// for a bead already wearing the split label: the route must not hold a bead
// whose work is on a branch, so it writes nothing and runs no estimate
// (gt-bj22wp). The ready query filters a split bead before the candidate loop,
// so the guard this pins is the one for a label that lands mid-tick.
func TestSpecStoryPointStepReworkExemptionCoversTheSplitHold(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown()
	env := f.env()
	env.After = neverTimer
	env.StoryPointSplitOver = 11
	calls := 0
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		calls++
		return splitOutcome("estimated", 13, 20, "all", ""), nil
	}
	latest := specdispatch.Spec{ID: "gt-a", Labels: []string{reworkLabel, specdispatch.SplitLabel}}
	step := specStoryPointStep(env, "gt-a", latest)

	if calls != 0 {
		t.Errorf("estimated a rework bead %d time(s), want none", calls)
	}
	if step.Hold != "" || len(step.Notices) != 0 || len(step.Errors) != 0 {
		t.Errorf("step = %+v, want no hold, notice or error", step)
	}
}

// splitSlungLabels returns the sp-slung labels a fake town's bead wears.
func splitSlungLabels(f *fakeSpecTown, id string) []string {
	var out []string
	for _, l := range f.labels[id] {
		if strings.HasPrefix(l, specSPSlungPrefix) {
			out = append(out, l)
		}
	}
	return out
}

// TestSpecStoryPointSlungReasons is gt-l04nj0: a bead the split route slings at
// or over its trigger records why it was slung instead of held — the rework
// exemption, a shape-only estimate, an estimate the route could not judge, or
// the trigger being off — and one sp-slung label lands, exactly. A bead under
// the trigger, and a bead with no estimate the route can read, records nothing.
func TestSpecStoryPointSlungReasons(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		trigger   float64
		shadow    bool
		labels    []string
		estimate  func(context.Context, string) (specEstimateOutcome, error)
		wantLabel string
		wantCalls int
	}{
		{
			name:    "a rework bead",
			trigger: 11,
			labels:  []string{reworkLabel, "sp:13", "sp-rung:all", "sp-minutes:12"},
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 13, 12, "all", ""), nil
			},
			wantLabel: "sp-slung:rework",
		},
		{
			name:    "a shape-only estimate",
			trigger: 11,
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 13, 12, "shape", ""), nil
			},
			wantLabel: "sp-slung:shape",
			wantCalls: 1,
		},
		{
			name:    "an estimate the route cannot judge",
			trigger: 11,
			labels:  []string{"sp:13", "sp-rung:all", "sp-minutes:12"},
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return specEstimateOutcome{}, errors.New("embedder unreachable")
			},
			wantLabel: "sp-slung:unjudged",
			wantCalls: 1,
		},
		{
			name:    "the route off",
			trigger: 0,
			shadow:  true,
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 13, 12, "all", ""), nil
			},
			wantLabel: "sp-slung:off",
			wantCalls: 1,
		},
		{
			name:    "a bead under the trigger",
			trigger: 11,
			labels:  []string{"sp:8", "sp-rung:all", "sp-minutes:9.3"},
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 8, 9.3, "all", ""), nil
			},
			wantCalls: 1,
		},
		{
			name:    "a shape-only estimate under the trigger",
			trigger: 11,
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 8, 9.3, "shape", ""), nil
			},
			wantCalls: 1,
		},
		{
			name:    "the route off with no estimate the route can read",
			trigger: 0,
			shadow:  true,
			estimate: func(context.Context, string) (specEstimateOutcome, error) {
				return specEstimateOutcome{}, errors.New("embedder unreachable")
			},
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
			spec.Labels = tc.labels
			f := newFakeSpecTown(spec)
			env := f.env()
			env.StoryPointSplitOver = tc.trigger
			env.StoryPointShadow = tc.shadow
			calls := 0
			env.StoryPointEstimate = func(ctx context.Context, id string) (specEstimateOutcome, error) {
				calls++
				return tc.estimate(ctx, id)
			}
			runSpecDispatchCycle(env)

			if calls != tc.wantCalls {
				t.Errorf("estimated the bead %d time(s), want %d", calls, tc.wantCalls)
			}
			if got := strings.Join(f.slung, " "); got != "gt-a" {
				t.Fatalf("slung %q, want gt-a: the bead is slung either way", got)
			}
			got := splitSlungLabels(f, "gt-a")
			if tc.wantLabel == "" {
				if len(got) != 0 {
					t.Errorf("sp-slung labels = %q, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0] != tc.wantLabel {
				t.Errorf("sp-slung labels = %q, want exactly [%q]", got, tc.wantLabel)
			}
		})
	}
}

// TestSpecStoryPointSlungOnce is gt-l04nj0: the reason is written once, so a
// second tick on a bead that already wears an sp-slung label adds no second
// one, and an AddLabel that fails is reported on the tick rather than
// swallowed.
func TestSpecStoryPointSlungOnce(t *testing.T) {
	t.Parallel()
	t.Run("a second tick adds no second label", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		env := f.env()
		env.StoryPointSplitOver = 11
		env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
			return splitOutcome("estimated", 13, 12, "shape", ""), nil
		}
		runSpecDispatchCycle(env)
		// The sling hooks the bead, so the board no longer offers it; the second
		// tick's own step, run on the bead as the tick re-reads it, is the case
		// the guard covers.
		f.mu.Lock()
		latest := f.specs["gt-a"]
		f.mu.Unlock()
		step := specStoryPointStep(env, "gt-a", latest)

		if len(step.Errors) != 0 {
			t.Errorf("errors = %q, want none", step.Errors)
		}
		if got := splitSlungLabels(f, "gt-a"); len(got) != 1 || got[0] != "sp-slung:shape" {
			t.Errorf("sp-slung labels after two ticks = %q, want exactly one sp-slung:shape", got)
		}
	})
	t.Run("a failed AddLabel is reported", func(t *testing.T) {
		t.Parallel()
		f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
		env := f.env()
		env.StoryPointSplitOver = 11
		env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
			return splitOutcome("estimated", 13, 12, "shape", ""), nil
		}
		env.AddLabel = func(string, string) error { return errors.New("bd refused") }
		r := runSpecDispatchCycle(env)

		if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "gt-a") ||
			!strings.Contains(r.Errors[0], "sp-slung:shape") || !strings.Contains(r.Errors[0], "bd refused") {
			t.Errorf("errors = %q, want one naming the bead, the label and the failed write", r.Errors)
		}
		if got := splitSlungLabels(f, "gt-a"); len(got) != 0 {
			t.Errorf("sp-slung labels = %q, want none written", got)
		}
		if got := strings.Join(f.slung, " "); got != "gt-a" {
			t.Errorf("slung %q, want gt-a: a failed reason write does not hold the bead", got)
		}
	})
}

// TestSplitLabelStale: the split label is stale only when the bead's own labels
// record an estimate the route acts on that reads under the trigger. A re-stamp
// of sp-minutes without the label updated is the case the function exists for;
// no readable estimate and the route off both leave the label standing.
func TestSplitLabelStale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		labels  []string
		trigger float64
		want    bool
	}{
		{"under the trigger", []string{"split", "sp-minutes:9.3", "sp-rung:all"}, 10, true},
		{"at the trigger", []string{"split", "sp-minutes:10", "sp-rung:all"}, 10, false},
		{"over the trigger", []string{"split", "sp-minutes:10.4", "sp-rung:all"}, 10, false},
		{"no minutes", []string{"split", "sp-rung:all"}, 10, false},
		{"unreadable minutes spelling", []string{"split", "sp-minutes:10.0", "sp-rung:all"}, 10, false},
		{"unreadable rung", []string{"split", "sp-minutes:9.3", "sp-rung:bogus"}, 10, false},
		{"trigger off", []string{"split", "sp-minutes:9.3", "sp-rung:all"}, 0, false},
		{"no split label", []string{"sp-minutes:9.3", "sp-rung:all"}, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specSplitLabelStale(tc.labels, tc.trigger); got != tc.want {
				t.Errorf("specSplitLabelStale(%v, %v) = %v, want %v", tc.labels, tc.trigger, got, tc.want)
			}
		})
	}
}

// TestSplitLabelStaleOnWeakRung: a split label recorded at a weak rung the
// route should not hold a bead on is stale whatever minutes it carries, while
// an acting rung keeps the minutes-under-trigger rule and a label set with no
// readable rung keeps its label (gt-ie2ehm).
func TestSplitLabelStaleOnWeakRung(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		labels []string
		want   bool
	}{
		{"shape-embed under the trigger", []string{"split", "sp-minutes:9.3", "sp-rung:shape-embed"}, true},
		{"shape-embed over the trigger", []string{"split", "sp-minutes:12.8", "sp-rung:shape-embed"}, true},
		{"shape rung over the trigger", []string{"split", "sp-minutes:12.8", "sp-rung:shape"}, true},
		{"shape-embed with no minutes", []string{"split", "sp-rung:shape-embed"}, true},
		{"an acting rung over the trigger", []string{"split", "sp-minutes:12.8", "sp-rung:all"}, false},
		{"an acting rung under the trigger", []string{"split", "sp-minutes:9.3", "sp-rung:all"}, true},
		{"no rung", []string{"split", "sp-minutes:12.8"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specSplitLabelStale(tc.labels, 10); got != tc.want {
				t.Errorf("specSplitLabelStale(%v, 10) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// splitStaleNotes returns the stale-drop notes the tick left on a fake town's
// bead.
func splitStaleNotes(f *fakeSpecTown, id string) []string {
	var out []string
	for _, n := range f.notes[id] {
		if strings.HasPrefix(n, specSplitStaleNotePrefix) {
			out = append(out, n)
		}
	}
	return out
}

// splitStaleNotices returns the tick's notices that say a split label is stale.
func splitStaleNotices(notices []string) []string {
	var out []string
	for _, n := range notices {
		if strings.Contains(n, "split label stale") {
			out = append(out, n)
		}
	}
	return out
}

// splitStaleLabels is the labels a bead whose own estimate has outlived its
// split label wears: 9.3 minutes under the 10-minute trigger.
func splitStaleLabels() []string {
	return []string{"sp:8", "sp-rung:all", "sp-minutes:9.3", specdispatch.SplitLabel}
}

// TestSplitLabelStaleDropped is gt-71y4v3: the board read takes a split label
// the bead's own estimate has outlived off, once, with one note, and offers the
// bead to the same tick's candidate scan; a label the estimate still stands up
// stays held with the line it had; and a removal that failed keeps the bead
// held and reports the error. A weak-rung label is dropped whatever its
// minutes, and its note names the rung rather than a trigger (gt-ie2ehm).
func TestSplitLabelStaleDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		labels     []string
		removeErr  error
		wantSlung  bool
		wantNote   []string
		refuseNote []string
	}{
		{name: "stale under the trigger", labels: splitStaleLabels(), wantSlung: true,
			wantNote: []string{"9.3 min", "under the 10-minute split trigger"}},
		{name: "a weak rung over the trigger",
			labels:     []string{"sp:8", "sp-rung:shape-embed", "sp-minutes:12.8", specdispatch.SplitLabel},
			wantSlung:  true,
			wantNote:   []string{"rung shape-embed", "should not hold a bead on"},
			refuseNote: []string{"trigger", "12.8"}},
		{name: "over the trigger", labels: []string{"sp:8", "sp-rung:all", "sp-minutes:10.4", specdispatch.SplitLabel}},
		{name: "a failed removal", labels: splitStaleLabels(), removeErr: errors.New("bd refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			held := cleanSpec("gt-split", 1, "2026-09-29T10:00:00Z")
			held.Labels = tc.labels
			f := newFakeSpecTown(held)
			if tc.removeErr != nil {
				f.unlabelErrs["gt-split"] = tc.removeErr
			}
			env := f.env()
			env.Candidates = f.productionCandidates(t, 10, false)
			env.StoryPointSplitOver = 10
			env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 8, 9.3, "all", ""), nil
			}
			r := runSpecDispatchCycle(env)

			if tc.wantSlung {
				if got := strings.Join(f.slung, " "); got != "gt-split" {
					t.Fatalf("slung %q, want the stale bead taken by this tick", got)
				}
				if slices.Contains(f.specs["gt-split"].Labels, specdispatch.SplitLabel) {
					t.Errorf("labels = %v, want the stale split label dropped", f.specs["gt-split"].Labels)
				}
				if got := f.unlabels; len(got) != 1 || got[0] != "gt-split:split" {
					t.Errorf("removals = %v, want the split label taken off once", got)
				}
				notes := splitStaleNotes(f, "gt-split")
				if len(notes) != 1 {
					t.Fatalf("stale notes = %q, want one", notes)
				}
				if strings.Contains(notes[0], "\n") {
					t.Errorf("stale note %q is not one line", notes[0])
				}
				for _, want := range tc.wantNote {
					if !strings.Contains(notes[0], want) {
						t.Errorf("stale note %q lacks %q", notes[0], want)
					}
				}
				for _, refuse := range tc.refuseNote {
					if strings.Contains(notes[0], refuse) {
						t.Errorf("stale note %q claims %q, which the labels do not stand up", notes[0], refuse)
					}
				}
				notices := splitStaleNotices(r.Notices)
				if len(notices) != 1 || !strings.Contains(notices[0], "offered to dispatch again") {
					t.Fatalf("notices = %q, want one saying the bead is offered again", notices)
				}
				for _, refuse := range tc.refuseNote {
					if strings.Contains(notices[0], refuse) {
						t.Errorf("notice %q claims %q, which the labels do not stand up", notices[0], refuse)
					}
				}
				if lines := splitHoldLines(r.Skipped); len(lines) != 0 {
					t.Errorf("split hold lines = %q, want none for a dropped label", lines)
				}
				return
			}
			if len(f.slung) != 0 {
				t.Fatalf("slung %v, want the bead held", f.slung)
			}
			if !slices.Contains(f.specs["gt-split"].Labels, specdispatch.SplitLabel) {
				t.Errorf("labels = %v, want the split label still on", f.specs["gt-split"].Labels)
			}
			if len(f.unlabels) != 0 {
				t.Errorf("removals = %v, want the label left on", f.unlabels)
			}
			// The note is written before the label comes off, so a removal
			// that failed leaves its record and the next tick's retry writes no
			// second copy.
			wantNotes := 0
			if tc.removeErr != nil {
				wantNotes = 1
				if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "gt-split") || !strings.Contains(r.Errors[0], "bd refused") {
					t.Errorf("errors = %q, want one naming the bead and the failed removal", r.Errors)
				}
			}
			if notes := splitStaleNotes(f, "gt-split"); len(notes) != wantNotes {
				t.Errorf("stale notes = %q, want %d", notes, wantNotes)
			}
			lines := splitHoldLines(r.Skipped)
			if len(lines) != 1 {
				t.Fatalf("split hold lines = %q, want the bead still reported held", lines)
			}
			if tc.removeErr == nil && !strings.Contains(lines[0], "10.4 min (sp:8, rung all), at or over the 10-minute trigger; held for splitting") {
				t.Errorf("hold line %q does not read as today's", lines[0])
			}
		})
	}
}

// A split label that landed after the board read is caught by the story-point
// step before the sling: the same drop leaves its note, takes the label off,
// and nothing holds the bead (gt-71y4v3).
func TestSplitLabelStaleDroppedMidTick(t *testing.T) {
	t.Parallel()
	held := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
	held.Labels = splitStaleLabels()
	f := newFakeSpecTown(held)
	env := f.env()
	env.StoryPointSplitOver = 10
	step := specStoryPointStep(env, "gt-a", held)

	if step.Hold != "" {
		t.Fatalf("hold = %q, want the stale label dropped and the bead offered", step.Hold)
	}
	if len(step.Errors) != 0 {
		t.Errorf("errors = %q, want none", step.Errors)
	}
	if notices := splitStaleNotices(step.Notices); len(notices) != 1 || !strings.Contains(notices[0], "offered to dispatch again") {
		t.Errorf("notices = %q, want one saying the bead is offered again", step.Notices)
	}
	if slices.Contains(f.specs["gt-a"].Labels, specdispatch.SplitLabel) {
		t.Errorf("labels = %v, want the stale split label dropped", f.specs["gt-a"].Labels)
	}
	if notes := splitStaleNotes(f, "gt-a"); len(notes) != 1 {
		t.Errorf("stale notes = %q, want one", notes)
	}
}

// TestSplitLabelStaleDropFailures: a note that cannot be written and a seam
// that is not wired both leave the label on, hold the bead, and are reported.
// A drop that cannot record its decision is never read as dropped.
func TestSplitLabelStaleDropFailures(t *testing.T) {
	t.Parallel()
	t.Run("note writer fails", func(t *testing.T) {
		t.Parallel()
		held := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
		held.Labels = splitStaleLabels()
		f := newFakeSpecTown(held)
		env := f.env()
		env.StoryPointSplitOver = 10
		env.Annotate = func(string, string, string) error { return errors.New("bd refused") }
		step := specStoryPointStep(env, "gt-a", held)

		if step.Hold == "" {
			t.Error("hold = \"\", want the bead held: the note did not land")
		}
		if len(step.Errors) != 1 || !strings.Contains(step.Errors[0], "bd refused") {
			t.Errorf("errors = %q, want one naming the failed note", step.Errors)
		}
		if !slices.Contains(f.specs["gt-a"].Labels, specdispatch.SplitLabel) || len(f.unlabels) != 0 {
			t.Errorf("labels = %v removals = %v, want the label left on", f.specs["gt-a"].Labels, f.unlabels)
		}
	})
	t.Run("no note writer or remover wired", func(t *testing.T) {
		t.Parallel()
		held := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
		held.Labels = splitStaleLabels()
		f := newFakeSpecTown(held)
		env := f.env()
		env.StoryPointSplitOver = 10
		env.RemoveLabel = nil
		step := specStoryPointStep(env, "gt-a", held)

		if step.Hold == "" {
			t.Error("hold = \"\", want the bead held: the remover is not wired")
		}
		if len(step.Errors) != 1 || !strings.Contains(step.Errors[0], "no note writer and label remover are wired") {
			t.Errorf("errors = %q, want one naming the unwired seam", step.Errors)
		}
		if !slices.Contains(f.specs["gt-a"].Labels, specdispatch.SplitLabel) || len(f.unlabels) != 0 {
			t.Errorf("labels = %v removals = %v, want the label left on", f.specs["gt-a"].Labels, f.unlabels)
		}
	})
}

// TestSplitLabelStaleDryRun: a dry run removes nothing and says the label is
// stale, on the board read's held list and on a label that landed after it.
func TestSplitLabelStaleDryRun(t *testing.T) {
	t.Parallel()
	t.Run("board read", func(t *testing.T) {
		t.Parallel()
		held := cleanSpec("gt-split", 1, "2026-09-29T10:00:00Z")
		held.Labels = splitStaleLabels()
		f := newFakeSpecTown(held)
		env := f.env()
		env.Candidates = f.productionCandidates(t, 10, true)
		env.StoryPointSplitOver = 10
		env.DryRun = true
		r := runSpecDispatchCycle(env)

		if !slices.Contains(f.specs["gt-split"].Labels, specdispatch.SplitLabel) {
			t.Errorf("labels = %v, want the stale split label left on by a dry run", f.specs["gt-split"].Labels)
		}
		if len(f.unlabels) != 0 || len(f.notes) != 0 {
			t.Errorf("a dry run wrote removals %v and notes %v", f.unlabels, f.notes)
		}
		notices := splitStaleNotices(r.Notices)
		if len(notices) != 1 {
			t.Fatalf("notices = %q, want one saying the label is stale", notices)
		}
		for _, want := range []string{"gt-split", "9.3 min", "a dry run removes nothing"} {
			if !strings.Contains(notices[0], want) {
				t.Errorf("notice %q lacks %q", notices[0], want)
			}
		}
		for _, e := range r.Skipped {
			if e.Bead == "gt-split" {
				t.Errorf("skipped %q, want the stale bead not read as held", e.Line)
			}
		}
	})
	t.Run("after the board read", func(t *testing.T) {
		t.Parallel()
		held := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
		held.Labels = splitStaleLabels()
		f := newFakeSpecTown(held)
		env := f.env()
		env.StoryPointSplitOver = 10
		env.DryRun = true
		step := specStoryPointStep(env, "gt-a", held)

		if step.Hold != "" {
			t.Fatalf("hold = %q, want a dry run to drop nothing and hold nothing", step.Hold)
		}
		if notices := splitStaleNotices(step.Notices); len(notices) != 1 {
			t.Fatalf("notices = %q, want one saying the label is stale", step.Notices)
		}
		if !slices.Contains(f.specs["gt-a"].Labels, specdispatch.SplitLabel) || len(f.unlabels) != 0 || len(f.notes) != 0 {
			t.Errorf("a dry run wrote: labels %v removals %v notes %v", f.specs["gt-a"].Labels, f.unlabels, f.notes)
		}
	})
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

// TestSpecDispatchSplitRouteNoteFailureRetriesNextTick: a split note that fails
// on the first tick leaves the bead unlabeled, so the next tick judges it again
// and writes the note. The label is written only after the note lands, so the
// guard that holds an already-labeled bead never skips a note that is missing.
func TestSpecDispatchSplitRouteNoteFailureRetriesNextTick(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	env := f.env()
	env.StoryPointSplitOver = 15
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 13, 20, "all", "nearest beads: none"), nil
	}
	noteWrites := 0
	annotate := env.Annotate
	env.Annotate = func(id, key, text string) error {
		noteWrites++
		if noteWrites == 1 {
			return errors.New("bd refused")
		}
		return annotate(id, key, text)
	}
	runSpecDispatchCycle(env)

	if labels := f.labels["gt-a"]; slices.Contains(labels, specdispatch.SplitLabel) {
		t.Fatalf("labels = %v after a failed split note, want the split label not yet written", labels)
	}

	r := runSpecDispatchCycle(env)

	if notes := splitNotes(f, "gt-a"); len(notes) != 1 {
		t.Errorf("split notes = %q after the retry, want exactly one", notes)
	}
	splitLabels := 0
	for _, l := range f.labels["gt-a"] {
		if l == specdispatch.SplitLabel {
			splitLabels++
		}
	}
	if splitLabels != 1 {
		t.Errorf("split label written %d time(s) after the retry, want once: %v", splitLabels, f.labels["gt-a"])
	}
	if len(f.slung) != 0 {
		t.Errorf("slung %v after the retry, want the bead held", f.slung)
	}
	if lines := splitHoldLines(r.Skipped); len(lines) != 1 {
		t.Errorf("split hold lines = %q after the retry, want the bead reported held", lines)
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

// A bead the split route holds is not slung and is reported by the tick as
// skipped, with the route named, the minutes its labels record and the trigger
// they are at or over: that line is the ledger's entry, and the dashboard's
// ready row for the bead is what reads it to stop counting the bead as
// dispatchable (gt-4yq0js).
func TestSpecDispatchReportsASplitHeldBeadSkipped(t *testing.T) {
	t.Parallel()
	held := cleanSpec("gt-split", 1, "2026-09-29T10:00:00Z")
	held.Labels = []string{"sp:8", "sp-rung:all", "sp-minutes:10.4", specdispatch.SplitLabel}
	f := newFakeSpecTown(held, cleanSpec("gt-work", 1, "2026-09-29T11:00:00Z"))
	env := f.env()
	env.Candidates = f.productionCandidates(t, 10, false)
	env.StoryPointSplitOver = 10
	env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
		return splitOutcome("estimated", 3, 4, "all", ""), nil
	}
	r := runSpecDispatchCycle(env)

	if got := strings.Join(f.slung, " "); got != "gt-work" {
		t.Fatalf("slung %q, want only the bead no route holds", got)
	}
	lines := splitHoldLines(r.Skipped)
	if len(lines) != 1 {
		t.Fatalf("split hold lines = %q, want the split-held bead reported once", lines)
	}
	for _, want := range []string{"gt-split: split: ", "estimated 10.4 min (sp:8, rung all)", "at or over the 10-minute trigger"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("skip line %q lacks %q", lines[0], want)
		}
	}
	verdicts := specTickVerdicts(r)
	if len(verdicts) != 1 || verdicts[0].Bead != "gt-split" || verdicts[0].Class != specVerdictSkipped || verdicts[0].Reason != lines[0] {
		t.Errorf("ledger = %+v, want one skipped entry for gt-split carrying the line", verdicts)
	}
}

// A split-labelled bead the route's labels no longer stand up is still held —
// the label is the split's record — so it is still reported, and the line says
// what the labels say instead of claiming a trigger that does not hold. Rows:
// no estimate at all, minutes with no rung, the trigger off (gt-4yq0js), and a
// weak rung whose label the tick could not remove (gt-ie2ehm). A label the tick
// reads as stale is not in this table: it drops the label and offers the bead
// instead of holding it (gt-71y4v3, TestSplitLabelStaleDropped).
func TestSpecDispatchReportsASplitHeldBeadItsLabelsDoNotExplain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		labels  []string
		trigger float64
		// removeErr makes the stale label's removal fail, which is the one way
		// an estimate under the trigger is still held (gt-71y4v3).
		removeErr error
		want      []string
		refuse    []string
	}{
		{
			name:    "no estimate at all",
			labels:  []string{specdispatch.SplitLabel},
			trigger: 11,
			want:    []string{"gt-split: split: ", "its labels name no estimate the route can read"},
			refuse:  []string{"trigger"},
		},
		{
			name:    "minutes with no rung",
			labels:  []string{specdispatch.SplitLabel, "sp-minutes:10.4"},
			trigger: 11,
			want:    []string{"gt-split: split: ", "records no rung"},
			refuse:  []string{"trigger", "10.4"},
		},
		{
			// The stale drop is the only thing that stands between an estimate
			// under the trigger and the bead, so the line is read here on the
			// path where that drop could not take the label off.
			name:      "estimate under the trigger, the removal failed",
			labels:    []string{specdispatch.SplitLabel, "sp:3", "sp-rung:all", "sp-minutes:4"},
			trigger:   11,
			removeErr: errors.New("bd refused"),
			want:      []string{"gt-split: split: ", "estimated 4.0 min (sp:3, rung all)"},
			refuse:    []string{"trigger", "at or over"},
		},
		{
			name:    "the trigger is off now",
			labels:  []string{specdispatch.SplitLabel, "sp:8", "sp-rung:all", "sp-minutes:10.4"},
			trigger: 0,
			want:    []string{"gt-split: split: ", "estimated 10.4 min (sp:8, rung all)"},
			refuse:  []string{"trigger", "at or over"},
		},
		{
			// A weak rung makes the label stale, so a failed removal is the one
			// way the bead is still held (gt-ie2ehm).
			name:      "a shape-only rung, the removal failed",
			labels:    []string{specdispatch.SplitLabel, "sp-rung:shape", "sp-minutes:10.4"},
			trigger:   11,
			removeErr: errors.New("bd refused"),
			want:      []string{"gt-split: split: ", "estimated 10.4 min (sp:8, rung shape)"},
			refuse:    []string{"trigger", "at or over"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			held := cleanSpec("gt-split", 1, "2026-09-29T10:00:00Z")
			held.Labels = tc.labels
			f := newFakeSpecTown(held)
			if tc.removeErr != nil {
				f.unlabelErrs["gt-split"] = tc.removeErr
			}
			env := f.env()
			env.Candidates = f.productionCandidates(t, tc.trigger, false)
			env.StoryPointSplitOver = tc.trigger
			r := runSpecDispatchCycle(env)

			if len(f.slung) != 0 {
				t.Fatalf("slung %v, want the split-held bead left alone", f.slung)
			}
			lines := splitHoldLines(r.Skipped)
			if len(lines) != 1 {
				t.Fatalf("split hold lines = %q, want the bead reported once", lines)
			}
			for _, want := range tc.want {
				if !strings.Contains(lines[0], want) {
					t.Errorf("skip line %q lacks %q", lines[0], want)
				}
			}
			for _, refuse := range tc.refuse {
				if strings.Contains(lines[0], refuse) {
					t.Errorf("skip line %q claims %q, which the labels do not stand up", lines[0], refuse)
				}
			}
			if verdicts := specTickVerdicts(r); len(verdicts) != 1 || verdicts[0].Class != specVerdictSkipped {
				t.Errorf("ledger = %+v, want one skipped entry", verdicts)
			}
		})
	}
}

// TestSpecSplitHysteresis: a bead that already carries an sp-minutes label under
// the trigger is held only when its new estimate clears the trigger by
// specSplitHysteresis, so estimate noise near the trigger does not flip it
// between dispatchable and held. A bead with no earlier label, or one already at
// or over the trigger, is judged by the trigger alone.
func TestSpecSplitHysteresis(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		labels   []string
		minutes  float64
		wantHeld bool
	}{
		{name: "an earlier label under the trigger, re-estimated inside the band", labels: []string{"sp-minutes:10.9"}, minutes: 11.3},
		{name: "an earlier label under the trigger, re-estimated past the band", labels: []string{"sp-minutes:10.9"}, minutes: 11.6, wantHeld: true},
		{name: "no earlier label", minutes: 11.3, wantHeld: true},
		{name: "an earlier label already at the trigger", labels: []string{"sp-minutes:11.2"}, minutes: 11.3, wantHeld: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")
			spec.Labels = tc.labels
			f := newFakeSpecTown(spec)
			env := f.env()
			env.After = neverTimer
			env.StoryPointSplitOver = 11
			env.StoryPointEstimate = func(context.Context, string) (specEstimateOutcome, error) {
				return splitOutcome("estimated", 13, tc.minutes, "all", "nearest beads: gt-b"), nil
			}
			runSpecDispatchCycle(env)

			held := slices.Contains(f.labels["gt-a"], specdispatch.SplitLabel)
			if held != tc.wantHeld {
				t.Fatalf("held = %v at %.1f minutes, want %v (labels %v)", held, tc.minutes, tc.wantHeld, f.labels["gt-a"])
			}
			if tc.wantHeld && len(f.slung) != 0 {
				t.Errorf("slung %v, want the bead held", f.slung)
			}
			if !tc.wantHeld && strings.Join(f.slung, " ") != "gt-a" {
				t.Errorf("slung %v, want gt-a", f.slung)
			}
		})
	}
}
