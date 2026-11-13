package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestPassRecordsTheBeadsSPEstimate: the landing request carries the sp:
// estimate the bead holds when it lands, so the landings record names it
// (gt-cdhagm).
func TestPassRecordsTheBeadsSPEstimate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp:3")
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	got := h.lander.calls[0].SPEstimate
	if got == nil || *got != 3 {
		t.Fatalf("landing sp estimate = %v, want 3: the bead holds sp:3", got)
	}
}

// TestPassLandsWithNoSPEstimate: a bead with no estimate, or with two that
// conflict, lands with the estimate left nil and no log line, since nothing
// failed. A read that fails logs one line and still lands (gt-cdhagm).
func TestPassLandsWithNoSPEstimate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		store  func(h *harness) Beads
		logs   string
	}{
		{"no estimate label", nil, nil, ""},
		{"conflicting estimates", []string{"sp:3", "sp:5"}, nil, ""},
		{"read answers no issue", []string{"sp:3"}, func(h *harness) Beads {
			return nilShow{spStore{Fake: h.bd}}
		}, ""},
		{"read fails", []string{"sp:3"}, func(h *harness) Beads {
			return spStore{Fake: h.bd, showErr: errors.New("bd show: timeout")}
		}, "reading the bead's sp: estimate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedLabelled(t, "gt-abc", tc.labels...)
			logs := h.captureLogs()
			if tc.store != nil {
				// The agent-bead hook runs just before the bead reads that land
				// the estimate, and the lander puts the working store back.
				h.w.AgentBeadID = func(string) string {
					h.w.Beads = tc.store(h)
					return ""
				}
				h.lander.fn = func(_ int, _ land.Work) (land.Result, error) {
					h.w.Beads = h.bd
					return land.Result{LandedCommit: spLanded, PatchID: "pppppppppp"}, nil
				}
			}
			rep := h.w.Pass(context.Background())
			if rep.Landed != 1 || rep.Failed != 0 {
				t.Fatalf("report %v; want the landing counted and nothing failed", rep)
			}
			if got := h.lander.calls[0].SPEstimate; got != nil {
				t.Fatalf("landing sp estimate = %d, want none", *got)
			}
			var named []string
			for _, l := range *logs {
				if strings.Contains(l, "sp: estimate") {
					named = append(named, l)
				}
			}
			if tc.logs == "" && len(named) != 0 {
				t.Errorf("a bead with no readable estimate logged %q; want no line", named)
			}
			if tc.logs != "" && (len(named) != 1 || !strings.Contains(named[0], tc.logs)) {
				t.Errorf("sp estimate log lines %q; want one naming %q", named, tc.logs)
			}
		})
	}
}

// TestPassRepairKeepsTheRecordedSPEstimate: a repair rewrites the landing record
// from the work it is given, so the estimate the record holds rides the repair.
// The bead carries no sp: label now, so only the record can supply it
// (gt-cdhagm).
func TestPassRepairKeepsTheRecordedSPEstimate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	three := 3
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: "a932cb5a", Target: "main", LandedCommit: "6db29a35", SPEstimate: &three}}
	h.remote.contains["6db29a35"] = true
	h.w.startupDone = true // exercise the per-bead check, not the startup sweep
	if rep := h.w.Pass(context.Background()); rep.Repaired != 1 {
		t.Fatalf("report %v; want one repair", rep)
	}
	got := h.lander.calls[0].SPEstimate
	if got == nil || *got != 3 {
		t.Fatalf("repair sp estimate = %v, want 3 from the record", got)
	}
}
