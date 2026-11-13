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

// TestPassLandsWithNoPriorityWhenThePriorityReadFails: the priority read is the
// only bead read that sees the failing store, and the landing still happens with
// the priority left nil. A read that fails logs one line; a read that answers
// with no issue leaves the priority nil without a line, since nothing failed
// (gt-tnml1k).
func TestPassLandsWithNoPriorityWhenThePriorityReadFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		store func(h *harness) Beads
		logs  string
	}{
		{"read fails", func(h *harness) Beads {
			return spStore{Fake: h.bd, showErr: errors.New("bd show: timeout")}
		}, "reading the bead's priority"},
		{"read answers no issue", func(h *harness) Beads {
			return nilShow{spStore{Fake: h.bd}}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedReady(t, "gt-abc")
			logs := h.captureLogs()
			// The agent-bead hook runs just before the priority read, and nothing
			// reads the store between it and the lander, so the swapped store is
			// seen by that read alone. The lander puts the working store back.
			h.w.AgentBeadID = func(string) string {
				h.w.Beads = tc.store(h)
				return ""
			}
			h.lander.fn = func(_ int, _ land.Work) (land.Result, error) {
				h.w.Beads = h.bd
				return land.Result{LandedCommit: spLanded, PatchID: "pppppppppp"}, nil
			}
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
		})
	}
}
