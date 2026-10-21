package cmd

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The tick slings its candidates concurrently, bounded by the free seats
// (design: docs/plans/2026-10-21-dispatcher-throughput-design.md section 2).
// Before this a tick slung at most max_per_tick candidates one after another,
// 45-70 s each, which is what kept 2-4 of 9 seats live.

// nineSeatTown is three seats of three with twelve clean candidates.
func nineSeatTown(t *testing.T, n int) *fakeSpecTown {
	t.Helper()
	specs := make([]specdispatch.Spec, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, cleanSpec(fmt.Sprintf("gt-c%02d", i), 2, fmt.Sprintf("2026-09-29T10:%02d:00Z", i)))
	}
	f := newFakeSpecTown(specs...)
	f.seats = []specdispatch.Seat{
		{Agent: "claude-haiku", Cap: 3},
		{Agent: "claude-sonnet", Cap: 3},
		{Agent: "deepseek-flash", Cap: 3},
	}
	f.maxTotal = 9
	return f
}

func TestSpecDispatchSlingsFreeSeatsInParallel(t *testing.T) {
	t.Parallel()
	f := nineSeatTown(t, 12)
	env := f.env()
	env.PerTick = 0 // the free seats: nine
	// Every sling waits until all nine have started: a serial tick would
	// never get past the first one and the test would hang at the barrier,
	// which the test binary's own deadline reports (the unit tier allows no
	// timer in a test).
	const want = 9
	var barrier sync.WaitGroup
	barrier.Add(want)
	inner := env.Sling
	env.Sling = func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
		barrier.Done()
		barrier.Wait()
		return inner(c, resume, seat)
	}
	r := runSpecDispatchCycle(env)
	if len(r.Failed) != 0 || len(r.Errors) != 0 {
		t.Fatalf("failed = %+v errors = %v", r.Failed, r.Errors)
	}
	if len(r.Dispatched) != want || len(f.slung) != want {
		t.Fatalf("dispatched %d, slung %d, want %d (the free seats)", len(r.Dispatched), len(f.slung), want)
	}
	if r.SlingsRun != want {
		t.Fatalf("SlingsRun = %d, want %d", r.SlingsRun, want)
	}
	// Candidate order in the report and the record, whatever order the
	// slings finished in.
	for i, e := range r.Dispatched {
		if wantID := fmt.Sprintf("gt-c%02d", i); e.Bead != wantID || f.slung[i] != wantID {
			t.Fatalf("position %d: report %s record %s, want %s", i, e.Bead, f.slung[i], wantID)
		}
	}
	// No seat over its cap.
	counts := map[string]int{}
	for _, seat := range f.slingSeats {
		counts[seat.Agent]++
	}
	for agent, n := range counts {
		if n > 3 {
			t.Fatalf("seat %s took %d slings, cap 3: %+v", agent, n, counts)
		}
	}
	// The three left over name the limit.
	limited := 0
	for _, s := range r.Skipped {
		if strings.Contains(s.Line, specdispatch.SkipPerTickLimit) {
			limited++
		}
	}
	if limited != 3 {
		t.Fatalf("skipped for the limit = %d, want 3: %+v", limited, r.Skipped)
	}
}

func TestSpecDispatchFailedSlingIsReportedInOrderAndLabeled(t *testing.T) {
	t.Parallel()
	f := nineSeatTown(t, 4)
	f.slingErrs["gt-c01"] = []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom"), errors.New("boom")}
	env := f.env()
	env.PerTick = 0
	r := runSpecDispatchCycle(env)
	if got := strings.Join(f.slung, " "); got != "gt-c00 gt-c02 gt-c03" {
		t.Fatalf("slung %q, want the three that worked, in order", got)
	}
	if len(r.Failed) != 1 || r.Failed[0].Bead != "gt-c01" || !strings.Contains(r.Failed[0].Line, "dispatch failed") {
		t.Fatalf("failed = %+v", r.Failed)
	}
	if got := f.labels["gt-c01"]; len(got) != 1 || got[0] != specdispatch.DispatchFailedLabel {
		t.Fatalf("labels on the failed bead = %v, want the failed label once", got)
	}
	if n := len(f.notes["gt-c01"]); n != 1 {
		t.Fatalf("notes on the failed bead = %d, want the failure note once", n)
	}
}

func TestSpecDispatchPerTickCeilingStillBinds(t *testing.T) {
	t.Parallel()
	f := nineSeatTown(t, 6)
	env := f.env()
	env.PerTick = 2
	r := runSpecDispatchCycle(env)
	if len(r.Dispatched) != 2 || len(f.slung) != 2 {
		t.Fatalf("dispatched %d with max_per_tick 2 and 9 free seats, want 2", len(r.Dispatched))
	}
}

func TestSpecDispatchDefaultLimitIsTheFreeSeats(t *testing.T) {
	t.Parallel()
	f := nineSeatTown(t, 12)
	// Two seats already live on sonnet, one on flash: six free.
	f.roster = specRoster{Live: map[string]int{"claude-sonnet": 2, "deepseek-flash": 1}}
	env := f.env()
	env.PerTick = 0
	r := runSpecDispatchCycle(env)
	if len(r.Dispatched) != 6 {
		t.Fatalf("dispatched %d, want the six free seats: %s", len(r.Dispatched), r.Roster)
	}
}

func TestSpecDispatchDryRunListsTheFreeSeats(t *testing.T) {
	t.Parallel()
	f := nineSeatTown(t, 12)
	env := f.env()
	env.PerTick = 0
	env.DryRun = true
	r := runSpecDispatchCycle(env)
	if len(r.Dispatched) != 9 || len(f.slung) != 0 {
		t.Fatalf("dry run dispatched %d (slung %d), want 9 would-sling lines and no sling", len(r.Dispatched), len(f.slung))
	}
	if r.SlingsRun != 0 {
		t.Fatalf("SlingsRun = %d on a dry run", r.SlingsRun)
	}
}

func TestSpecTickCostNamesTheParallelSlings(t *testing.T) {
	t.Parallel()
	reads := newSpecTickReads()
	got := specTickCost(reads, 1500*time.Millisecond, 3, 900*time.Millisecond)
	want := "tick cost: 0 bd read call(s) outside the sling path in 1.5s; 3 sling(s) in parallel, slowest 900ms"
	if got != want {
		t.Fatalf("cost line = %q, want %q", got, want)
	}
}
