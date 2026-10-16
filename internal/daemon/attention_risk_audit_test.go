package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// riskAuditWorld is a town of landed work beads with notes and finding beads
// with statuses and close reasons, wired into a fixture's risk-audit sources.
// Appends go into the notes the next read sees, so a test can check the item
// left the queue.
type riskAuditWorld struct {
	landings []land.LandingRecord
	notes    map[string]string
	status   map[string]string // finding bead id -> status
	reason   map[string]string // finding bead id -> close reason
	broken   map[string]error  // finding bead id -> read error
	appends  []string          // "<bead>: <line>" in write order
	reads    map[string]int    // finding bead id -> reads
}

func newRiskAuditWorld(f *attentionFixture) *riskAuditWorld {
	w := &riskAuditWorld{
		notes:  map[string]string{},
		status: map[string]string{},
		reason: map[string]string{},
		broken: map[string]error{},
		reads:  map[string]int{},
	}
	f.src.riskLandings = func(string, time.Time) ([]land.LandingRecord, error) { return w.landings, nil }
	f.src.riskIssue = func(_ context.Context, _, id string) (*beads.Issue, error) {
		// P2: an ordinary landing, so its item is review debt whatever the pass
		// does to its notes.
		return &beads.Issue{ID: id, Notes: w.notes[id], Priority: 2}, nil
	}
	f.src.findingIssue = func(_ context.Context, id string) (*beads.Issue, error) {
		w.reads[id]++
		if err := w.broken[id]; err != nil {
			return nil, err
		}
		st, ok := w.status[id]
		if !ok {
			return nil, beads.ErrNotFound
		}
		return &beads.Issue{ID: id, Status: st, CloseReason: w.reason[id]}, nil
	}
	f.src.riskAppend = func(_ context.Context, _, id, note string) error {
		w.appends = append(w.appends, id+": "+note)
		w.notes[id] += "\n" + note
		return nil
	}
	return w
}

// land records a risk-path landing of bead id at head, with notes.
func (w *riskAuditWorld) land(id, head, notes string) {
	w.landings = append(w.landings, land.LandingRecord{BeadID: id, Head: head, RiskPaths: []string{"internal/daemon/attention.go"}})
	w.notes[id] = notes
}

func failNote(head, findings string) string {
	if findings == "" {
		return land.OverseerReviewMarker + " " + head + " FAIL"
	}
	return land.OverseerReviewMarker + " " + head + " FAIL findings=" + findings
}

// gt-tlzvh: a FAIL whose finding beads are all closed gets one AUDITED line
// naming them, and the risk-path item leaves the queue on the same pass.
func TestAttentionRiskAuditClearsAFailWhoseFindingsAreClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	head := strings.Repeat("a", 40)
	fail := failNote(head, "gt-fa,gt-fb")
	w.land("gt-abc", head, "earlier note\n"+fail)
	w.status["gt-fa"], w.status["gt-fb"] = "closed", "closed"

	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 1 {
		t.Fatalf("items = %+v, want the FAIL held before the pass", items)
	}
	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil {
		t.Fatalf("auditResolvedRiskFails: %v", err)
	}
	want := land.OverseerReviewMarker + " " + head + " AUDITED: findings resolved by gt-fa,gt-fb"
	if len(w.appends) != 1 || w.appends[0] != "gt-abc: "+want || len(wrote) != 1 {
		t.Fatalf("appends = %q, wrote = %q, want the one line %q on gt-abc", w.appends, wrote, want)
	}
	if !strings.HasPrefix(w.notes["gt-abc"], "earlier note\n"+fail+"\n") {
		t.Errorf("notes = %q, want the FAIL line and the notes before it kept", w.notes["gt-abc"])
	}
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 0 {
		t.Errorf("items = %+v, want the item gone once AUDITED is recorded", items)
	}

	// The next tick finds the head answered and writes nothing more.
	if wrote, err := f.src.auditResolvedRiskFails(context.Background()); err != nil || len(wrote) != 0 || len(w.appends) != 1 {
		t.Errorf("second pass: wrote = %q, err = %v, appends = %q, want no new line", wrote, err, w.appends)
	}
}

// A FAIL is left alone unless every finding is known closed: an open bead, a
// bead in any other state, one bd cannot read, one that does not exist, a
// findings= id no bead can have, and a FAIL with no findings= (a note from
// before the suffix) all hold the item.
func TestAttentionRiskAuditLeavesAnUnresolvedFailAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		findings string
		status   map[string]string
		broken   map[string]error
		wantErr  bool
	}{
		{"one open", "gt-fa,gt-fb", map[string]string{"gt-fa": "closed", "gt-fb": "open"}, nil, false},
		{"in progress", "gt-fa", map[string]string{"gt-fa": "in_progress"}, nil, false},
		{"blocked", "gt-fa", map[string]string{"gt-fa": "blocked"}, nil, false},
		{"unreadable", "gt-fa,gt-fb", map[string]string{"gt-fa": "closed"}, map[string]error{"gt-fb": errors.New("bd: timeout")}, true},
		{"absent", "gt-fa,gt-gone", map[string]string{"gt-fa": "closed"}, nil, false},
		// bd would read "--force" as a flag, so the pass must not ask it.
		{"an id no bead can have", "gt-fa,--force", map[string]string{"gt-fa": "closed"}, nil, true},
		{"no findings suffix", "", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			w := newRiskAuditWorld(f)
			head := strings.Repeat("a", 40)
			fail := failNote(head, tc.findings)
			w.land("gt-abc", head, fail)
			for id, st := range tc.status {
				w.status[id] = st
			}
			for id, err := range tc.broken {
				w.broken[id] = err
			}
			_, err := f.src.auditResolvedRiskFails(context.Background())
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want an error reported for this case: %v", err, tc.wantErr)
			}
			if len(w.appends) != 0 || w.notes["gt-abc"] != fail {
				t.Errorf("appends = %q, notes = %q, want nothing written", w.appends, w.notes["gt-abc"])
			}
			if items := f.collect(t, f.src.collectRiskPaths); len(items) != 1 {
				t.Errorf("items = %+v, want the FAIL still in the queue", items)
			}
			if w.reads["--force"] != 0 {
				t.Errorf("the unshaped id was read %d times, want it never given to bd", w.reads["--force"])
			}
		})
	}
}

// A finding closed as resolved clears the FAIL; one closed for a reason that
// abandons it — the "supersede:" and "cancel:" prefixes the town records
// (gt-6hmz) — leaves it standing, whatever case it is written in.
func TestAttentionRiskAuditHoldsAFailWhoseFindingWasAbandoned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		reason string
		want   bool // the AUDITED line is written
	}{
		{"no reason recorded", "", true},
		{"closed as fixed", "fixed by gt-xyz", true},
		{"nothing to change", "no-changes: the finding was already handled", true},
		{"superseded", "supersede: gt-fx carries it", false},
		{"cancelled", "cancel: not a defect after all", false},
		{"the same in capitals", "CANCEL: not a defect after all", false},
		{"padded supersede", "  Supersede: gt-fx carries it", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			w := newRiskAuditWorld(f)
			head := strings.Repeat("a", 40)
			w.land("gt-abc", head, failNote(head, "gt-fa"))
			w.status["gt-fa"], w.reason["gt-fa"] = "closed", tc.reason

			wrote, err := f.src.auditResolvedRiskFails(context.Background())
			if err != nil {
				t.Fatalf("auditResolvedRiskFails: %v", err)
			}
			if (len(wrote) == 1) != tc.want {
				t.Fatalf("wrote = %q, appends = %q, want the line written: %v", wrote, w.appends, tc.want)
			}
			if cleared := len(f.collect(t, f.src.collectRiskPaths)) == 0; cleared != tc.want {
				t.Errorf("queue cleared = %v, want %v (the FAIL clears only with the AUDITED line)", cleared, tc.want)
			}
		})
	}
}

// The append is judged against the notes as they are at the last moment before
// it, not the ones the pass started from: bd has no compare-and-swap, and a
// FAIL that moved while the finding beads were read — here a re-review naming a
// new finding — takes the next tick instead of an AUDITED line.
func TestAttentionRiskAuditLeavesAFailThatMovedBeforeTheAppend(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	head := strings.Repeat("a", 40)
	w.land("gt-abc", head, failNote(head, "gt-fa"))
	w.status["gt-fa"], w.status["gt-fb"] = "closed", "open"
	read := f.src.riskIssue
	reads := 0
	f.src.riskIssue = func(ctx context.Context, rig, id string) (*beads.Issue, error) {
		is, err := read(ctx, rig, id)
		reads++
		if err != nil || is == nil || reads == 1 {
			return is, err
		}
		// The review moved between the pass's first read and its last.
		is.Notes += "\n" + failNote(head, "gt-fa,gt-fb")
		return is, nil
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil || len(wrote) != 0 || len(w.appends) != 0 {
		t.Fatalf("wrote = %q, err = %v, appends = %q, want nothing written", wrote, err, w.appends)
	}
	if items := f.collect(t, f.src.collectRiskPaths); len(items) != 1 {
		t.Errorf("items = %+v, want the FAIL still in the queue", items)
	}
}

// Only a FAIL is touched: a head with no review, a PASS, a WAIVED, and a FAIL
// since superseded all pass through untouched, and a landing with no risk path
// is not read at all.
func TestAttentionRiskAuditTouchesOnlyAFail(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	h := func(c string) string { return strings.Repeat(c, 40) }
	w.status["gt-fa"] = "closed"
	w.land("gt-none", h("1"), "")
	w.land("gt-pass", h("2"), land.OverseerReviewMarker+" "+h("2")+" PASS")
	w.land("gt-waived", h("3"), land.OverseerReviewMarker+" "+h("3")+" WAIVED: docs only")
	w.land("gt-redo", h("4"), failNote(h("4"), "gt-fa")+"\n"+land.OverseerReviewMarker+" "+h("4")+" PASS")
	// A FAIL of another head of the same bead does not answer for this one.
	w.land("gt-other", h("5"), failNote(h("6"), "gt-fa"))
	w.landings = append(w.landings, land.LandingRecord{BeadID: "gt-plain", Head: h("7")})
	w.notes["gt-plain"] = failNote(h("7"), "gt-fa")
	before := map[string]string{}
	for id, n := range w.notes {
		before[id] = n
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil || len(wrote) != 0 || len(w.appends) != 0 {
		t.Fatalf("wrote = %q, err = %v, appends = %q, want nothing written", wrote, err, w.appends)
	}
	for id, n := range before {
		if w.notes[id] != n {
			t.Errorf("%s notes = %q, want %q unchanged", id, w.notes[id], n)
		}
	}
	if w.reads["gt-fa"] != 0 {
		t.Errorf("finding gt-fa was read %d times, want 0: no FAIL for a landed head named it", w.reads["gt-fa"])
	}
}

// The pass writes at most riskAuditCap lines a tick and drains the rest on the
// next, each head once, never touching a line already in the notes; a finding
// bead named by several landings is read once a tick.
func TestAttentionRiskAuditWritesAtMostItsCapPerTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	w.status["gt-shared"] = "closed"
	total := riskAuditCap + 3
	fails := map[string]string{}
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("gt-w%02d", i)
		head := fmt.Sprintf("%040x", i+1)
		w.land(id, head, failNote(head, "gt-shared"))
		fails[id] = w.notes[id]
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil || len(wrote) != riskAuditCap || len(w.appends) != riskAuditCap {
		t.Fatalf("first tick: wrote %d, appends %d, err %v, want %d", len(wrote), len(w.appends), err, riskAuditCap)
	}
	if w.reads["gt-shared"] != 1 {
		t.Errorf("gt-shared read %d times in one tick, want 1", w.reads["gt-shared"])
	}
	wrote, err = f.src.auditResolvedRiskFails(context.Background())
	if err != nil || len(wrote) != 3 || len(w.appends) != total {
		t.Fatalf("second tick: wrote %d, appends %d, err %v, want the last 3 of %d", len(wrote), len(w.appends), err, total)
	}
	if wrote, _ := f.src.auditResolvedRiskFails(context.Background()); len(wrote) != 0 {
		t.Errorf("third tick wrote %q, want nothing left", wrote)
	}
	for id, fail := range fails {
		if !strings.HasPrefix(w.notes[id], fail+"\n"+land.OverseerReviewMarker) || strings.Count(w.notes[id], "AUDITED") != 1 {
			t.Errorf("%s notes = %q, want the FAIL line kept and one AUDITED after it", id, w.notes[id])
		}
	}
}

// A write that fails leaves the FAIL standing and is reported; the pass goes
// on to the next landing, and the next tick retries.
func TestAttentionRiskAuditReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newRiskAuditWorld(f)
	w.status["gt-fa"] = "closed"
	ha, hb := strings.Repeat("a", 40), strings.Repeat("b", 40)
	w.land("gt-a", ha, failNote(ha, "gt-fa"))
	w.land("gt-b", hb, failNote(hb, "gt-fa"))
	down := true
	record := f.src.riskAppend
	f.src.riskAppend = func(ctx context.Context, rig, id, note string) error {
		if down && id == "gt-a" {
			return errors.New("bd: connection refused")
		}
		return record(ctx, rig, id, note)
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gt-a") || len(wrote) != 1 || !strings.HasPrefix(wrote[0], "gt-b:") {
		t.Fatalf("wrote = %q, err = %v, want gt-b audited and the gt-a failure reported", wrote, err)
	}
	down = false
	if wrote, err := f.src.auditResolvedRiskFails(context.Background()); err != nil || len(wrote) != 1 || !strings.HasPrefix(wrote[0], "gt-a:") {
		t.Errorf("retry: wrote = %q, err = %v, want gt-a audited", wrote, err)
	}
}
