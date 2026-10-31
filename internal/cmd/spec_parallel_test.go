package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specSlingDoorWait bounds the test's check that no sling starts while the
// cap's slots are full. The check is for absence, so a short bound is what
// makes it: a cap-less tick starts a sling at once and fails the test, while a
// capped one ends the wait at the bound. A context, not time.After, is the
// unit tier's way to bound a wait.
const specSlingDoorWait = 250 * time.Millisecond

// assertNoSlingStarts fails when a sling starts before the bound passes.
func assertNoSlingStarts(t *testing.T, starts <-chan string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), specSlingDoorWait)
	defer cancel()
	select {
	case id := <-starts:
		t.Fatalf("%s started while every in-flight slot was held", id)
	case <-ctx.Done():
	}
}

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
	// The in-flight cap is raised to those seats so this test measures the
	// parallel design itself; the cap's own bound is
	// TestSpecDispatchCapsSlingsInFlight (gt-16pqa).
	env.MaxInFlight = 9
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

// A tick runs at most max_in_flight slings at once: the rest of its candidates
// wait for a slot as one frees (gt-16pqa). Every fake sling holds its slot
// until the test frees one, so a sling past the cap is kept at the cap's door
// and cannot reach the starter until a slot frees.
func TestSpecDispatchCapsSlingsInFlight(t *testing.T) {
	t.Parallel()
	const (
		inFlight = 2
		cands    = 5
	)
	f := nineSeatTown(t, cands)
	env := f.env()
	env.PerTick = 0 // the free seats: nine, so the in-flight cap is the bound
	env.MaxInFlight = inFlight
	starts := make(chan string, cands)
	release := make(chan struct{})
	var mu sync.Mutex
	running, peak := 0, 0
	inner := env.Sling
	env.Sling = func(c specCandidate, resume string, seat specdispatch.SeatChoice) (string, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		starts <- c.Spec.ID
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return inner(c, resume, seat)
	}
	done := make(chan specDispatchReport, 1)
	go func() { done <- runSpecDispatchCycle(env) }()
	started := map[string]bool{}
	// The first inFlight slings fill the slots and hold them.
	for range inFlight {
		id := <-starts
		started[id] = true
	}
	// The candidates past the cap wait for a slot, so no further sling may
	// start while both slots are held. The bound fails a cap-less tick rather
	// than hanging the test.
	assertNoSlingStarts(t, starts)
	// Each freed slot lets the next waiting sling start, until the tick has
	// run them all.
	for range cands - inFlight {
		release <- struct{}{}
		id := <-starts
		if started[id] {
			t.Fatalf("%s started twice", id)
		}
		started[id] = true
	}
	for range inFlight {
		release <- struct{}{}
	}
	r := <-done

	mu.Lock()
	defer mu.Unlock()
	if peak > inFlight {
		t.Errorf("the fake starter saw %d slings running at once, cap %d", peak, inFlight)
	}
	if len(r.Dispatched) != cands || r.SlingsRun != cands {
		t.Fatalf("dispatched %d (slings run %d), want all %d candidates", len(r.Dispatched), r.SlingsRun, cands)
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want none held out by the cap", r.Skipped)
	}
	// Every candidate was slung, and the record keeps candidate order whatever
	// order the slots freed in.
	if len(f.slung) != cands {
		t.Fatalf("slung %v, want all %d", f.slung, cands)
	}
	for i, id := range f.slung {
		if want := fmt.Sprintf("gt-c%02d", i); id != want || !started[want] {
			t.Fatalf("record position %d: slung %s (started %v), want %s", i, id, started[want], want)
		}
	}
}

// The candidates past the in-flight cap are started as slots free, not dropped:
// the cap bounds how many slings run, never how many the tick dispatches
// (gt-16pqa).
func TestSpecDispatchRemainderWaitsNotDropped(t *testing.T) {
	t.Parallel()
	const (
		inFlight = 2
		cands    = 5
	)
	f := nineSeatTown(t, cands)
	env := f.env()
	env.PerTick = 0
	env.MaxInFlight = inFlight
	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != cands || len(f.slung) != cands || r.SlingsRun != cands {
		t.Fatalf("dispatched %d (slung %d, run %d), want all %d candidates", len(r.Dispatched), len(f.slung), r.SlingsRun, cands)
	}
	if len(r.Failed) != 0 || len(r.Errors) != 0 {
		t.Fatalf("failed = %+v errors = %v", r.Failed, r.Errors)
	}
	for i, id := range f.slung {
		if want := fmt.Sprintf("gt-c%02d", i); id != want {
			t.Fatalf("slung[%d] = %s, want %s", i, id, want)
		}
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

// The tick times each phase on its own clock, so the daemon's 'tick took' line
// can name the phase a slow tick spent its time in (gt-bqch0). The clock stands
// still except inside each phase's one read, which moves it by a known step, so
// every phase reports exactly the time its read took.
func TestSpecDispatchReportsItsPhaseTimes(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T10:00:00Z"))
	env := f.env()
	var mu sync.Mutex
	now := specTestNow
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	env.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	roster, board, children, sling := env.Roster, env.Candidates, env.Children, env.Sling
	env.Roster = func() (specRoster, error) {
		advance(2 * time.Second)
		return roster()
	}
	env.Candidates = func() specBoardRead {
		advance(3 * time.Second)
		return board()
	}
	env.Children = func(id string) ([]specdispatch.Child, error) {
		advance(5 * time.Second)
		return children(id)
	}
	env.Sling = func(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice) (string, error) {
		advance(7 * time.Second)
		return sling(c, resumeBranch, seat)
	}

	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 1 {
		t.Fatalf("dispatched %d, want the one clean candidate slung", len(r.Dispatched))
	}
	want := map[string]struct{ got, want time.Duration }{
		"roster":     {r.RosterTook, 2 * time.Second},
		"board":      {r.BoardTook, 3 * time.Second},
		"candidates": {r.CandidatesTook, 5 * time.Second},
		"sling":      {r.SlingTook, 7 * time.Second},
	}
	for name, p := range want {
		if p.got != p.want {
			t.Errorf("%s phase = %v, want %v", name, p.got, p.want)
		}
	}
}

// A held tick runs no phase, so its report carries no phase times, and the
// JSON leaves the keys out for the parsers that predate them.
func TestSpecDispatchHeldTickOmitsItsPhaseTimes(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T10:00:00Z"))
	f.hold = "town parked by the operator"
	r := runSpecDispatchCycle(f.env())

	if r.Hold == "" {
		t.Fatal("report is not held, want the operator hold")
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"roster_ns", "board_ns", "candidates_ns", "sling_ns"} {
		if strings.Contains(string(out), `"`+key+`"`) {
			t.Errorf("held report JSON carries %s: %s", key, out)
		}
	}
}

// Two candidates of one tick naming the same file both dispatch: the tick holds
// the second on the first for an overlap it no longer reads, so both slings run
// and neither line reports a hold (gt-e1ufo).
func TestSpecDispatchSlingsBothCandidatesOfOneTickOnTheSameFile(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
	filing := func(id, created string) specdispatch.Spec {
		s := cleanSpec(id, 2, created)
		s.Title = "overrun guard nudge uses wait-idle under a 30s context"
		s.Description = specTestDescription + "\n\nToday " + guard + " (~254) calls Nudge with a 30 s context."
		return s
	}
	f := nineSeatTown(t, 0)
	f.specs["gt-first"] = filing("gt-first", "2026-09-29T10:03:45Z")
	f.specs["gt-second"] = filing("gt-second", "2026-09-29T10:04:00Z")
	env := f.env()
	env.PerTick = 0
	r := runSpecDispatchCycle(env)
	if got := strings.Join(f.slung, " "); got != "gt-first gt-second" {
		t.Fatalf("slung %q, want both filings dispatched", got)
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want no hold line", r.Skipped)
	}
}
