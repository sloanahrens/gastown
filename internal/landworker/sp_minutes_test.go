package landworker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// countingShow is a bead store that counts the Show calls it answers.
type countingShow struct {
	Beads
	shows *int
}

func (c countingShow) Show(id string) (*beads.Issue, error) {
	*c.shows++
	return c.Beads.Show(id)
}

// TestWorkerSPMinutesOneShow: the three sp label values a landing carries —
// its sp: estimate, its sp-minutes:<n> and its sp-slung:<reason> — come from
// one Show of the bead, so a landing with those labels makes the same Show
// calls as one without: two, one for the priority and one for the sp labels.
// A read per value would make the labelled landing cost three or four
// (gt-fqaj7z).
func TestWorkerSPMinutesOneShow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
	}{
		{"no sp labels", nil},
		{"sp labels", []string{"sp:3", "sp-minutes:10.8", "sp-slung:rework"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedLabelled(t, "gt-abc", tc.labels...)
			shows := 0
			h.w.Beads = countingShow{Beads: h.bd, shows: &shows}
			if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
				t.Fatalf("report %v; want one landing", rep)
			}
			if shows != 2 {
				t.Fatalf("one landing made %d Show calls; want 2: one for the priority and one for the three sp labels", shows)
			}
		})
	}
}

// TestWorkerSPMinutesRead: the landing carries the minutes its sp: estimate was
// computed as and the reason the dispatcher recorded for slinging the bead, so
// the landings record names both (gt-5eeqin).
func TestWorkerSPMinutesRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLabelled(t, "gt-abc", "sp-minutes:10.8", "sp-slung:rework")
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	got := h.lander.calls[0]
	if got.SPMinutes == nil || *got.SPMinutes != 10.8 {
		t.Fatalf("landing sp minutes = %v, want 10.8: the bead holds sp-minutes:10.8", got.SPMinutes)
	}
	if got.SPSlung != "rework" {
		t.Fatalf("landing sp slung = %q, want rework: the bead holds sp-slung:rework", got.SPSlung)
	}
}

// TestWorkerSPMinutesAbsent: a bead with no such labels, a malformed
// sp-minutes label, two that disagree, a sp-slung label with no reason, a read
// that answers no issue and a read that fails all land with the fields empty,
// as for the sp: estimate. A reasonless sp-slung label beside a real one is
// skipped, not read as a conflict. A failed read logs one line for each label
// it could not read, and the landing stands (gt-5eeqin).
func TestWorkerSPMinutesAbsent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		labels []string
		store  func(h *harness) Beads
		logs   []string
		// slung is the reason the case reads, if any. Every case wants the
		// minutes empty; the last drives the skip of a reasonless label.
		slung string
	}{
		{name: "no labels"},
		{name: "malformed sp-minutes", labels: []string{"sp-minutes:ten"}},
		{name: "conflicting sp-minutes", labels: []string{"sp-minutes:10.8", "sp-minutes:12"}},
		{name: "sp-slung with no reason", labels: []string{"sp-slung:"}},
		{name: "conflicting sp-slung reasons", labels: []string{"sp-slung:rework", "sp-slung:shape"}},
		{name: "reasonless sp-slung beside a real one", labels: []string{"sp-slung:", "sp-slung:rework"}, slung: "rework"},
		{name: "read answers no issue", labels: []string{"sp-minutes:10.8", "sp-slung:rework"}, store: func(h *harness) Beads {
			return nilShow{spStore{Fake: h.bd}}
		}},
		{name: "read fails", labels: []string{"sp-minutes:10.8", "sp-slung:rework"}, store: func(h *harness) Beads {
			return spStore{Fake: h.bd, showErr: errors.New("bd show: timeout")}
		}, logs: []string{"sp-minutes", "sp-slung reason"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedLabelled(t, "gt-abc", tc.labels...)
			logs := h.captureLogs()
			if tc.store != nil {
				// Injected before the pass, so the test does not depend on where
				// in the pass a bead read happens (gt-9rfjgs).
				h.w.Beads = tc.store(h)
			}
			rep := h.w.Pass(context.Background())
			if rep.Landed != 1 || rep.Failed != 0 {
				t.Fatalf("report %v; want the landing counted and nothing failed", rep)
			}
			got := h.lander.calls[0]
			if got.SPMinutes != nil {
				t.Fatalf("landing sp minutes = %v, want none", *got.SPMinutes)
			}
			if got.SPSlung != tc.slung {
				t.Fatalf("landing sp slung = %q, want %q", got.SPSlung, tc.slung)
			}
			var named []string
			for _, l := range *logs {
				if strings.Contains(l, "sp-minutes") || strings.Contains(l, "sp-slung reason") {
					named = append(named, l)
				}
			}
			if len(tc.logs) == 0 {
				if len(named) != 0 {
					t.Errorf("a bead with no readable labels logged %q; want no line", named)
				}
				return
			}
			for _, want := range tc.logs {
				var matching []string
				for _, l := range named {
					if strings.Contains(l, want) {
						matching = append(matching, l)
					}
				}
				if len(matching) != 1 {
					t.Errorf("log lines naming %q = %q; want exactly one of %q", want, matching, named)
				}
			}
		})
	}
}

// TestWorkerSPMinutesRepair: a repair rewrites the landing record from the work
// it is given, so the minutes and the reason the record holds ride the repair.
// The bead carries no such labels now, so only the record can supply them
// (gt-5eeqin).
func TestWorkerSPMinutesRepair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	minutes := 10.8
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: "a932cb5a",
		Target: "main", LandedCommit: "6db29a35", SPMinutes: &minutes, SPSlung: "rework"}}
	h.remote.contains["6db29a35"] = true
	h.w.startupDone = true // exercise the per-bead check, not the startup sweep
	if rep := h.w.Pass(context.Background()); rep.Repaired != 1 {
		t.Fatalf("report %v; want one repair", rep)
	}
	got := h.lander.calls[0]
	if got.SPMinutes == nil || *got.SPMinutes != minutes {
		t.Fatalf("repair sp minutes = %v, want 10.8 from the record", got.SPMinutes)
	}
	if got.SPSlung != "rework" {
		t.Fatalf("repair sp slung = %q, want rework from the record", got.SPSlung)
	}
}
