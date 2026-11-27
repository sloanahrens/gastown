package rehearsal

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// collectStart and collectEnd bound the window Collect reads the log over.
var collectStart, collectEnd = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)

// collectLog holds one failed call and one tier sweep run of rig load inside that
// window; collectListing holds one stranded seat in rig load and one in another
// rig. collectLoad holds two load samples in the window, so collectSummary is
// their nearest-rank p90 and max.
const (
	collectFailed  = "2026/11/27 10:15:05 bd: call failed: i/o timeout"
	collectSweep   = "2026/11/27 10:40:00 tier_sweep: load: swept 1a2b3c4d (shell GREEN) in 5m0s"
	collectLog     = "2026/11/27 10:30:00 daemon: heartbeat ok\n" + collectFailed + "\n" + collectSweep + "\n"
	collectListing = `[{"rig":"load","name":"alpha","state":"working","issue":"gt-1","session_running":true},{"rig":"load","name":"beta","state":"idle"},{"rig":"other","name":"eps","state":"stalled","issue":"gt-2"}]`
	collectLoad    = "2026-11-27T10:10:00Z load1=2.00 load5=0.00 load15=0.00 ncpu=24\n2026-11-27T10:20:00Z load1=6.50 load5=0.00 load15=0.00 ncpu=24\n"
)

var collectSummary = Load{Samples: 2, Max1: 6.5, P90One: 6.5}

// collectAnswers is a fake town's answer for each argv: gt-a carries a passing
// Gate line in its note and a failed gate run in a comment, and gt-b has landed.
func collectAnswers(t *testing.T) map[string]fakeAnswer {
	t.Helper()
	landed := land.LandingRecord{LandedCommit: "c0ffee", Target: "main", Branch: "b", Head: "h"}.NoteBlock()
	noteA := readyNote + "pass presubmit 3m5s, slot wait 0s"
	return map[string]fakeAnswer{
		"bd show gt-a --json":         {out: `{"data":[{"status":"closed","notes":` + strconv.Quote(noteA) + `}]}`},
		"bd comments gt-a --json":     {out: `{"data":[{"text":"GATE RUN: fail 2m0s, slot wait 0s, head abc1234"}]}`},
		"bd show gt-b --json":         {out: `{"data":[{"status":"closed","notes":` + strconv.Quote(landed) + `}]}`},
		"bd comments gt-b --json":     {out: `{"data":[]}`},
		"gt daemon logs -n 400000":    {out: collectLog},
		"gt polecat list load --json": {out: collectListing},
	}
}

func TestRehearsalCollect(t *testing.T) {
	t.Parallel()
	answers := collectAnswers(t)
	gates := []Gate{{Bead: "gt-a", Verdict: GatePass, Seconds: 185}, {Bead: "gt-a", Verdict: GateFail, Seconds: 120}}
	landings := []Landing{{Bead: "gt-a"}, {Bead: "gt-b", Landed: true}}
	argv := []string{"bd show gt-a --json", "bd comments gt-a --json", "bd show gt-b --json", "bd comments gt-b --json", "gt daemon logs -n 400000", "gt polecat list load --json"}
	for _, tc := range []struct {
		name string
		runs int
	}{
		{"a fake town fills every field", 1},
		{"a second Collect replaces the fields, not appends", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			rec := Record{Start: collectStart, End: collectEnd}
			for i := 0; i < tc.runs; i++ {
				if err := Collect(context.Background(), Town{Rig: "load", Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"}, strings.NewReader(collectLoad)); err != nil {
					t.Fatalf("Collect: %v", err)
				}
			}
			if !slices.Equal(rec.Gates, gates) || !slices.Equal(rec.Landings, landings) {
				t.Errorf("Gates, Landings = %+v, %+v; want %+v, %+v", rec.Gates, rec.Landings, gates, landings)
			}
			if !slices.Equal(rec.FailedCalls, []string{collectFailed}) || !slices.Equal(rec.Stranded, []string{"alpha"}) {
				t.Errorf("FailedCalls, Stranded = %q, %q; want [%s] and [alpha]", rec.FailedCalls, rec.Stranded, collectFailed)
			}
			if rec.Load != collectSummary || !rec.SweepRan {
				t.Errorf("Load, SweepRan = %+v, %v; want %+v and true", rec.Load, rec.SweepRan, collectSummary)
			}
			want := []string{}
			for i := 0; i < tc.runs; i++ {
				want = append(want, argv...)
			}
			if !slices.Equal(ran, want) {
				t.Errorf("town ran %q, want %q", ran, want)
			}
		})
	}
}

// TestRehearsalCollectErrors stops Collect at one failed read, decode or refused
// line. steps is how many steps had finished, in order: the beads, the daemon
// log, the polecat listing and the host load. A finished step's fields are filled;
// the rest keep the seed.
func TestRehearsalCollectErrors(t *testing.T) {
	t.Parallel()
	beads := []string{"bd show gt-a --json", "bd comments gt-a --json", "bd show gt-b --json", "bd comments gt-b --json"}
	logs, listing := "gt daemon logs -n 400000", "gt polecat list load --json"
	seed := Record{Start: collectStart, End: collectEnd, Gates: []Gate{{Bead: "seed", Verdict: GateFail}}, Landings: []Landing{{Bead: "seed"}}, FailedCalls: []string{"seed failed"}, Stranded: []string{"seed seat"}}
	gates := []Gate{{Bead: "gt-a", Verdict: GatePass, Seconds: 185}, {Bead: "gt-a", Verdict: GateFail, Seconds: 120}}
	landings := []Landing{{Bead: "gt-a"}, {Bead: "gt-b", Landed: true}}
	cause := errors.New("exit status 1")
	const leaked = "bd: secret output"
	// badSweep keeps the failed call and adds a sweep line in the window whose wall is not a duration.
	badSweep := collectFailed + "\n2026/11/27 10:20:00 tier_sweep: load: swept 1a2b3c4d (shell GREEN) in soon\n"
	full := append(slices.Clone(beads), logs, listing)
	for _, tc := range []struct {
		name, command, wants string
		answer               fakeAnswer
		load                 io.Reader
		wraps                bool
		ran                  []string
		steps                int
	}{
		{"a failed show read", "bd show gt-b --json", "gt-b", fakeAnswer{out: leaked, err: cause}, nil, true, beads[:3], 0},
		{"a failed comments read", "bd comments gt-a --json", "gt-a", fakeAnswer{out: leaked, err: cause}, nil, true, beads[:2], 0},
		{"a failed daemon log read", logs, "failed calls", fakeAnswer{out: leaked, err: cause}, nil, true, append(slices.Clone(beads), logs), 1},
		{"an uncovered log window", logs, "failed calls", fakeAnswer{out: "2026/11/27 09:00:00 daemon: asleep\n"}, nil, false, append(slices.Clone(beads), logs), 1},
		{"a failed listing read", listing, "stranded seats", fakeAnswer{out: leaked, err: cause}, nil, true, append(slices.Clone(beads), logs, listing), 2},
		{"a listing that does not decode", listing, "stranded seats", fakeAnswer{out: "[{"}, nil, false, append(slices.Clone(beads), logs, listing), 2},
		{"a bad load text", "", "host load", fakeAnswer{}, strings.NewReader("2026-11-27T10:10:00Z load1=oops load5=0.00 load15=0.00 ncpu=24\n"), false, full, 3},
		{"a failing host load reader", "", "host load", fakeAnswer{}, iotest.ErrReader(cause), true, full, 3},
		{"an unreadable sweep line", logs, "soon", fakeAnswer{out: badSweep}, nil, false, full, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answers := collectAnswers(t)
			if tc.command != "" {
				answers[tc.command] = tc.answer
			}
			load := tc.load
			if load == nil {
				load = strings.NewReader(collectLoad)
			}
			var ran []string
			rec := seed
			err := Collect(context.Background(), Town{Rig: "load", Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"}, load)
			if err == nil {
				t.Fatalf("Collect = nil, want an error from %s", tc.name)
			}
			if errors.Is(err, cause) != tc.wraps || strings.Contains(err.Error(), leaked) || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("Collect error = %q, wraps the cause = %v (want %v), want it to name %q and carry no output", err, errors.Is(err, cause), tc.wraps, tc.wants)
			}
			wantGates, wantLandings, wantFailed := seed.Gates, seed.Landings, seed.FailedCalls
			wantStranded, wantLoad := seed.Stranded, Load{}
			if tc.steps > 0 {
				wantGates, wantLandings = gates, landings
				if tc.steps > 1 {
					wantFailed = []string{collectFailed}
				}
			}
			if tc.steps > 2 {
				wantStranded = []string{"alpha"}
			}
			if tc.steps > 3 {
				wantLoad = collectSummary
			}
			if !slices.Equal(rec.Gates, wantGates) || !slices.Equal(rec.Landings, wantLandings) || !slices.Equal(rec.FailedCalls, wantFailed) || !slices.Equal(rec.Stranded, wantStranded) || rec.Load != wantLoad || rec.SweepRan {
				t.Errorf("record = Gates %+v, Landings %+v, FailedCalls %q, Stranded %q, Load %+v, SweepRan %v; want %+v, %+v, %q, %q, %+v, false", rec.Gates, rec.Landings, rec.FailedCalls, rec.Stranded, rec.Load, rec.SweepRan, wantGates, wantLandings, wantFailed, wantStranded, wantLoad)
			}
			if !slices.Equal(ran, tc.ran) {
				t.Errorf("town ran %q, want %q", ran, tc.ran)
			}
		})
	}
}
