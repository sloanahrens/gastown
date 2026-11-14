package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// TestPassRecordsTheBeadsPriority: the landing request carries the priority
// the bead holds when it lands, so the landings record names it (gt-tnml1k).
func TestPassRecordsTheBeadsPriority(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-abc", 1, "2026-09-30T11:00:00Z")
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	got := h.lander.calls[0].Priority
	if got == nil || *got != 1 {
		t.Fatalf("landing priority = %v, want 1: the bead holds priority 1", got)
	}
}

// nilShow is a bead store whose Show answers with no issue and no error.
type nilShow struct {
	spStore
}

func (nilShow) Show(string) (*beads.Issue, error) {
	return nil, nil
}

// failingReads is a bead store whose every read fails: Show, and the batched
// ShowMultiple the startup repair reads landing records through. spStore alone
// would promote the fake's ShowMultiple, so that read would still answer.
type failingReads struct {
	spStore
}

func (s failingReads) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	return nil, s.showErr
}

// TestPassLandsWithNoBeadPriorityWhenTheReadFails: the store is swapped before
// the pass, so the test does not depend on where in the pass a bead read
// happens, and the landing still happens with the priority left nil. A
// priority read that fails logs one line; one that answers with no issue leaves
// the priority nil without a line, since nothing failed (gt-tnml1k, gt-y7g62j).
// The failing store also fails the batched read the startup repair makes over
// the landing records, which the seeded record gives the pass.
func TestPassLandsWithNoBeadPriorityWhenTheReadFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		store func(h *harness) Beads
		logs  string
		batch string
	}{
		{"read fails", func(h *harness) Beads {
			return failingReads{spStore{Fake: h.bd, showErr: errors.New("bd show: timeout")}}
		}, "reading the bead's priority", "reading the beads of 1 landing records"},
		{"read answers no issue", func(h *harness) Beads {
			return nilShow{spStore{Fake: h.bd}}
		}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedReady(t, "gt-abc")
			h.bd.Seed(beads.Issue{ID: "gt-old", Title: "landed work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal"})
			h.files.recs = []land.LandingRecord{{BeadID: "gt-old", Target: "main", LandedCommit: "cccccccccc"}}
			logs := h.captureLogs()
			h.w.Beads = tc.store(h)
			rep := h.w.Pass(context.Background())
			if rep.Landed != 1 || rep.Failed != 0 {
				t.Fatalf("report %v; want the landing counted and nothing failed", rep)
			}
			if got := h.lander.calls[0].Priority; got != nil {
				t.Fatalf("landing priority = %d, want none when the bead read gives none", *got)
			}
			var named []string
			for _, l := range *logs {
				if strings.Contains(l, "priority") {
					named = append(named, l)
				}
			}
			if tc.logs == "" && len(named) != 0 {
				t.Errorf("a read that answers no issue logged %q; want no line", named)
			}
			if tc.logs != "" && (len(named) != 1 || !strings.Contains(named[0], tc.logs)) {
				t.Errorf("priority log lines %q; want one naming %q", named, tc.logs)
			}
			var batch []string
			for _, l := range *logs {
				if strings.Contains(l, "landing records in one call") {
					batch = append(batch, l)
				}
			}
			if tc.batch == "" && len(batch) != 0 {
				t.Errorf("the startup repair's batched read logged %q; want no line", batch)
			}
			if tc.batch != "" && (len(batch) != 1 || !strings.Contains(batch[0], tc.batch)) {
				t.Errorf("batched read log lines %q; want one naming %q", batch, tc.batch)
			}
		})
	}
}
