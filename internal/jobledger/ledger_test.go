package jobledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// testRun returns a run of job whose Stats["bytes"] is minute, so a test can
// tell the runs apart by ids.
func testRun(job string, minute int) Run {
	start := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC).Add(time.Duration(minute) * time.Minute)
	return Run{Job: job, StartedAt: start, EndedAt: start.Add(time.Second), Outcome: "ok", Stats: map[string]int64{"bytes": int64(minute)}}
}

func newLedgerAt(t *testing.T) (*Ledger, string) {
	t.Helper()
	town := t.TempDir()
	l, err := NewLedger(town)
	if err != nil {
		t.Fatal(err)
	}
	return l, town
}

func mustAppend(t *testing.T, l *Ledger, runs ...Run) {
	t.Helper()
	for _, r := range runs {
		if err := l.Append(r); err != nil {
			t.Fatalf("Append(%s, minute %d): %v", r.Job, r.Stats["bytes"], err)
		}
	}
}

func liveFile(town string) string { return filepath.Join(town, filepath.FromSlash(liveName)) }

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func encodeLines(t *testing.T, runs ...Run) []byte {
	t.Helper()
	var b []byte
	for _, r := range runs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b = append(append(b, line...), '\n')
	}
	return b
}

// ids returns the minute of each run, so a test can compare histories in order.
func ids(runs []Run) []int64 {
	out := make([]int64, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.Stats["bytes"])
	}
	return out
}

// statFailFS lets the first ok Stat calls through and refuses every Stat after,
// the one read failure a real directory cannot be made to produce on demand.
type statFailFS struct {
	fs.FS
	ok int
}

func (s *statFailFS) Stat(name string) (fs.FileInfo, error) {
	if s.ok > 0 {
		s.ok--
		return fs.Stat(s.FS, name)
	}
	return nil, errors.New("stat refused")
}

// rotateOnLiveRead runs rotate just before the live file is read, which is where
// a rotation by a concurrent writer would land between the reader's two reads.
// It runs once unless always is set.
type rotateOnLiveRead struct {
	fs.FS
	rotate func()
	always bool
}

func (r *rotateOnLiveRead) ReadFile(name string) ([]byte, error) {
	if name == liveName && r.rotate != nil {
		r.rotate()
		if !r.always {
			r.rotate = nil
		}
	}
	return fs.ReadFile(r.FS, name)
}

func TestJobLedger(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	mustAppend(t, l, testRun("a", 0), testRun("a", 1), testRun("b", 2))

	last, ok, err := l.Last("a")
	if err != nil || !ok || last.Stats["bytes"] != 1 {
		t.Fatalf("Last(a) = minute %d, ok %v, err %v; want minute 1", last.Stats["bytes"], ok, err)
	}
	got, err := l.Tail("a", 5)
	if err != nil || !slices.Equal(ids(got), []int64{0, 1}) {
		t.Fatalf("Tail(a, 5) = %v, err %v; want minutes [0 1]", ids(got), err)
	}
	if got, err := l.Tail("a", 0); err != nil || got != nil {
		t.Fatalf("Tail(a, 0) = %v, err %v; want no runs", got, err)
	}
	if _, ok, err := l.Last("c"); err != nil || ok {
		t.Fatalf("Last(c) ok %v, err %v; want no run and no error", ok, err)
	}

	// A writer that died mid-line leaves a fragment; the next run must start its own line.
	f, err := os.OpenFile(liveFile(town), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"job":"a","sta`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	mustAppend(t, l, testRun("a", 3))
	got, err = l.Tail("a", 5)
	if err != nil || !slices.Equal(ids(got), []int64{0, 1, 3}) {
		t.Fatalf("Tail(a, 5) after torn line = %v, err %v; want minutes [0 1 3]", ids(got), err)
	}
	if l.Skipped() != 1 {
		t.Fatalf("Skipped() = %d; want the one torn line counted", l.Skipped())
	}
}

func TestJobLedgerRotationKeepsNewest(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	l.maxBytes = 400
	for i := range 10 {
		mustAppend(t, l, testRun("a", i))
	}
	if _, err := os.Stat(liveFile(town) + ".1"); err != nil {
		t.Fatalf("rotation file missing: %v", err)
	}
	got, err := l.Tail("a", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := ids(got)
	if len(seen) == 0 || len(seen) >= 10 || seen[len(seen)-1] != 9 {
		t.Fatalf("Tail after rotation = %v; want the newest runs ending at minute 9, older ones dropped", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Fatalf("Tail after rotation = %v; want consecutive runs", seen)
		}
	}
}

func TestJobLedgerStatsCap(t *testing.T) {
	t.Parallel()
	stats := func(n int) map[string]int64 {
		m := make(map[string]int64, n)
		for i := range n {
			m[fmt.Sprintf("k%02d", i)] = int64(i)
		}
		return m
	}
	tests := []struct {
		name    string
		stats   map[string]int64
		wantErr bool
	}{
		{"16 keys accepted", stats(MaxStats), false},
		{"17 keys refused", stats(MaxStats + 1), true},
		{"empty key refused", map[string]int64{"": 1}, true},
		{"padded key refused", map[string]int64{" bytes ": 1}, true},
		{"32-byte key accepted", map[string]int64{strings.Repeat("k", 32): 1}, false},
		{"33-byte key refused", map[string]int64{strings.Repeat("k", 33): 1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedgerAt(t)
			r := testRun("a", 0)
			r.Stats = tc.stats
			err := l.Append(r)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Append accepted the run; want a refusal")
				}
				if _, ok, _ := l.Last("a"); ok {
					t.Fatal("refused run was stored")
				}
				return
			}
			if err != nil {
				t.Fatalf("Append refused a valid run: %v", err)
			}
			if _, ok, err := l.Last("a"); err != nil || !ok {
				t.Fatalf("Last after accepted run ok %v, err %v; want the run", ok, err)
			}
		})
	}
}

func TestJobLedgerRefusesBadRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		job  string
	}{
		{"empty job", ""},
		{"padded job", " a "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := newLedgerAt(t)
			if err := l.Append(testRun(tc.job, 0)); err == nil {
				t.Fatal("Append accepted a run with a bad job name; want a refusal")
			}
		})
	}
}

func TestJobLedgerReadRefusesBadJob(t *testing.T) {
	t.Parallel()
	l, _ := newLedgerAt(t)
	if _, _, err := l.Last(""); err == nil {
		t.Fatal("Last(\"\") returned no error; want a refusal")
	}
	if _, err := l.Tail(" a ", 1); err == nil {
		t.Fatal("Tail with a padded job returned no error; want a refusal")
	}
}

func TestJobLedgerNewLedgerRefusesEmptyRoot(t *testing.T) {
	t.Parallel()
	if _, err := NewLedger(""); err == nil {
		t.Fatal("NewLedger(\"\") returned no error; want a refusal")
	}
}

func TestJobLedgerAppendRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		run   Run
		setup func(t *testing.T, l *Ledger, town string)
	}{
		{
			name: "marshal fails on a year past 9999",
			run:  Run{Job: "a", StartedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		{
			name: "jobs directory cannot be created",
			run:  testRun("a", 0),
			setup: func(t *testing.T, _ *Ledger, town string) {
				writeFile(t, filepath.Join(town, ".runtime"), "not a directory")
			},
		},
		{
			name: "lock refused",
			run:  testRun("a", 0),
			setup: func(t *testing.T, l *Ledger, _ string) {
				l.acquire = func(string) (func(), error) { return nil, errors.New("lock refused") }
			},
		},
		{
			name: "live path is a directory",
			run:  testRun("a", 0),
			setup: func(t *testing.T, _ *Ledger, town string) {
				mkdir(t, liveFile(town))
			},
		},
		{
			name: "rotation rename blocked by a directory",
			run:  testRun("a", 1),
			setup: func(t *testing.T, l *Ledger, town string) {
				l.maxBytes = 1
				mustAppend(t, l, testRun("a", 0))
				mkdir(t, liveFile(town)+".1")
			},
		},
		{
			name: "live file cannot be statted",
			run:  testRun("a", 1),
			setup: func(t *testing.T, l *Ledger, _ string) {
				mustAppend(t, l, testRun("a", 0))
				l.statOpen = func(*os.File) (fs.FileInfo, error) { return nil, errors.New("stat refused") }
			},
		},
		{
			name: "open refused",
			run:  testRun("a", 0),
			setup: func(t *testing.T, l *Ledger, _ string) {
				l.open = func(string) (*os.File, error) { return nil, errors.New("open refused") }
			},
		},
		{
			name: "write refused",
			run:  testRun("a", 0),
			setup: func(t *testing.T, l *Ledger, _ string) {
				l.write = func(*os.File, []byte) (int, error) { return 0, errors.New("write refused") }
			},
		},
		{
			name: "short write",
			run:  testRun("a", 0),
			setup: func(t *testing.T, l *Ledger, _ string) {
				l.write = func(_ *os.File, b []byte) (int, error) { return len(b) - 1, nil }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, town := newLedgerAt(t)
			if tc.setup != nil {
				tc.setup(t, l, town)
			}
			if err := l.Append(tc.run); err == nil {
				t.Fatal("Append returned no error; want a refusal")
			}
		})
	}
}

func TestJobLedgerRefusesUnreadableFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, l *Ledger, town string)
	}{
		{
			name: "live file is a directory",
			setup: func(t *testing.T, _ *Ledger, town string) {
				mkdir(t, liveFile(town))
			},
		},
		{
			name: "rotation file is a directory",
			setup: func(t *testing.T, _ *Ledger, town string) {
				mkdir(t, liveFile(town)+".1")
			},
		},
		{
			name: "stat refused",
			setup: func(_ *testing.T, l *Ledger, _ string) {
				l.readFS = &statFailFS{FS: l.readFS}
			},
		},
		{
			name: "stat refused after the reads",
			setup: func(_ *testing.T, l *Ledger, _ string) {
				l.readFS = &statFailFS{FS: l.readFS, ok: 2}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, town := newLedgerAt(t)
			tc.setup(t, l, town)
			if _, err := l.Tail("a", 5); err == nil {
				t.Fatal("Tail returned no error for an unreadable ledger; want an error, never an empty history")
			}
			if _, _, err := l.Last("a"); err == nil {
				t.Fatal("Last returned no error for an unreadable ledger; want an error")
			}
		})
	}
}

func TestJobLedgerSkippedResetsOnFailure(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	mustAppend(t, l, testRun("a", 0))
	f, err := os.OpenFile(liveFile(town), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"job":"a","sta`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := l.Tail("a", 5); err != nil {
		t.Fatal(err)
	}
	if l.Skipped() != 1 {
		t.Fatalf("Skipped() = %d before the failure; want 1", l.Skipped())
	}
	l.readFS = &statFailFS{FS: l.readFS}
	if _, err := l.Tail("a", 5); err == nil {
		t.Fatal("Tail returned no error after the stat was refused")
	}
	if got := l.Skipped(); got != 0 {
		t.Fatalf("Skipped() = %d after a failed read; want the stale count cleared", got)
	}
}

func TestJobLedgerReadRetriesAcrossRotation(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	mustAppend(t, l, testRun("a", 0), testRun("a", 1))

	livePath := liveFile(town)
	hook := &rotateOnLiveRead{FS: os.DirFS(town), rotate: func() {
		if err := os.Rename(livePath, livePath+".1"); err != nil {
			t.Errorf("rotate: %v", err)
		}
	}}
	l.readFS = hook

	got, err := l.Tail("a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hook.rotate != nil {
		t.Fatal("rotation hook never ran; the test did not exercise the race")
	}
	if !slices.Equal(ids(got), []int64{0, 1}) {
		t.Fatalf("Tail across a rotation = %v; want the complete history [0 1]", ids(got))
	}
}

func TestJobLedgerReadOnly(t *testing.T) {
	t.Parallel()
	l, _ := newLedgerAt(t)
	l.readFS = fstest.MapFS{
		olderName: {Data: encodeLines(t, testRun("a", 0), testRun("b", 1))},
		liveName:  {Data: append(encodeLines(t, testRun("a", 2)), "not json\n{\"outcome\":\"ok\"}\n"...)},
	}

	last, ok, err := l.Last("a")
	if err != nil || !ok || last.Stats["bytes"] != 2 {
		t.Fatalf("Last(a) = minute %d, ok %v, err %v; want minute 2", last.Stats["bytes"], ok, err)
	}
	got, err := l.Tail("a", 10)
	if err != nil || !slices.Equal(ids(got), []int64{0, 2}) {
		t.Fatalf("Tail(a, 10) = %v, err %v; want minutes [0 2]", ids(got), err)
	}
	if l.Skipped() != 2 {
		t.Fatalf("Skipped() = %d; want the bad line and the job-less line counted", l.Skipped())
	}
}

func TestJobLedgerMissingLedgerIsEmpty(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	if _, ok, err := l.Last("a"); err != nil || ok {
		t.Fatalf("Last on a missing ledger ok %v, err %v; want no run and no error", ok, err)
	}
	if got, err := l.Tail("a", 5); err != nil || len(got) != 0 {
		t.Fatalf("Tail on a missing ledger = %v, err %v; want no runs", got, err)
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading created .runtime: stat err %v; readers must write nothing", err)
	}
}

func TestJobLedgerReadGivesUpWhenRotationNeverSettles(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	l.maxBytes = 1
	mustAppend(t, l, testRun("a", 0))
	// Each rotation leaves a file of a new size, so the snapshot sees a change on
	// every attempt. Equal sizes could match inside one filesystem timestamp tick.
	rotations := 0
	l.readFS = &rotateOnLiveRead{FS: os.DirFS(town), rotate: func() {
		rotations++
		r := testRun("a", 9)
		r.Outcome = strings.Repeat("x", rotations)
		if err := l.Append(r); err != nil {
			t.Errorf("rotating Append: %v", err)
		}
	}, always: true}

	if _, err := l.Tail("a", 5); err == nil {
		t.Fatal("Tail returned no error while every read was rotated; want an error, never a partial history")
	}
}

func TestJobLedgerTailStateRefusesUnopenableLive(t *testing.T) {
	t.Parallel()
	l, town := newLedgerAt(t)
	writeFile(t, filepath.Join(town, ".runtime"), "not a directory")
	if _, _, err := l.tailState(); err == nil {
		t.Fatal("tailState returned no error for a live path it cannot open; want a refusal")
	}
}
