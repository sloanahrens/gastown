package beads

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// censusLogSink records what StartCensusLog logs and signals each line, so a
// test waits on the log itself instead of sleeping and cannot race the
// goroutine's snapshot.
type censusLogSink struct {
	mu    sync.Mutex
	lines []string
	wrote chan struct{}
}

func newCensusLogSink() *censusLogSink {
	return &censusLogSink{wrote: make(chan struct{}, 64)}
}

func (s *censusLogSink) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.lines = append(s.lines, line)
	s.mu.Unlock()
	s.wrote <- struct{}{}
}

// next waits for one more line and returns it.
func (s *censusLogSink) next(t *testing.T) string {
	t.Helper()
	select {
	case <-s.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no census line within 5s")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines[len(s.lines)-1]
}

func (s *censusLogSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// censusLogRun is one StartCensusLog goroutine the test drives by hand.
type censusLogRun struct {
	sink   *censusLogSink
	every  chan time.Time
	cancel context.CancelFunc
	done   chan struct{}
}

func startCensusLog(t *testing.T) *censusLogRun {
	t.Helper()
	resetCensus(t)
	run := &censusLogRun{
		sink:  newCensusLogSink(),
		every: make(chan time.Time),
		done:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go func() {
		defer close(run.done)
		StartCensusLog(ctx, run.every, run.sink.logf)
	}()
	t.Cleanup(func() { run.stop(t) })
	return run
}

// waitDone fails unless StartCensusLog has returned.
func (r *censusLogRun) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCensusLog did not return")
	}
}

func (r *censusLogRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	r.waitDone(t)
}

// tick delivers one tick to the goroutine, failing rather than blocking the
// package when StartCensusLog has stopped receiving.
func (r *censusLogRun) tick(t *testing.T, at time.Time) {
	t.Helper()
	select {
	case r.every <- at:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCensusLog stopped receiving ticks")
	}
}

func TestCensusLogLogsOneLinePerWindow(t *testing.T) {
	run := startCensusLog(t)

	censusRecord([]string{"show"})
	censusRecord([]string{"show"})
	censusRecord([]string{"show"})
	censusRecord([]string{"list"})
	first := time.Now()
	run.tick(t, first)
	line := run.sink.next(t)
	if want := "bd census: total=4 window="; !strings.HasPrefix(line, want) {
		t.Errorf("first line = %q, want prefix %q", line, want)
	}
	if !strings.Contains(line, " top: ") {
		t.Errorf("first line = %q, want the top: label before the rows", line)
	}
	rows := strings.Fields(line)[5:]
	if len(rows) != 2 {
		t.Fatalf("first line lists %d rows, want 2: %q", len(rows), line)
	}
	if verb, count := censusLogRow(t, rows[0]); verb != "show" || count != "3" {
		t.Errorf("busiest row = %q, want show with 3 calls", rows[0])
	}
	if verb, count := censusLogRow(t, rows[1]); verb != "list" || count != "1" {
		t.Errorf("second row = %q, want list with 1 call", rows[1])
	}

	censusRecord([]string{"create"})
	censusRecord([]string{"create"})
	run.tick(t, first.Add(time.Minute))
	line = run.sink.next(t)
	if want := "bd census: total=2 window=60s top: create@"; !strings.HasPrefix(line, want) {
		t.Errorf("second line = %q, want prefix %q", line, want)
	}
	if !strings.HasSuffix(line, "=2") {
		t.Errorf("second line = %q, want the two create calls", line)
	}

	run.stop(t)
	if lines := run.sink.all(); len(lines) != 2 {
		t.Errorf("logged %d lines, want one per window: %q", len(lines), lines)
	}
}

func TestCensusLogSilentWindowLogsNothing(t *testing.T) {
	run := startCensusLog(t)
	base := time.Now()

	censusRecord([]string{"show"})
	run.tick(t, base)
	run.sink.next(t) // window 1 is snapshotted, so the next record starts window 3

	run.tick(t, base.Add(time.Minute))   // window 2: nothing recorded
	run.tick(t, base.Add(2*time.Minute)) // the tick lands only after window 2 is done
	censusRecord([]string{"show"})       // window 3 or 4, never the silent one
	run.tick(t, base.Add(3*time.Minute))
	line := run.sink.next(t)
	if want := "bd census: total=1 window=60s top: "; !strings.HasPrefix(line, want) {
		t.Errorf("line = %q, want prefix %q (a silent window must log nothing)", line, want)
	}
	if verb, count := censusLogRow(t, strings.Fields(line)[5]); verb != "show" || count != "1" {
		t.Errorf("line = %q, want the single show call", line)
	}

	run.stop(t)
	if lines := run.sink.all(); len(lines) != 2 {
		t.Errorf("logged %d lines, want only the windows with calls: %q", len(lines), lines)
	}
}

func TestCensusLogListsEightBusiestRows(t *testing.T) {
	run := startCensusLog(t)

	verbs := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	for i, verb := range verbs {
		for n := 0; n < len(verbs)-i; n++ {
			censusRecord([]string{verb})
		}
	}
	run.tick(t, time.Now())
	line := run.sink.next(t)

	if want := "bd census: total=78 window="; !strings.HasPrefix(line, want) {
		t.Fatalf("line = %q, want prefix %q", line, want)
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[4] != "top:" {
		t.Fatalf("line = %q, want the top: label before the rows", line)
	}
	rows := fields[5:]
	if len(rows) != censusLogRows {
		t.Fatalf("line lists %d rows, want %d: %q", len(rows), censusLogRows, line)
	}
	for i, row := range rows {
		verb, count := censusLogRow(t, row)
		if want := verbs[i]; verb != want {
			t.Errorf("row %d = %q, want verb %q (descending count, busiest first)", i, row, want)
		}
		if want := fmt.Sprintf("%d", len(verbs)-i); count != want {
			t.Errorf("row %d = %q, want count %s", i, row, want)
		}
	}
}

// censusLogRow splits "verb@caller=count" into its verb and count; the caller
// is whatever frame the test binary leaves, so the log tests do not name it.
func censusLogRow(t *testing.T, field string) (verb, count string) {
	t.Helper()
	at, eq := strings.LastIndex(field, "@"), strings.LastIndex(field, "=")
	if at < 0 || eq < at+1 {
		t.Fatalf("row %q is not verb@caller=count", field)
	}
	return field[:at], field[eq+1:]
}

func TestCensusLogStopsOnCancelAndChannelClose(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		run := startCensusLog(t)
		run.stop(t)
	})

	t.Run("channel close", func(t *testing.T) {
		run := startCensusLog(t)
		close(run.every)
		run.waitDone(t)
	})
}
