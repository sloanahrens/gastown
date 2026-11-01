package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// collectorWorld is the batch audit world with 30 distinct beads over three
// rigs and 40 landing records (ten naming a bead already named in the rig).
func collectorWorld(t *testing.T, f *attentionFixture) *batchAuditWorld {
	t.Helper()
	w := newBatchAuditWorld(f, "alpha", "beta", "gamma")
	head := strings.Repeat("a", 40)
	for _, rig := range w.rigs {
		for i := 0; i < 10; i++ {
			w.landIn(rig, fmt.Sprintf("gt-%s%d", rig[:1], i), head, "ok")
		}
	}
	for i := 0; i < 10; i++ {
		w.landIn("alpha", fmt.Sprintf("gt-a%d", i), head, "ok")
	}
	return w
}

func itemKeys(items []attention.Item) string {
	keys := make([]string, 0, len(items))
	for _, it := range items {
		keys = append(keys, it.Key+"|"+it.Summary)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// 40 qualifying landing records over 3 rigs naming 30 distinct beads are read
// in one batched call per rig, with no single read, and the items equal the
// per-bead collector's (gt-12mix).
func TestRiskCollectorReadsOneBatchPerRig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := collectorWorld(t, f)

	items := f.collect(t, f.src.collectRiskPaths)

	if len(w.batches) != 3 || w.singles != 0 {
		t.Errorf("%s: batches %v singles %d, want 3 batched reads and none single", t.Name(), w.batches, w.singles)
	}
	if want := []string{"alpha:10", "beta:10", "gamma:10"}; strings.Join(w.batches, ",") != strings.Join(want, ",") {
		t.Errorf("%s: batches = %v, want %v (repeats folded)", t.Name(), w.batches, want)
	}
	// The same fixture through the per-bead path (no batch read) gives the same items.
	g := newAttentionFixture(t, now)
	w2 := collectorWorld(t, g)
	g.src.riskIssues = nil
	ref := g.collect(t, g.src.collectRiskPaths)
	if w2.singles != 30 {
		t.Fatalf("%s: the reference made %d single reads, want 30", t.Name(), w2.singles)
	}
	if itemKeys(items) != itemKeys(ref) {
		t.Errorf("%s: items differ from the per-bead collector:\n%s\n--- want\n%s", t.Name(), itemKeys(items), itemKeys(ref))
	}
}

// The audit and the collector in one tick make one batched read per rig
// between them; a bead the audit wrote an AUDITED line to is read again by the
// collector and its item is gone.
func TestRiskCollectorSharesTheAuditsRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := collectorWorld(t, f)

	if _, err := f.src.auditResolvedRiskFails(context.Background()); err != nil {
		t.Fatalf("%s: audit: %v", t.Name(), err)
	}
	items := f.collect(t, f.src.collectRiskPaths)
	if len(w.batches) != 3 {
		t.Errorf("%s: %d batched reads %v for the audit and the collector together, want 3", t.Name(), len(w.batches), w.batches)
	}
	if len(items) == 0 {
		t.Fatalf("%s: the collector raised nothing", t.Name())
	}

	// Now one bead carries a resolved FAIL: the audit writes to it, and the
	// collector reads it fresh.
	g := newAttentionFixture(t, now)
	w2 := collectorWorld(t, g)
	head := strings.Repeat("a", 40)
	w2.notes["gt-b3"] = failNote(head, "gt-fa")
	w2.status["gt-fa"] = "closed"
	wrote, err := g.src.auditResolvedRiskFails(context.Background())
	if err != nil || len(wrote) != 1 {
		t.Fatalf("%s: audit wrote %v err %v, want one AUDITED line", t.Name(), wrote, err)
	}
	items2 := g.collect(t, g.src.collectRiskPaths)
	for _, it := range items2 {
		if it.Bead == "gt-b3" {
			t.Errorf("%s: gt-b3 still has an item after the audit cleared it: %+v", t.Name(), it)
		}
	}
	if w2.singles != 1 {
		t.Errorf("%s: %d single reads, want 1 (the audit's re-read before its write)", t.Name(), w2.singles)
	}
	if len(w2.batches) != 4 || w2.batches[3] != "beta:1" {
		t.Errorf("%s: batches = %v, want three for the audit's rigs and one beta:1 for the bead the audit wrote", t.Name(), w2.batches)
	}
}

// A batch that fails for one rig keeps that rig's previous items, logs the
// skip once, leaves the count withheld, and the other rigs' items are raised.
func TestRiskCollectorKeepsFailedReadSemantics(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	w := newBatchAuditWorld(f, "alpha", "beta")
	head := strings.Repeat("c", 40)
	w.landIn("alpha", "gt-a1", head, "ok")
	w.landIn("beta", "gt-b1", head, "ok")
	inner := f.src.riskIssues
	f.src.riskIssues = func(c context.Context, rig string, ids []string) (map[string]*beads.Issue, error) {
		if rig == "alpha" {
			return nil, errors.New("bd show: boom")
		}
		return inner(c, rig, ids)
	}
	prev := attention.Item{Key: "risk:gt-a1:" + sha12(head), Kind: attention.KindRiskPath, Severity: attention.SeverityLow,
		Rig: "alpha", Bead: "gt-a1", SHA: head, Summary: "gt-a1 landed earlier"}
	f.src.prev = attention.State{Updated: now, Items: []attention.Item{prev}}

	items := f.collect(t, f.src.collectRiskPaths)

	keys := map[string]bool{}
	for _, it := range items {
		keys[it.Key] = true
	}
	if !keys[prev.Key] || !keys["risk:gt-b1:"+sha12(head)] || len(items) != 2 {
		t.Errorf("%s: items = %+v, want alpha's previous item kept and beta's raised", t.Name(), items)
	}
	if f.src.riskCounted {
		t.Errorf("%s: riskCounted = true after a failed read, want the count withheld", t.Name())
	}
	if !f.src.riskSkipped["alpha\x00gt-a1"] {
		t.Errorf("%s: riskSkipped = %v, want alpha's bead recorded", t.Name(), f.src.riskSkipped)
	}
	// The skip is said once: the first tick logs it, and a second tick that
	// finds the bead already in riskSkipped stays quiet.
	const skipLine = "attention: collector risk-path: skipping gt-a1 this tick"
	if n := strings.Count(f.logs.String(), skipLine); n != 1 {
		t.Errorf("%s: skip logged %d times on the first tick, want once; log %q", t.Name(), n, f.logs.String())
	}
	f.src.prev = attention.State{Updated: now, Items: items}
	f.collect(t, f.src.collectRiskPaths)
	if n := strings.Count(f.logs.String(), skipLine); n != 1 {
		t.Errorf("%s: skip logged %d times after a second failing tick, want still once", t.Name(), n)
	}
}

// A source without a batch read reads each id on its own, and a context that
// ends mid-way stops the loop: the ids after it are not asked for and the
// error is the context's (gt-5req0).
func TestRiskIssuesBatchStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	head := strings.Repeat("d", 40)
	recs := []land.LandingRecord{
		{BeadID: "gt-c1", Head: head, RiskPaths: []string{"x"}},
		{BeadID: "gt-c2", Head: head, RiskPaths: []string{"x"}},
		{BeadID: "gt-c3", Head: head, RiskPaths: []string{"x"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var asked []string
	f.src.riskIssues = nil
	f.src.riskIssue = func(_ context.Context, _, id string) (*beads.Issue, error) {
		asked = append(asked, id)
		cancel()
		return &beads.Issue{ID: id}, nil
	}

	got, err := f.src.riskIssuesBatch(ctx, "alpha", recs)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("%s: err = %v, want context canceled", t.Name(), err)
	}
	if len(asked) != 1 || asked[0] != "gt-c1" {
		t.Errorf("%s: asked %v, want only gt-c1 before the context ended", t.Name(), asked)
	}
	if len(got) != 1 || got["gt-c1"] == nil {
		t.Errorf("%s: got %v, want the one read that finished", t.Name(), got)
	}
}
