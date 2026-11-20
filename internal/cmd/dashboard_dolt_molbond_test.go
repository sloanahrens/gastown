package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// molBondEntry is one call log entry the injected reader answers with.
func molBondEntry(at time.Time, verb string, exit int) bdgateway.CallEntry {
	return bdgateway.CallEntry{
		TS:       at.UTC().Format(time.RFC3339Nano),
		Caller:   "daemon",
		Verb:     verb,
		ExitCode: exit,
	}
}

// fakeDoltMolBondCalls is the section's injected gateway read: one fixed page
// (or error) for every poll, and the since and limit of each poll recorded.
type fakeDoltMolBondCalls struct {
	page    bdgateway.CallsPage
	err     error
	since   []time.Time
	limited []int
}

func (f *fakeDoltMolBondCalls) Calls(_ context.Context, since time.Time, limit int) (bdgateway.CallsPage, error) {
	f.since = append(f.since, since)
	f.limited = append(f.limited, limit)
	return f.page, f.err
}

// Only the mol bond entries are attempts, and only the failed ones are also
// failures: the other verbs in the page, including a failed one, never reach
// either count.
func TestDoltMolBondSectionCountsAttemptsAndFailures(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 20, 15, 20, 0, 0, time.UTC)
	calls := &fakeDoltMolBondCalls{page: bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		molBondEntry(start.Add(10*time.Second), "mol bond", 0),
		molBondEntry(start.Add(20*time.Second), "mol bond", 0),
		molBondEntry(start.Add(30*time.Second), "mol bond", 1),
		molBondEntry(start.Add(40*time.Second), "mol bond", 0),
		molBondEntry(start.Add(50*time.Second), "mol bond", 0),
		{TS: "not a stamp", Verb: "mol bond"},
		molBondEntry(start.Add(50*time.Second), "create", 0),
		molBondEntry(start.Add(50*time.Second), "update", 0),
		molBondEntry(start.Add(50*time.Second), "close", 1),
	}}}
	counter := newDoltMolBondCounter(calls.Calls, func() time.Time { return start }, 500)

	section, err := counter.read(context.Background(), start.Add(time.Minute))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := section.Title, "mol bond per 10 minutes"; got != want {
		t.Errorf("Title = %q, want %q", got, want)
	}
	if got, want := section.Head, []string{"ten minutes", "attempts", "failures"}; !slices.Equal(got, want) {
		t.Errorf("Head = %v, want %v", got, want)
	}
	if len(section.Rows) != 6 {
		t.Fatalf("Rows = %v, want six ten-minute buckets", section.Rows)
	}
	last := section.Rows[len(section.Rows)-1]
	if got, want := last, []string{"15:20-15:30", "5", "1"}; !slices.Equal(got, want) {
		t.Errorf("the bucket the calls fell in = %v, want %v", got, want)
	}
	if prev := section.Rows[len(section.Rows)-2]; !slices.Equal(prev, []string{"15:10-15:20", "0", "0"}) {
		t.Errorf("the bucket before it = %v, want an empty one", prev)
	}
	if !strings.Contains(section.Note, "start when the dashboard starts") {
		t.Errorf("Note = %q, want it to say the counts start when the dashboard starts", section.Note)
	}
	if !strings.Contains(section.Note, "not counted") {
		t.Errorf("Note = %q, want it to report the entry with no readable time", section.Note)
	}
	if strings.Contains(section.Note, "truncated") {
		t.Errorf("Note = %q, want no truncation for a page under the limit", section.Note)
	}
	if got, want := calls.since, []time.Time{start}; len(got) != 1 || !got[0].Equal(want[0]) {
		t.Errorf("the poll's since = %v, want %v", got, want)
	}
	if got := calls.limited; len(got) != 1 || got[0] != 500 {
		t.Errorf("the poll's limit = %v, want 500", got)
	}
}

// The cursor moves past the newest stamp, so a page served again counts
// nothing twice; a page as long as the limit says the window may be truncated.
func TestDoltMolBondSectionCountsACallOnce(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	entries := []bdgateway.CallEntry{
		molBondEntry(start.Add(10*time.Second), "mol bond", 0),
		molBondEntry(start.Add(20*time.Second), "mol bond", 1),
		molBondEntry(start.Add(30*time.Second), "create", 0),
	}
	calls := &fakeDoltMolBondCalls{page: bdgateway.CallsPage{Entries: entries}}
	counter := newDoltMolBondCounter(calls.Calls, func() time.Time { return start }, len(entries))

	first, err := counter.read(context.Background(), start.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	second, err := counter.read(context.Background(), start.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("second read: %v", err)
	}

	want := []string{"15:00-15:10", "2", "1"}
	if got := first.Rows[len(first.Rows)-1]; !slices.Equal(got, want) {
		t.Errorf("first read's last bucket = %v, want %v", got, want)
	}
	if got := second.Rows[len(second.Rows)-1]; !slices.Equal(got, want) {
		t.Errorf("second read counted the same page twice: last bucket = %v, want %v", got, want)
	}
	if got, want := calls.since, []time.Time{start, start.Add(30 * time.Second)}; len(got) != 2 || !got[0].Equal(want[0]) || !got[1].Equal(want[1]) {
		t.Errorf("the polls' since = %v, want %v", got, want)
	}
	if !strings.Contains(second.Note, "may be truncated") {
		t.Errorf("Note = %q, want it to say a page at the limit may be truncated", second.Note)
	}
}

// A gateway that cannot be read fails the section: an empty table would read as
// a gateway with no mol bond calls.
func TestDoltMolBondSectionRefusesAFailedRead(t *testing.T) {
	t.Parallel()

	boom := errors.New("the gateway at bd-gateway:7440 did not answer the calls read")
	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	calls := &fakeDoltMolBondCalls{err: boom}
	counter := newDoltMolBondCounter(calls.Calls, func() time.Time { return now }, 500)

	section, err := counter.read(context.Background(), now)
	if err == nil {
		t.Fatalf("a failed read answered section %+v", section)
	}
	if !strings.Contains(err.Error(), "bd-gateway:7440") {
		t.Errorf("the error %q does not name the gateway host", err)
	}
	if section.Head != nil || section.Rows != nil {
		t.Errorf("the failed read carries a table: %+v", section)
	}
}

// The registered read counts through the gateway the town's settings name, so a
// town with no gateway URL configured says so rather than counting nothing.
func TestDoltMolBondSectionReadRefusesNoGateway(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	read := newDoltMolBondReader(func(string) string { return "" }, func() time.Time { return now })

	if _, err := read(context.Background(), now); err == nil || !strings.Contains(err.Error(), "no gateway url") {
		t.Errorf("a read with no gateway configured answered %v, want the missing-URL refusal", err)
	}
}

// The section is registered under `molbond` by its own file.
func TestDoltMolBondSectionRegisters(t *testing.T) {
	t.Parallel()

	for _, r := range doltSections.snapshot() {
		if r.key == "molbond" {
			return
		}
	}
	t.Fatal("the Dolt panel has no molbond section registered")
}
