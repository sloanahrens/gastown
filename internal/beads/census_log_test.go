package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// The census sink carries a process's rows to the process that logs them, as
// the row text the log line uses, and reads back what it wrote (gt-139dp).
func TestCensusSinkRoundTripsItsRows(t *testing.T) {
	t.Parallel()
	// A directory of its own that does not exist yet: the sink makes it.
	path := filepath.Join(t.TempDir(), "daemon", "spec_census.txt")
	want := []CensusRow{
		{Verb: "mol wisp", Caller: "cmd.specRoster", Count: 30},
		{Verb: "show", Caller: "cmd.specBoard", Count: 7},
	}
	if err := WriteCensusSink(path, want); err != nil {
		t.Fatalf("WriteCensusSink: %v", err)
	}
	got, err := ReadCensusSink(path)
	if err != nil {
		t.Fatalf("ReadCensusSink: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows read back = %+v, want %+v", got, want)
	}
	if total := CensusRowsTotal(got); total != 37 {
		t.Errorf("total = %d, want 37", total)
	}
}

// A row the reader cannot parse fails the read: a dropped row is a call count
// the daemon never sees (gt-139dp).
func TestCensusSinkFailsOnARowItCannotRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "spec_census.txt")
	if err := os.WriteFile(path, []byte("show@cmd.a=2\nnot-a-row\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows, err := ReadCensusSink(path); err == nil {
		t.Fatalf("rows = %+v, want an error for the unreadable row", rows)
	}
	if _, err := ReadCensusSink(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("reading a missing sink succeeded")
	}
}

// A sink whose directory the writer cannot make, or whose file it cannot open,
// is an error: the tick that could not write its census says so rather than
// reading as no calls (gt-139dp).
func TestWriteCensusSinkFailsOnAPathItCannotWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCensusSink(filepath.Join(blocker, "census.txt"), nil); err == nil {
		t.Error("writing the sink under a regular file succeeded")
	}
	asDir := filepath.Join(dir, "census.txt")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteCensusSink(asDir, []CensusRow{{Verb: "show", Caller: "cmd.a", Count: 1}}); err == nil {
		t.Error("writing the sink over a directory succeeded")
	}
	// A directory where the sink's temp file goes stops the write at the open.
	tmpBlocked := filepath.Join(dir, "tmp_blocked.txt")
	if err := os.Mkdir(tmpBlocked+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteCensusSink(tmpBlocked, nil); err == nil {
		t.Error("writing the sink whose temp path is a directory succeeded")
	}
}

// A reader racing the writer reads one whole snapshot or the other, never a
// line torn in half: the sink writes a temp file and renames it over the census
// (gt-139dp).
func TestCensusSinkReplacesTheFileWhole(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "spec_census.txt")
	snapshot := func(prefix string) []CensusRow {
		rows := make([]CensusRow, 64)
		for i := range rows {
			rows[i] = CensusRow{Verb: "show", Caller: fmt.Sprintf("%s.call%d", prefix, i), Count: i + 1}
		}
		return rows
	}
	first, second := snapshot("cmd.first"), snapshot("cmd.second")
	if err := WriteCensusSink(path, first); err != nil {
		t.Fatal(err)
	}

	writeErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			rows := first
			if i%2 == 1 {
				rows = second
			}
			if err := WriteCensusSink(path, rows); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	reads := 0
	for {
		rows, err := ReadCensusSink(path)
		if err != nil {
			t.Fatalf("a reader saw a torn census: %v", err)
		}
		if !reflect.DeepEqual(rows, first) && !reflect.DeepEqual(rows, second) {
			t.Fatalf("reader saw %d rows, neither whole snapshot", len(rows))
		}
		reads++
		select {
		case <-done:
			select {
			case err := <-writeErr:
				t.Fatalf("writer: %v", err)
			default:
			}
			if reads == 0 {
				t.Fatal("the reader never read the sink")
			}
			return
		default:
		}
	}
}

// The top-rows text names the busiest censusLogRows and nothing of an empty
// census (gt-139dp).
func TestCensusTopRowsCapsItsRows(t *testing.T) {
	t.Parallel()
	if got := CensusTopRows(nil); got != "" {
		t.Errorf("CensusTopRows(nil) = %q, want empty", got)
	}
	rows := make([]CensusRow, censusLogRows+1)
	for i := range rows {
		rows[i] = CensusRow{Verb: fmt.Sprintf("v%d", i), Caller: "cmd.a", Count: len(rows) - i}
	}
	got := CensusTopRows(rows)
	if n := strings.Count(got, "@cmd.a="); n != censusLogRows {
		t.Errorf("CensusTopRows names %d rows, want %d: %q", n, censusLogRows, got)
	}
	if !strings.HasPrefix(got, "top: v0@cmd.a=") {
		t.Errorf("CensusTopRows = %q, want the busiest row first", got)
	}
}
