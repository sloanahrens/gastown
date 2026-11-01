package daemon

import (
	"context"
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
	if f.src.auditNext != 1 {
		t.Fatalf("%s: auditNext = %d after alpha finished and beta was cut, want 1", t.Name(), f.src.auditNext)
	}

	w.batches = nil
	f.src.auditStart = f.src.auditNext
	if _, err := f.src.auditResolvedRiskFails(context.Background()); err != nil {
		t.Fatalf("%s: second pass: %v", t.Name(), err)
	}
	if got := strings.Join(w.batches, ","); got != "beta:1,gamma:1,alpha:1" {
		t.Errorf("%s: second pass read %s, want it to begin at beta", t.Name(), got)
	}
}
