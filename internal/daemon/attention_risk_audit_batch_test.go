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

// batchAuditWorld is a town of three rigs of landed work beads whose work
// beads are read through a counting batched source (gt-5req0).
type batchAuditWorld struct {
	*riskAuditWorld
	rigs    []string
	records map[string][]land.LandingRecord // rig -> its landing records
	batches []string                        // "rig:n" per batched read, in order
	singles int                             // riskIssue calls
}

func newBatchAuditWorld(f *attentionFixture, rigs ...string) *batchAuditWorld {
	w := &batchAuditWorld{riskAuditWorld: newRiskAuditWorld(f), rigs: rigs, records: map[string][]land.LandingRecord{}}
	f.src.landingRigs = func() []string { return rigs }
	f.src.riskLandings = func(rig string, _ time.Time) ([]land.LandingRecord, error) { return w.records[rig], nil }
	f.src.riskIssue = func(_ context.Context, _, id string) (*beads.Issue, error) {
		w.singles++
		return &beads.Issue{ID: id, Notes: w.notes[id], Priority: 2}, nil
	}
	f.src.riskIssues = func(_ context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
		w.batches = append(w.batches, fmt.Sprintf("%s:%d", rig, len(ids)))
		out := make(map[string]*beads.Issue, len(ids))
		for _, id := range ids {
			out[id] = &beads.Issue{ID: id, Notes: w.notes[id], Priority: 2}
		}
		return out, nil
	}
	return w
}

// landIn records a risk-path landing of bead id at head in rig.
func (w *batchAuditWorld) landIn(rig, id, head, notes string) {
	w.records[rig] = append(w.records[rig], land.LandingRecord{BeadID: id, Head: head, RiskPaths: []string{"internal/daemon/attention.go"}})
	w.notes[id] = notes
}

// 40 qualifying landing records over 3 rigs, six of them repeating a bead, are
// read in one batched call per rig, with the repeats folded into one id each;
// the one FAIL whose finding is closed is still written (gt-5req0).
func TestRiskAuditReadsOneBatchPerRig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta", "gamma")
	head := strings.Repeat("a", 40)
	n := 0
	for ri, rig := range w.rigs {
		for i := 0; i < []int{12, 11, 11}[ri]; i++ {
			n++
			id := fmt.Sprintf("gt-%s%d", rig[:1], i)
			w.landIn(rig, id, head, "ok")
		}
	}
	// Six records name a bead already named in the same rig.
	for i := 0; i < 6; i++ {
		w.landIn("alpha", fmt.Sprintf("gt-a%d", i), head, "ok")
		n++
	}
	w.notes["gt-b3"] = failNote(head, "gt-fa")
	w.status["gt-fa"] = "closed"
	if n != 40 {
		t.Fatalf("%s: built %d records, want 40", t.Name(), n)
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())
	if err != nil {
		t.Fatalf("%s: audit: %v", t.Name(), err)
	}

	if len(w.batches) != 3 {
		t.Errorf("%s: %d batched reads %v, want 3 (one per rig)", t.Name(), len(w.batches), w.batches)
	}
	if want := []string{"alpha:12", "beta:11", "gamma:11"}; strings.Join(w.batches, ",") != strings.Join(want, ",") {
		t.Errorf("%s: batches = %v, want %v (repeats folded)", t.Name(), w.batches, want)
	}
	// The one single read is the re-read of the FAIL bead before its write.
	if w.singles != 1 {
		t.Errorf("%s: %d single reads, want 1 (the re-read before the write)", t.Name(), w.singles)
	}
	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "gt-b3: ") {
		t.Errorf("%s: wrote %v, want one AUDITED line on gt-b3", t.Name(), wrote)
	}
}

// A second tick over the same records costs one batched read per rig again
// and writes nothing new.
func TestRiskAuditRepeatTickCostsOneBatchPerRig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta")
	head := strings.Repeat("b", 40)
	w.landIn("alpha", "gt-a1", head, failNote(head, "gt-fa"))
	w.landIn("beta", "gt-b1", head, "ok")
	w.status["gt-fa"] = "closed"

	for tick := 1; tick <= 2; tick++ {
		wrote, err := f.src.auditResolvedRiskFails(context.Background())
		if err != nil {
			t.Fatalf("%s: tick %d: %v", t.Name(), tick, err)
		}
		if tick == 1 && len(wrote) != 1 {
			t.Fatalf("%s: tick 1 wrote %v, want one line", t.Name(), wrote)
		}
		if tick == 2 && len(wrote) != 0 {
			t.Errorf("%s: tick 2 wrote %v, want nothing new", t.Name(), wrote)
		}
	}
	if len(w.batches) != 4 {
		t.Errorf("%s: %d batched reads %v over two ticks, want 4", t.Name(), len(w.batches), w.batches)
	}
}

// A pass cut short by its context begins the next one at the rig after the
// last it finished, so the rigs behind the first are not starved.
func TestRiskAuditRotatesAfterATimeout(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta", "gamma")
	head := strings.Repeat("c", 40)
	for _, rig := range w.rigs {
		w.landIn(rig, "gt-"+rig[:1]+"1", head, "ok")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := f.src.riskIssues
	f.src.riskIssues = func(c context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
		if rig == "beta" && c == ctx {
			cancel()
			return nil, c.Err()
		}
		return inner(c, rig, ids)
	}

	if _, err := f.src.auditResolvedRiskFails(ctx); err == nil {
		t.Fatalf("%s: the cut-short pass returned no error", t.Name())
	}
	if f.src.auditNext != 2 {
		t.Fatalf("%s: auditNext = %d after alpha finished and beta was cut, want 2 (the rig after the cut one)", t.Name(), f.src.auditNext)
	}

	w.batches = nil
	f.src.auditStart = f.src.auditNext
	if _, err := f.src.auditResolvedRiskFails(context.Background()); err != nil {
		t.Fatalf("%s: second pass: %v", t.Name(), err)
	}
	if got := strings.Join(w.batches, ","); got != "gamma:1,alpha:1,beta:1" {
		t.Errorf("%s: second pass read %s, want it to begin at gamma", t.Name(), got)
	}
}

// A context that ended before the pass stops it at the first rig with the
// context's error and reads nothing.
func TestRiskAuditStopsOnACanceledContext(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta")
	w.landIn("alpha", "gt-a1", strings.Repeat("d", 40), "ok")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.src.auditResolvedRiskFails(ctx); err == nil {
		t.Errorf("%s: a canceled context returned no error", t.Name())
	}
	if len(w.batches) != 0 || w.singles != 0 {
		t.Errorf("%s: a canceled pass read %v and %d singles, want nothing", t.Name(), w.batches, w.singles)
	}
}

// batchReader is a work-bead reader with the batched read.
type batchReader struct {
	workBeadReader
	batched [][]string
}

func (b *batchReader) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	b.batched = append(b.batched, ids)
	return map[string]*beads.Issue{ids[0]: {ID: ids[0]}}, nil
}

// singleReader is a work-bead reader without it.
type singleReader struct {
	workBeadReader
	shown []string
	fail  string
}

func (r *singleReader) Show(id string) (*beads.Issue, error) {
	r.shown = append(r.shown, id)
	if id == r.fail {
		return nil, fmt.Errorf("show %s: boom", id)
	}
	return &beads.Issue{ID: id}, nil
}

// The production read uses the batched call when the reader has one, falls
// back to one read per id when it does not, and fails on a canceled context or
// a failed read.
func TestAttentionRiskIssuesBatchesOrFallsBack(t *testing.T) {
	t.Parallel()
	d, _ := signalTestDaemon(t)

	t.Run("a reader with the batch read gets one call", func(t *testing.T) {
		t.Parallel()
		r := &batchReader{}
		d.openWorkBeads = func([]string, time.Duration) workBeadReader { return r }
		got, err := d.attentionRiskIssues(context.Background(), "alpha", []string{"gt-1", "gt-2"})
		if err != nil || len(r.batched) != 1 || got["gt-1"] == nil {
			t.Errorf("%s: got %v err %v batched %v, want one batched call", t.Name(), got, err, r.batched)
		}
	})
	t.Run("a reader without it is read one id at a time", func(t *testing.T) {
		t.Parallel()
		dd, _ := signalTestDaemon(t)
		r := &singleReader{}
		dd.openWorkBeads = func([]string, time.Duration) workBeadReader { return r }
		got, err := dd.attentionRiskIssues(context.Background(), "alpha", []string{"gt-1", "gt-2"})
		if err != nil || len(got) != 2 || strings.Join(r.shown, ",") != "gt-1,gt-2" {
			t.Errorf("%s: got %v err %v shown %v", t.Name(), got, err, r.shown)
		}
	})
	t.Run("a failed read fails the call", func(t *testing.T) {
		t.Parallel()
		dd, _ := signalTestDaemon(t)
		dd.openWorkBeads = func([]string, time.Duration) workBeadReader { return &singleReader{fail: "gt-2"} }
		if _, err := dd.attentionRiskIssues(context.Background(), "alpha", []string{"gt-1", "gt-2"}); err == nil {
			t.Errorf("%s: a failed Show returned no error", t.Name())
		}
	})
	t.Run("a canceled context fails the call", func(t *testing.T) {
		t.Parallel()
		dd, _ := signalTestDaemon(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := dd.attentionRiskIssues(ctx, "alpha", []string{"gt-1"}); err == nil {
			t.Errorf("%s: a canceled context returned no error", t.Name())
		}
	})
}

// A rig whose batched read always outlasts the share is skipped past by the
// next tick: every tick that is cut inside it starts one rig later, so the
// rigs behind it are reached (gt-5req0 round 1).
func TestRiskAuditDoesNotStayOnARigThatIsAlwaysCut(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta", "gamma")
	head := strings.Repeat("e", 40)
	for _, rig := range w.rigs {
		w.landIn(rig, "gt-"+rig[:1]+"1", head, "ok")
	}
	inner := f.src.riskIssues
	cutAlpha := func(c context.Context, cancel context.CancelFunc) {
		f.src.riskIssues = func(cc context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
			if rig == "alpha" && cc == c {
				cancel()
				return nil, cc.Err()
			}
			return inner(cc, rig, ids)
		}
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	cutAlpha(ctx1, cancel1)
	if _, err := f.src.auditResolvedRiskFails(ctx1); err == nil {
		t.Fatalf("%s: the cut pass returned no error", t.Name())
	}
	if f.src.auditNext != 1 {
		t.Fatalf("%s: auditNext = %d after a cut inside the first rig, want 1 (the rig after it)", t.Name(), f.src.auditNext)
	}

	w.batches = nil
	f.src.auditStart = f.src.auditNext
	if _, err := f.src.auditResolvedRiskFails(context.Background()); err != nil {
		t.Fatalf("%s: second pass: %v", t.Name(), err)
	}
	if got := strings.Join(w.batches, ","); got != "beta:1,gamma:1,alpha:1" {
		t.Errorf("%s: second pass read %s, want beta and gamma reached first", t.Name(), got)
	}
}

// A batch that fails for a reason other than the context skips that rig, keeps
// the error for the caller, and the next rig is still audited; a pass cut short
// later returns the earlier error with the cut.
func TestRiskAuditSkipsARigWhoseBatchFailsAndGoesOn(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta")
	head := strings.Repeat("f", 40)
	w.landIn("alpha", "gt-a1", head, failNote(head, "gt-fa"))
	w.landIn("beta", "gt-b1", head, failNote(head, "gt-fb"))
	w.status["gt-fa"], w.status["gt-fb"] = "closed", "closed"
	inner := f.src.riskIssues
	f.src.riskIssues = func(c context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
		if rig == "alpha" {
			return nil, errors.New("bd show: boom")
		}
		return inner(c, rig, ids)
	}

	wrote, err := f.src.auditResolvedRiskFails(context.Background())

	if err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("%s: error = %v, want one naming alpha and the failure", t.Name(), err)
	}
	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "gt-b1: ") {
		t.Errorf("%s: wrote %v, want only beta's FAIL audited", t.Name(), wrote)
	}
	if f.src.auditNext != 0 {
		t.Errorf("%s: auditNext = %d after a full pass, want it back at the start", t.Name(), f.src.auditNext)
	}
}

// A cut-short pass keeps an earlier rig's error: the caller logs one error, and
// it must not be only the cut.
func TestRiskAuditCutKeepsAnEarlierRigsError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta")
	head := strings.Repeat("a", 40)
	w.landIn("alpha", "gt-a1", head, "ok")
	w.landIn("beta", "gt-b1", head, "ok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := f.src.riskIssues
	f.src.riskIssues = func(c context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
		if rig == "alpha" {
			return nil, errors.New("bd show: boom")
		}
		cancel()
		return inner(c, rig, ids)
	}

	_, err := f.src.auditResolvedRiskFails(ctx)

	if err == nil || !strings.Contains(err.Error(), "boom") || !errors.Is(err, context.Canceled) {
		t.Errorf("%s: error = %v, want the earlier rig's failure joined with the cut", t.Name(), err)
	}
}
