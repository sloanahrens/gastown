package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/land"
)

// openRiskLandings makes the fixture's risk-path collector see n unreviewed
// landings. It returns a setter, so a test can move the count between ticks.
func openRiskLandings(f *attentionFixture, landed time.Time) func(n int) {
	n := 0
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) {
		recs := make([]land.LandingRecord, 0, n)
		for i := 0; i < n; i++ {
			recs = append(recs, land.LandingRecord{
				BeadID:    fmt.Sprintf("gt-b%02d", i),
				Head:      strings.Repeat(fmt.Sprintf("%x", i%16), 40),
				RiskPaths: []string{"internal/land/land.go"},
				LandedAt:  landed,
			})
		}
		return recs, nil
	}
	return func(count int) { n = count }
}

// backlogWakes is the review.backlog events that wake, raised only.
func backlogWakes(t *testing.T, f *attentionFixture) []attention.Event {
	t.Helper()
	var out []attention.Event
	for _, e := range f.events(t) {
		if e.Rule == attention.RuleReviewBacklog && e.State == attention.EventNew && e.Wake {
			out = append(out, e)
		}
	}
	return out
}

// 16 open risk-path items are past the default threshold of 15: one wake, whose
// text names the total and the one-hour change.
func TestAttentionReviewBacklogRaisesPastThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	setOpen := openRiskLandings(f, now)

	setOpen(16)
	st := f.tick(t, now)
	it := itemByKey(t, st, reviewBacklogKey)
	if it.Kind != attention.KindReviewBacklog || !strings.Contains(it.Summary, "16 open") || !strings.Contains(it.Summary, "+0 in the last hour") {
		t.Errorf("item = %+v, want the total 16 and the one-hour change in the summary", it)
	}
	wakes := backlogWakes(t, f)
	if len(wakes) != 1 || wakes[0].Handled != attention.HandledOperatorAction || wakes[0].Key != reviewBacklogKey {
		t.Fatalf("wake events = %+v, want one operator_action raise", wakes)
	}
	if !strings.Contains(wakes[0].Text, "16 open") {
		t.Errorf("wake text = %q, want it to name the total", wakes[0].Text)
	}
}

// 14 items under the threshold, or a slow rise under the limit, raise nothing.
func TestAttentionReviewBacklogQuietBelowLimits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	setOpen := openRiskLandings(f, now)

	setOpen(14)
	f.tick(t, now)

	// 3 -> 13 across the hour is a rise of 10, which is not more than 10, and
	// 13 is under the threshold.
	setOpen(3)
	at := now.Add(time.Minute)
	f.tick(t, at)
	setOpen(13)
	st := f.tick(t, at.Add(59*time.Minute))

	if _, ok := attention.Find(st, reviewBacklogKey); ok {
		t.Errorf("state = %+v, want no review:backlog item", st.Items)
	}
	for _, e := range f.events(t) {
		if e.Rule == attention.RuleReviewBacklog {
			t.Errorf("event %+v, want no review.backlog line", e)
		}
	}
}

// A rise of more than 10 within the hour raises under the threshold, and the
// summary carries the rise. The same climb spread over more than an hour does
// not.
func TestAttentionReviewBacklogRaisesOnFastRise(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	f := newAttentionFixture(t, now)
	setOpen := openRiskLandings(f, now)
	setOpen(2)
	f.tick(t, now)
	setOpen(13)
	st := f.tick(t, now.Add(30*time.Minute))
	it := itemByKey(t, st, reviewBacklogKey)
	if !strings.Contains(it.Summary, "13 open") || !strings.Contains(it.Summary, "+11 in the last hour") {
		t.Errorf("summary = %q, want the total 13 and +11", it.Summary)
	}
	if n := len(backlogWakes(t, f)); n != 1 {
		t.Errorf("wake events = %d, want 1", n)
	}

	slow := newAttentionFixture(t, now)
	setSlow := openRiskLandings(slow, now)
	setSlow(2)
	slow.tick(t, now)
	setSlow(13)
	st = slow.tick(t, now.Add(61*time.Minute))
	if _, ok := attention.Find(st, reviewBacklogKey); ok {
		t.Errorf("a rise over 61 minutes raised: %+v", st.Items)
	}
}

// Staying above the threshold does not wake again, falling to the threshold's
// half keeps it standing, falling under half clears it without a wake, and a
// new crossing after that raises once more.
func TestAttentionReviewBacklogOncePerCrossing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	setOpen := openRiskLandings(f, now)
	at := now

	step := func(open int) attention.State {
		t.Helper()
		setOpen(open)
		at = at.Add(2 * time.Hour) // far apart, so the one-hour rise never fires
		return f.tick(t, at)
	}

	step(16)
	step(20)
	step(16)
	if n := len(backlogWakes(t, f)); n != 1 {
		t.Fatalf("wake events above the threshold = %d, want 1", n)
	}

	// 8 is not under half of 15 (7.5): still standing, still one wake.
	if st := step(8); !hasItem(st, reviewBacklogKey) {
		t.Errorf("at 8 the item cleared; want it held until the count is under half the threshold")
	}
	if n := len(backlogWakes(t, f)); n != 1 {
		t.Errorf("wake events after falling to 8 = %d, want 1", n)
	}

	// 7 is under half: cleared, with a line that does not wake.
	if st := step(7); hasItem(st, reviewBacklogKey) {
		t.Errorf("at 7 the item is still there; want it cleared")
	}
	var cleared []attention.Event
	for _, e := range f.events(t) {
		if e.Rule == attention.RuleReviewBacklog && e.State == attention.EventCleared {
			cleared = append(cleared, e)
		}
	}
	if len(cleared) != 1 || cleared[0].Wake {
		t.Errorf("cleared review.backlog events = %+v, want one that does not wake", cleared)
	}

	// Between half and the threshold after clearing raises nothing; a new
	// crossing raises again.
	step(12)
	if n := len(backlogWakes(t, f)); n != 1 {
		t.Errorf("wake events at 12 after clearing = %d, want still 1", n)
	}
	step(16)
	if n := len(backlogWakes(t, f)); n != 2 {
		t.Errorf("wake events after the second crossing = %d, want 2", n)
	}
}

// The limits come from the operational config, and a failed risk-path read
// neither clears a standing backlog nor raises one.
func TestAttentionReviewBacklogLimitsAndReadFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	setOpen := openRiskLandings(f, now)
	f.src.backlog = backlogLimits{threshold: 4, rise: 100}

	setOpen(4)
	if st := f.tick(t, now); hasItem(st, reviewBacklogKey) {
		t.Errorf("4 items at a threshold of 4 raised; want it to need more than the threshold")
	}
	setOpen(5)
	if st := f.tick(t, now.Add(2*time.Hour)); !hasItem(st, reviewBacklogKey) {
		t.Errorf("5 items at a threshold of 4 did not raise")
	}

	good := f.src.riskLandings
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) { return nil, fmt.Errorf("landings unreadable") }
	if st := f.tick(t, now.Add(4*time.Hour)); !hasItem(st, reviewBacklogKey) {
		t.Errorf("a failed risk-path read cleared the standing backlog")
	}
	f.src.riskLandings = good
}

func hasItem(st attention.State, key string) bool {
	_, ok := attention.Find(st, key)
	return ok
}
