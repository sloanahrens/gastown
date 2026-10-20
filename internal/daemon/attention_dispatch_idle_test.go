package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// idleTick is a tick report with one free seat and the given skipped lines.
func idleTick(skipped ...string) specDispatchTickReport {
	r := specDispatchTickReport{Roster: "claude-sonnet 1/3", Candidates: len(skipped)}
	for _, line := range skipped {
		bead, _, _ := strings.Cut(line, ":")
		r.Skipped = append(r.Skipped, specDispatchTickEntry{Bead: bead, Line: line})
	}
	return r
}

const (
	overlapLine = "gt-aaaaa: sling deferred, content overlap clears when the overlapping bead closes: gt-zzzzz holds internal/cmd/spec.go"
	heldLine    = "gt-bbbbb: held: gt-yyyyy is already hooked to a working seat and names internal/cmd/spec.go"
	unshapedLn  = "gt-ccccc: unshaped: Goal, Gate"
)

// idleRig drives the tracker and the collector the way the daemon does: the
// ticker observes a report, the heartbeat collects.
type idleRig struct {
	f    *attentionFixture
	tr   *dispatchIdleTracker
	hold string
	now  time.Time
}

func newIdleRig(t *testing.T) *idleRig {
	t.Helper()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	r := &idleRig{f: newAttentionFixture(t, now), tr: &dispatchIdleTracker{}, now: now}
	r.f.src.dispatchIdle = r.tr.Status
	r.f.src.dispatchHold = func() string { return r.hold }
	return r
}

// observe feeds n copies of report, one minute apart.
func (r *idleRig) observe(report specDispatchTickReport, n int) {
	for i := 0; i < n; i++ {
		r.now = r.now.Add(time.Minute)
		r.tr.Observe(report, r.now)
	}
}

func (r *idleRig) items(t *testing.T) []attention.Item {
	t.Helper()
	r.f.src.now = r.now
	return r.f.collect(t, r.f.src.collectDispatchIdle)
}

func TestDispatchIdleRaisesAfterFiveIdleTicks(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	tick := idleTick(overlapLine, overlapLine, overlapLine, heldLine, unshapedLn)
	tick.Skipped[1].Bead, tick.Skipped[1].Line = "gt-ddddd", strings.Replace(overlapLine, "gt-aaaaa", "gt-ddddd", 1)
	tick.Skipped[2].Bead, tick.Skipped[2].Line = "gt-eeeee", strings.Replace(overlapLine, "gt-aaaaa", "gt-eeeee", 1)

	r.observe(tick, attentionDispatchIdleTicks-1)
	if got := r.items(t); len(got) != 0 {
		t.Fatalf("raised after %d ticks: %+v", attentionDispatchIdleTicks-1, got)
	}
	r.observe(tick, 1)
	items := r.items(t)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	it := items[0]
	if it.Kind != attention.KindDispatchIdle || it.Key != dispatchIdleKey || it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v", it)
	}
	if rule := attention.RuleFor(it); rule != attention.RuleDispatchIdle {
		t.Errorf("rule = %q", rule)
	}
	for _, want := range []string{
		"idle for 5 ticks",
		"claude-sonnet 1/3",
		"4 dispatchable",
		"sling deferred, content overlap clears when the overlapping bead closes x3",
		"held: <bead> is already hooked to a working seat x1",
	} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("summary %q lacks %q", it.Summary, want)
		}
	}
	if strings.Contains(it.Summary, "unshaped") {
		t.Errorf("summary names an unshaped bead as a skip reason: %q", it.Summary)
	}
}

// gt-icmdq: the ticker was parked or failing between two observations, so the
// idle ticks before the gap and the one after it are not a consecutive run.
// Without this, four idle ticks, an hour's hold and one more idle tick raise
// the HIGH item after one tick of a resumed streak.
func TestDispatchIdleStreakResetsAcrossAGap(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	tick := idleTick(overlapLine)

	r.observe(tick, attentionDispatchIdleTicks-1)
	r.now = r.now.Add(time.Hour)
	r.tr.Observe(tick, r.now)

	if got := r.tr.Status().Streak; got != 1 {
		t.Fatalf("Streak = %d after an idle tick an hour on, want 1", got)
	}
	if items := r.items(t); len(items) != 0 {
		t.Fatalf("raised after one idle tick of a resumed streak: %+v", items)
	}
}

// gt-icmdq: the gap between consecutive ticks is a multiple of the ticker's own
// cadence. A fixed five minutes ended the streak on every tick of a town that
// configured a slower ticker, so the HIGH item could never raise at all.
func TestDispatchIdleStreakSurvivesASlowTicker(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	const interval = 10 * time.Minute
	r.tr.setInterval(interval)
	tick := idleTick(overlapLine)

	for i := 0; i < attentionDispatchIdleTicks; i++ {
		r.now = r.now.Add(interval)
		r.tr.Observe(tick, r.now)
	}
	if got := r.tr.Status().Streak; got != attentionDispatchIdleTicks {
		t.Fatalf("Streak = %d after %d idle ticks %s apart, want %d",
			got, attentionDispatchIdleTicks, interval, attentionDispatchIdleTicks)
	}
	if items := r.items(t); len(items) != 1 {
		t.Fatalf("items = %+v, want the idle item raised on a slow ticker", items)
	}

	// The gap scales with the cadence rather than vanishing: a ticker parked
	// past its own gap still ends the run.
	r.now = r.now.Add(attentionDispatchIdleGapIntervals*interval + time.Minute)
	r.tr.Observe(tick, r.now)
	if got := r.tr.Status().Streak; got != 1 {
		t.Fatalf("Streak = %d after a tick past the scaled gap, want 1", got)
	}
}

// gt-icmdq: the bound on how old the newest tick may be is the ticker's own
// cadence too. A ten-minute-bound ticker would otherwise have its streak read
// as stale for all but the minute after each of its ticks, and the HIGH item
// could raise only in the seconds right after one.
func TestDispatchIdleStaysFreshForASlowTicker(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	const interval = 10 * time.Minute
	r.tr.setInterval(interval)
	r.observe(idleTick(overlapLine), attentionDispatchIdleTicks)

	// Two intervals on, before the ticker's next tick is due: the run still
	// describes now.
	r.now = r.now.Add(2 * interval)
	if items := r.items(t); len(items) != 1 {
		t.Fatalf("items = %+v, want the idle item to survive two slow intervals", items)
	}

	// Past the ticker's own gap, the ticker is off or failing, and a streak
	// that old no longer describes now.
	r.now = r.now.Add(attentionDispatchIdleGapIntervals * interval)
	if items := r.items(t); len(items) != 0 {
		t.Fatalf("items = %+v, want none: the newest tick is older than the cadence allows", items)
	}
}

func TestDispatchIdleNamesOnlyTheTopThreeReasons(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	r.observe(idleTick(
		"gt-aaaaa: sling deferred: x", "gt-bbbbb: sling deferred: y",
		"gt-ccccc: held: red-main revert of gt-rrrrr in flight",
		"gt-ddddd: the ready board read is stale: z",
		"gt-eeeee: held: gt-yyyyy is already hooked to a working seat and names a.go",
	), attentionDispatchIdleTicks)
	items := r.items(t)
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	if n := strings.Count(items[0].Summary, " x"); n != attentionDispatchIdleReasons {
		t.Errorf("summary names %d reasons, want %d: %q", n, attentionDispatchIdleReasons, items[0].Summary)
	}
	if !strings.Contains(items[0].Summary, "sling deferred x2") {
		t.Errorf("commonest reason missing: %q", items[0].Summary)
	}
}

// A bead whose formula declaration is bad cannot start however many seats are
// free, so it is not waiting on the dispatcher.
func TestDispatchableWaitingIgnoresBadFormula(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"gt-abcde: conflicting formula labels: a, b",
		"gt-abcde: bad formula vars: x",
		"gt-abcde: bad formula label: y",
	} {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			report := idleTick(line)
			if waiting, reasons := dispatchableWaiting(report); waiting != 0 || len(reasons) != 0 {
				t.Fatalf("waiting = %d, reasons = %+v; want none", waiting, reasons)
			}
			r := newIdleRig(t)
			r.observe(report, attentionDispatchIdleTicks+3)
			if st := r.tr.Status(); st.Streak != 0 {
				t.Fatalf("streak = %d, want 0", st.Streak)
			}
			if got := r.items(t); len(got) != 0 {
				t.Fatalf("raised: %+v", got)
			}
		})
	}
}

func TestDispatchIdleDoesNotRaise(t *testing.T) {
	t.Parallel()
	dispatched := idleTick(overlapLine)
	dispatched.Dispatched = []specDispatchTickEntry{{Bead: "gt-fffff", Line: "gt-fffff: slung"}}
	held := idleTick(overlapLine)
	held.Hold = "operator dispatch hold present"
	for _, tc := range []struct {
		name   string
		report specDispatchTickReport
	}{
		{"a dispatch happened", dispatched},
		{"only unshaped beads wait", idleTick(unshapedLn, "gt-ggggg: not eligible (crew-only)", "gt-hhhhh: not eligible (needs-human)")},
		{"only seat skips wait", idleTick("gt-iiiii: no seat: claude-sonnet 3/3", "gt-jjjjj: rig hold: estop", "gt-kkkkk: clean, per-tick limit 1 reached")},
		{"the tick was held", held},
		{"the roster is full", func() specDispatchTickReport { r := idleTick(overlapLine); r.Roster = "claude-sonnet 3/3"; return r }()},
		{"no seats at all", func() specDispatchTickReport { r := idleTick(overlapLine); r.Roster = "no seats"; return r }()},
		{"the roster is unreadable", func() specDispatchTickReport { r := idleTick(overlapLine); r.Roster = ""; return r }()},
		{"nothing is waiting", idleTick()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newIdleRig(t)
			r.observe(tc.report, attentionDispatchIdleTicks+3)
			if got := r.items(t); len(got) != 0 {
				t.Fatalf("raised: %+v", got)
			}
		})
	}
}

func TestDispatchIdleStreakMustBeConsecutive(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	idle := idleTick(overlapLine)
	r.observe(idle, attentionDispatchIdleTicks-1)
	full := idleTick(overlapLine)
	full.Roster = "claude-sonnet 3/3"
	r.observe(full, 1)
	r.observe(idle, attentionDispatchIdleTicks-1)
	if got := r.items(t); len(got) != 0 {
		t.Fatalf("raised across a broken streak: %+v", got)
	}
	r.observe(idle, 1)
	if got := r.items(t); len(got) != 1 {
		t.Fatalf("items = %+v, want one after a fresh run of %d", got, attentionDispatchIdleTicks)
	}
}

func TestDispatchIdleClears(t *testing.T) {
	t.Parallel()
	idle := idleTick(overlapLine)
	dispatched := idleTick(overlapLine)
	dispatched.Dispatched = []specDispatchTickEntry{{Bead: "gt-fffff", Line: "gt-fffff: slung"}}
	for _, tc := range []struct {
		name  string
		clear func(r *idleRig)
	}{
		{"a dispatch happens", func(r *idleRig) { r.observe(dispatched, 1) }},
		{"no dispatchable candidate remains", func(r *idleRig) { r.observe(idleTick(unshapedLn), 1) }},
		{"a tick is held", func(r *idleRig) {
			h := idle
			h.Hold = "operator dispatch hold present"
			r.observe(h, 1)
		}},
		{"the operator hold appears", func(r *idleRig) { r.hold = "operator dispatch hold present" }},
		{"the ticker goes quiet", func(r *idleRig) { r.now = r.now.Add(attentionDispatchIdleFresh + time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newIdleRig(t)
			r.observe(idle, attentionDispatchIdleTicks)
			if got := r.items(t); len(got) != 1 {
				t.Fatalf("not raised: %+v", got)
			}
			tc.clear(r)
			if got := r.items(t); len(got) != 0 {
				t.Fatalf("still raised: %+v", got)
			}
		})
	}
}

// Through a full attention tick the rule is an operator_action that wakes, and
// the next tick without the condition writes its cleared line.
func TestDispatchIdleWakesThroughTheQueueAndClears(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	r.observe(idleTick(overlapLine), attentionDispatchIdleTicks)
	st := r.f.tick(t, r.now)
	it := itemByKey(t, st, dispatchIdleKey)
	if it.Kind != attention.KindDispatchIdle {
		t.Fatalf("item = %+v", it)
	}
	evs := r.f.events(t)
	var raised *attention.Event
	for i := range evs {
		if evs[i].Rule == attention.RuleDispatchIdle && evs[i].State == attention.EventNew {
			raised = &evs[i]
		}
	}
	if raised == nil {
		t.Fatalf("no raised event in %+v", evs)
	}
	if !raised.Wake || raised.Handled != attention.HandledOperatorAction {
		t.Errorf("event = %+v, want operator_action and wake", *raised)
	}

	dispatched := idleTick(overlapLine)
	dispatched.Dispatched = []specDispatchTickEntry{{Bead: "gt-fffff", Line: "gt-fffff: slung"}}
	r.observe(dispatched, 1)
	st = r.f.tick(t, r.now)
	if _, ok := attention.Find(st, dispatchIdleKey); ok {
		t.Errorf("item survived a dispatch: %+v", st.Items)
	}
}

func TestDispatchIdleReasonKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"sling deferred, content overlap clears when the overlapping bead closes: gt-zzzzz holds a.go": "sling deferred, content overlap clears when the overlapping bead closes",
		"held: gt-yyyyy is already hooked to a working seat and names a.go, b.go (+2 more)":            "held: <bead> is already hooked to a working seat",
		"held: red-main revert of gt-rrrrr in flight":                                                  "held: red-main revert of <bead> in flight",
		"the ready board read is stale: re-read failed: boom":                                          "the ready board read is stale",
	} {
		if got := dispatchIdleReasonKey(in); got != want {
			t.Errorf("dispatchIdleReasonKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// The tracker reads the JSON a tick prints: the bead and line of each skipped
// entry, and the roster string.
func TestDispatchIdleTrackerReadsTheTickJSON(t *testing.T) {
	t.Parallel()
	report := tickReport(t, `{"roster": "claude-sonnet 1/3", "candidates": 2, "dispatched": null,
		"skipped": [
			{"bead": "gt-aaaaa", "line": "gt-aaaaa: sling deferred, content overlap clears when the overlapping bead closes: gt-zzzzz holds a.go"},
			{"bead": "gt-bbbbb", "line": "gt-bbbbb: unshaped: Goal"}]}`)
	tr := &dispatchIdleTracker{}
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tr.Observe(report, at)
	st := tr.Status()
	if st.Streak != 1 || st.Waiting != 1 || len(st.Reasons) != 1 || !st.At.Equal(at) {
		t.Errorf("status = %+v, want a streak of 1 with one waiting bead", st)
	}
}
