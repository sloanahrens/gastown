package daemon

import (
	"encoding/json"
	"fmt"
	"os"
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
	for _, e := range f.events(t) {
		if e.Rule == attention.RuleReviewBacklog && e.State != attention.EventNew {
			t.Errorf("event %+v, want a failed read to leave the raised line alone", e)
		}
	}
	f.src.riskLandings = good

	// The same failure on a queue with nothing standing raises nothing: an
	// absent count is not a count of zero.
	bare := newAttentionFixture(t, now)
	bare.src.backlog = backlogLimits{threshold: 4, rise: 100}
	bare.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) { return nil, fmt.Errorf("landings unreadable") }
	if st := bare.tick(t, now); hasItem(st, reviewBacklogKey) {
		t.Errorf("a failed risk-path read raised a backlog")
	}
	for _, e := range bare.events(t) {
		if e.Rule == attention.RuleReviewBacklog {
			t.Errorf("event %+v, want no review.backlog line from a failed read", e)
		}
	}
}

// writeRiskLandings writes n unreviewed risk-path landings into a rig's
// landings file, one bead and head each.
func writeRiskLandings(t *testing.T, lf *land.LandingsFile, n int, landed time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := lf.Append(land.LandingRecord{
			BeadID:    fmt.Sprintf("gt-b%02d", i),
			Head:      strings.Repeat(fmt.Sprintf("%x", i%16), 40),
			RiskPaths: []string{"internal/land/land.go"},
			LandedAt:  landed,
		}); err != nil {
			t.Fatalf("appending a landing record: %v", err)
		}
	}
}

// writeTornRiskLandings rewrites a landings file as one whole record and eight
// torn lines: the shape a read that cannot parse every line is handed.
func writeTornRiskLandings(t *testing.T, lf *land.LandingsFile, landed time.Time) {
	t.Helper()
	rec, err := json.Marshal(land.LandingRecord{
		BeadID:    "gt-b00",
		Head:      strings.Repeat("0", 40),
		RiskPaths: []string{"internal/land/land.go"},
		LandedAt:  landed,
	})
	if err != nil {
		t.Fatalf("encoding a landing record: %v", err)
	}
	var b strings.Builder
	b.Write(append(rec, '\n'))
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, "{\"bead\":\"gt-b%02d\",\"head\":\"%s\"\n", i, strings.Repeat("f", 40))
	}
	if err := os.WriteFile(lf.Path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("writing the landings file: %v", err)
	}
}

// riskItems is the risk-path items a state holds.
func riskItems(st attention.State) []attention.Item {
	var out []attention.Item
	for _, it := range st.Items {
		if it.Kind == attention.KindRiskPath {
			out = append(out, it)
		}
	}
	return out
}

// A landings file with a line the read cannot parse is a partial risk-path
// read, not a short one: the count under it would put 9 open at 1, under half
// the threshold, so a read that reported it would clear the standing backlog
// and drop the 8 items it did not read (gt-8mm0g).
func TestAttentionReviewBacklogPartialReadHoldsTheStandingItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.backlog = backlogLimits{threshold: 4, rise: 100}
	f.src.riskLandings = f.d.attentionRiskLandings

	lf, err := land.RigLandingsFile(f.d.config.TownRoot, attentionRig)
	if err != nil {
		t.Fatalf("RigLandingsFile: %v", err)
	}
	writeRiskLandings(t, lf, 9, now.Add(-30*time.Minute))
	if st := f.tick(t, now); !hasItem(st, reviewBacklogKey) {
		t.Fatalf("9 open risk-path items at a threshold of 4 did not raise")
	}

	writeTornRiskLandings(t, lf, now.Add(-30*time.Minute))
	st := f.tick(t, now.Add(time.Hour))
	if !hasItem(st, reviewBacklogKey) {
		t.Errorf("a partial risk-path read cleared the standing backlog")
	}
	if n := len(riskItems(st)); n != 9 {
		t.Errorf("risk-path items held = %d, want the 9 a short read would drop", n)
	}
	if !strings.Contains(f.logs.String(), "unparsable") {
		t.Errorf("logs = %q, want the partial read reported", f.logs.String())
	}
}

func hasItem(st attention.State, key string) bool {
	_, ok := attention.Find(st, key)
	return ok
}
