package rehearsal

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// collectStart and collectEnd bound the window Collect reads the log over.
var collectStart, collectEnd = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)

// collectLog holds one failed call inside that window; collectListing holds one
// stranded seat in rig load and one in another rig.
const (
	collectFailed  = "2026/11/27 10:15:05 bd: call failed: i/o timeout"
	collectLog     = "2026/11/27 10:30:00 daemon: heartbeat ok\n" + collectFailed + "\n"
	collectListing = `[{"rig":"load","name":"alpha","state":"working","issue":"gt-1","session_running":true},{"rig":"load","name":"beta","state":"idle"},{"rig":"other","name":"eps","state":"stalled","issue":"gt-2"}]`
)

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
				if err := Collect(context.Background(), Town{Rig: "load", Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"}); err != nil {
					t.Fatalf("Collect: %v", err)
				}
			}
			if !slices.Equal(rec.Gates, gates) || !slices.Equal(rec.Landings, landings) {
				t.Errorf("Gates, Landings = %+v, %+v; want %+v, %+v", rec.Gates, rec.Landings, gates, landings)
			}
			if !slices.Equal(rec.FailedCalls, []string{collectFailed}) || !slices.Equal(rec.Stranded, []string{"alpha"}) {
				t.Errorf("FailedCalls, Stranded = %q, %q; want [%s] and [alpha]", rec.FailedCalls, rec.Stranded, collectFailed)
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

// TestRehearsalCollectErrors stops Collect at one failed read or decode. steps is
// how many reads had finished: their fields stay filled, later ones keep the seed.
func TestRehearsalCollectErrors(t *testing.T) {
	t.Parallel()
	beads := []string{"bd show gt-a --json", "bd comments gt-a --json", "bd show gt-b --json", "bd comments gt-b --json"}
	logs, listing := "gt daemon logs -n 400000", "gt polecat list load --json"
	seed := Record{Start: collectStart, End: collectEnd, Gates: []Gate{{Bead: "seed", Verdict: GateFail}}, Landings: []Landing{{Bead: "seed"}}, FailedCalls: []string{"seed failed"}, Stranded: []string{"seed seat"}}
	gates := []Gate{{Bead: "gt-a", Verdict: GatePass, Seconds: 185}, {Bead: "gt-a", Verdict: GateFail, Seconds: 120}}
	landings := []Landing{{Bead: "gt-a"}, {Bead: "gt-b", Landed: true}}
	cause := errors.New("exit status 1")
	const leaked = "bd: secret output"
	for _, tc := range []struct {
		name, command, wants string
		answer               fakeAnswer
		wraps                bool
		ran                  []string
		steps                int
	}{
		{"a failed show read", "bd show gt-b --json", "gt-b", fakeAnswer{out: leaked, err: cause}, true, beads[:3], 0},
		{"a failed comments read", "bd comments gt-a --json", "gt-a", fakeAnswer{out: leaked, err: cause}, true, beads[:2], 0},
		{"a failed daemon log read", logs, "failed calls", fakeAnswer{out: leaked, err: cause}, true, append(slices.Clone(beads), logs), 1},
		{"an uncovered log window", logs, "failed calls", fakeAnswer{out: "2026/11/27 09:00:00 daemon: asleep\n"}, false, append(slices.Clone(beads), logs), 1},
		{"a failed listing read", listing, "stranded seats", fakeAnswer{out: leaked, err: cause}, true, append(slices.Clone(beads), logs, listing), 2},
		{"a listing that does not decode", listing, "stranded seats", fakeAnswer{out: "[{"}, false, append(slices.Clone(beads), logs, listing), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answers := collectAnswers(t)
			answers[tc.command] = tc.answer
			var ran []string
			rec := seed
			err := Collect(context.Background(), Town{Rig: "load", Run: fakeRun(answers, &ran)}, &rec, []string{"gt-a", "gt-b"})
			if err == nil {
				t.Fatalf("Collect = nil, want an error from %s", tc.command)
			}
			if errors.Is(err, cause) != tc.wraps || strings.Contains(err.Error(), leaked) || !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("Collect error = %q, wraps the cause = %v (want %v), want it to name %q and carry no output", err, errors.Is(err, cause), tc.wraps, tc.wants)
			}
			wantGates, wantLandings, wantFailed := seed.Gates, seed.Landings, seed.FailedCalls
			if tc.steps > 0 {
				wantGates, wantLandings = gates, landings
				if tc.steps > 1 {
					wantFailed = []string{collectFailed}
				}
			}
			if !slices.Equal(rec.Gates, wantGates) || !slices.Equal(rec.Landings, wantLandings) || !slices.Equal(rec.FailedCalls, wantFailed) || !slices.Equal(rec.Stranded, seed.Stranded) {
				t.Errorf("record = Gates %+v, Landings %+v, FailedCalls %q, Stranded %q; want %+v, %+v, %q, %q", rec.Gates, rec.Landings, rec.FailedCalls, rec.Stranded, wantGates, wantLandings, wantFailed, seed.Stranded)
			}
			if !slices.Equal(ran, tc.ran) {
				t.Errorf("town ran %q, want %q", ran, tc.ran)
			}
		})
	}
}
