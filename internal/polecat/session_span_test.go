package polecat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/polecat/spans"
	"github.com/steveyegge/gastown/internal/rig"
)

// spanStopEpoch is the time the fixture's attempt started, and spanStopAt when
// the fixture's stop happens: both fixed, so nothing here depends on how long
// the test took.
var (
	spanStopEpoch = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	spanStopAt    = spanStopEpoch.Add(30 * time.Second)
)

// spanStopFixture is a town whose rig holds polecat Toast with an attempt
// already started in the town's span log: the state every path that stops a
// seat ends from. Nothing here opens a bead store — a stop must not need one.
func spanStopFixture(t *testing.T) (*SessionManager, *spans.Store) {
	t.Helper()
	town := t.TempDir()
	store := spans.New(spans.DefaultDir(town), &spanStopClock{at: spanStopEpoch})
	if err := store.Start("gt-abc", "test-rig/Toast"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	tm := newFakeSessionTmux()
	m := &SessionManager{
		tmux: tm,
		rig:  &rig.Rig{Name: "test-rig", Path: filepath.Join(town, "test-rig"), Polecats: []string{"Toast"}},
		gits: newWorld().opener(),
		clk:  clockwork.NewFakeClockAt(spanStopAt),
	}
	if err := tm.NewSession(m.SessionName("Toast"), ""); err != nil {
		t.Fatal(err)
	}
	return m, store
}

// spanStopClock is a fixed spans.Clock.
type spanStopClock struct{ at time.Time }

func (c *spanStopClock) Now() time.Time { return c.at }

// TestStopEndsTheSpanOfTheSeatItStops: a stop is the last the host sees of a
// seat that reports no end of its own — a sandboxed one cannot write this
// town's log — so the stop closes the attempt it held (gt-8yk78).
func TestStopEndsTheSpanOfTheSeatItStops(t *testing.T) {
	t.Parallel()
	m, store := spanStopFixture(t)

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Fatalf("summary = %+v, want the seat's attempt closed", sum)
	}
	if a := sum.Attempts[0]; a.Seat != "test-rig/Toast" || !a.LowerBound {
		t.Errorf("attempt = %+v, want test-rig/Toast with a lower bound", a)
	}
	if !sum.LowerBound {
		t.Error("LowerBound = false, want true: a resumed seat continues this attempt")
	}
}

// TestStopStampsTheEndWithTheManagersClock: the end is dated at the stop, not
// at a time of the store's choosing, so the seconds a landing reports are the
// time the seat was held (gt-8yk78).
func TestStopStampsTheEndWithTheManagersClock(t *testing.T) {
	t.Parallel()
	m, store := spanStopFixture(t)
	m.clk = clockwork.NewFakeClockAt(spanStopEpoch.Add(90 * time.Second))

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Secs != 90 {
		t.Errorf("summary = %+v, want one attempt of 90s", sum)
	}
}

// TestStopThroughTheStopHookAlsoEndsTheSpan: the operator verbs that stop a
// seat route the kill through the supervisor's stop hook, and the span ends
// whichever way the kill went (gt-4k3fj.4.1, gt-8yk78).
func TestStopThroughTheStopHookAlsoEndsTheSpan(t *testing.T) {
	t.Parallel()
	m, store := spanStopFixture(t)
	m.SetHooks(SessionHooks{Stop: func(string) error { return nil }})

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt closed through the hook's stop too", sum)
	}
}

// TestStopWritesNoEndForASeatThatReportedItsOwn: a seat that ran gt done
// already ended its attempt, and a repeat stop on a bead with no open attempt
// appends no second end (gt-liseq).
func TestStopWritesNoEndForASeatThatReportedItsOwn(t *testing.T) {
	t.Parallel()
	m, store := spanStopFixture(t)
	if err := store.End("gt-abc", "test-rig/Toast", spans.ReasonDone, spanStopEpoch.Add(time.Minute)); err != nil {
		t.Fatalf("end span: %v", err)
	}

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	sum, err := store.Summarize("gt-abc")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Skipped != 0 || len(sum.Attempts) != 1 || sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want one attempt and no second end", sum)
	}
}

// TestStopWithNoOpenSpanWritesNothing: a seat holding no bead has no attempt to
// end, so the stop writes nothing to the log (gt-8yk78).
func TestStopWithNoOpenSpanWritesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tm := newFakeSessionTmux()
	m := &SessionManager{
		tmux: tm,
		rig:  &rig.Rig{Name: "test-rig", Path: filepath.Join(town, "test-rig"), Polecats: []string{"Toast"}},
		gits: newWorld().opener(),
	}
	if err := tm.NewSession(m.SessionName("Toast"), ""); err != nil {
		t.Fatal(err)
	}
	m.clk = clockwork.NewFakeClockAt(spanStopAt)

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(spans.DefaultDir(town)); !os.IsNotExist(err) {
		t.Errorf("a stop with no attempt created the span store: %v", err)
	}
}

// TestStopSurvivesASpanStoreItCannotRead: the span is a measurement, so a store
// that cannot answer costs the time and not the stop the operator asked for,
// and the operator can see that it did: the failed end is warned with the seat
// it could not close (gt-8yk78).
func TestStopSurvivesASpanStoreItCannotRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A regular file where the store's directory belongs: every read of the
	// store fails.
	if err := os.MkdirAll(filepath.Dir(spans.DefaultDir(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spans.DefaultDir(town), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	tm := newFakeSessionTmux()
	m := &SessionManager{
		tmux: tm,
		rig:  &rig.Rig{Name: "test-rig", Path: filepath.Join(town, "test-rig"), Polecats: []string{"Toast"}},
		gits: newWorld().opener(),
		clk:  clockwork.NewFakeClockAt(spanStopAt),
	}
	var warned []string
	m.warn = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	if err := tm.NewSession(m.SessionName("Toast"), ""); err != nil {
		t.Fatal(err)
	}

	if err := m.Stop("Toast", true); err != nil {
		t.Fatalf("Stop = %v, want the stop to stand", err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "test-rig/Toast") {
		t.Errorf("warned = %q, want one line naming test-rig/Toast's span", warned)
	}
}
