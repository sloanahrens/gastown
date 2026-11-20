package jobs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/diskmaint"
)

// ledgerBytes appends entries through the ledger writer and returns the live
// file's bytes, so a fixture is the writer's own JSON rather than a hand copy
// of it that drifts when the writer changes.
func ledgerBytes(t *testing.T, entries ...diskmaint.LedgerEntry) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger := diskmaint.NewLedger(path, 0)
	for _, e := range entries {
		if err := ledger.Append(e); err != nil {
			t.Fatalf("append ledger entry: %v", err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the ledger fixture: %v", err)
	}
	return data
}

// errorFS is an fs.FS whose every open fails, standing for a ledger the list
// cannot read.
type errorFS struct{}

func (errorFS) Open(string) (fs.File, error) { return nil, errors.New("ledger unreadable") }

// diskmaintRowsNamed returns the disk maintenance rows among rows.
func diskmaintRowsNamed(rows []Job) []Job {
	var out []Job
	for _, j := range rows {
		if strings.HasPrefix(j.Name, diskmaintJobPrefix) {
			out = append(out, j)
		}
	}
	return out
}

// TestJobsDiskmaintLedger is bead gt-zxwd3h's first criterion: one row per
// target from its newest applied entry, with the outcome its run ended with,
// and a dry run never taking a target's place.
func TestJobsDiskmaintLedger(t *testing.T) {
	t.Parallel()
	older := testNow.Add(-2 * time.Hour)
	newer := testNow.Add(-time.Hour)
	fsys := fstest.MapFS{
		daemon.DiskMaintLedgerRelPath + ".1": {Data: ledgerBytes(t,
			diskmaint.LedgerEntry{TS: older, Target: "registry", Mode: "apply", Done: 9, BytesFreed: 900},
			diskmaint.LedgerEntry{TS: older, Target: "sizes", Mode: "apply", Done: 1},
		)},
		daemon.DiskMaintLedgerRelPath: {Data: ledgerBytes(t,
			diskmaint.LedgerEntry{TS: newer, Target: "registry", Mode: "apply", Done: 2, BytesFreed: 20, Errors: []string{"delete failed"}},
			diskmaint.LedgerEntry{TS: newer, Target: "registry", Mode: "dry-run", Done: 7},
			diskmaint.LedgerEntry{TS: newer, Target: "worktree", Mode: "apply", Done: 3, Refused: "a worktree is dirty"},
			diskmaint.LedgerEntry{TS: newer, Target: "cloud", Mode: "apply", Deferred: "push in flight"},
			diskmaint.LedgerEntry{TS: newer, Target: "baks", Mode: "apply", Done: 4, BytesFreed: 40},
		)},
	}
	rows, err := List(t.TempDir(), Options{diskmaintFS: fsys}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]struct {
		outcome Outcome
		done    int64
		freed   int64
		lastRun time.Time
	}{
		"diskmaint:registry": {OutcomeFailed, 2, 20, newer},
		"diskmaint:sizes":    {OutcomeOK, 1, 0, older},
		"diskmaint:worktree": {OutcomeRefused, 3, 0, newer},
		"diskmaint:cloud":    {OutcomeDeferred, 0, 0, newer},
		"diskmaint:baks":     {OutcomeOK, 4, 40, newer},
	}
	if got := len(diskmaintRowsNamed(rows)); got != len(want) {
		t.Fatalf("got %d disk maintenance rows, want %d", got, len(want))
	}
	for name, w := range want {
		j, ok := findJob(rows, name)
		if !ok {
			t.Errorf("no %q row", name)
			continue
		}
		if j.Outcome != w.outcome {
			t.Errorf("%s outcome = %q, want %q", name, j.Outcome, w.outcome)
		}
		if j.Kind != config.DaemonStepPatrol {
			t.Errorf("%s kind = %q, want %q", name, j.Kind, config.DaemonStepPatrol)
		}
		if j.Stats["done"] != w.done || j.Stats["bytes_freed"] != w.freed {
			t.Errorf("%s stats = %v, want done %d and bytes_freed %d", name, j.Stats, w.done, w.freed)
		}
		if j.LastRun == nil || !j.LastRun.Equal(w.lastRun) {
			t.Errorf("%s last run = %v, want %v", name, j.LastRun, w.lastRun)
		}
	}
}

// TestJobsDiskmaintLedgerBad is bead gt-zxwd3h's second criterion: a ledger the
// list cannot read is one unknown failed row, a torn last line is skipped and
// counted while the lines before it stand, and a missing ledger gives no row.
func TestJobsDiskmaintLedgerBad(t *testing.T) {
	t.Parallel()
	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		rows, err := List(t.TempDir(), Options{diskmaintFS: errorFS{}}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		j, ok := findJob(rows, diskmaintLedgerJobName)
		if !ok {
			t.Fatalf("no %q row", diskmaintLedgerJobName)
		}
		if j.State != StateUnknown || j.Outcome != OutcomeFailed {
			t.Errorf("unreadable ledger row = %q/%q, want %q/%q", j.State, j.Outcome, StateUnknown, OutcomeFailed)
		}
	})
	t.Run("torn last line", func(t *testing.T) {
		t.Parallel()
		data := append(ledgerBytes(t, diskmaint.LedgerEntry{
			TS: testNow.Add(-time.Hour), Target: "registry", Mode: "apply", Done: 1,
		}), []byte(`{"ts":"`)...)
		fsys := fstest.MapFS{daemon.DiskMaintLedgerRelPath: {Data: data}}
		rows, err := List(t.TempDir(), Options{diskmaintFS: fsys}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if j, ok := findJob(rows, "diskmaint:registry"); !ok || j.Outcome != OutcomeOK {
			t.Errorf("registry row = %+v, present %v, want the applied run before the torn line", j, ok)
		}
		j, ok := findJob(rows, diskmaintLedgerJobName)
		if !ok {
			t.Fatalf("no %q row for the torn line", diskmaintLedgerJobName)
		}
		if j.State != StateUnknown || j.Outcome != OutcomeFailed {
			t.Errorf("torn line row = %q/%q, want %q/%q", j.State, j.Outcome, StateUnknown, OutcomeFailed)
		}
		if got := j.Stats[diskmaintSkippedStat]; got != 1 {
			t.Errorf("skipped count = %d, want 1", got)
		}
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		rows, err := List(t.TempDir(), Options{diskmaintFS: fstest.MapFS{}}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := diskmaintRowsNamed(rows); len(got) != 0 {
			t.Errorf("missing ledger gave %d rows: %+v", len(got), got)
		}
	})
}

// TestJobsDiskmaintLedgerUndecodable is bead gt-zxwd3h's third criterion: a
// ledger that decodes to nothing is the ledger's own failed row naming the
// skipped count, never an empty table, while a healthy ledger of dry runs has
// no row at all.
func TestJobsDiskmaintLedgerUndecodable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		data    []byte
		skipped int64
	}{
		{
			name:    "every line fails to decode",
			data:    []byte("not a ledger line\n{\"ts\":\n"),
			skipped: 2,
		},
		{
			name: "every entry names no target",
			data: ledgerBytes(t,
				diskmaint.LedgerEntry{TS: testNow, Mode: "apply", Done: 1},
				diskmaint.LedgerEntry{TS: testNow, Mode: "dry-run"},
			),
			skipped: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{daemon.DiskMaintLedgerRelPath: {Data: tc.data}}
			rows, err := List(t.TempDir(), Options{diskmaintFS: fsys}, testNow)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := len(diskmaintRowsNamed(rows)); got != 1 {
				t.Fatalf("got %d disk maintenance rows, want the one %q row", got, diskmaintLedgerJobName)
			}
			j, ok := findJob(rows, diskmaintLedgerJobName)
			if !ok {
				t.Fatalf("no %q row", diskmaintLedgerJobName)
			}
			if j.State != StateUnknown || j.Outcome != OutcomeFailed {
				t.Errorf("row = %q/%q, want %q/%q", j.State, j.Outcome, StateUnknown, OutcomeFailed)
			}
			if got := j.Stats[diskmaintSkippedStat]; got != tc.skipped {
				t.Errorf("skipped count = %d, want %d", got, tc.skipped)
			}
		})
	}
	t.Run("healthy ledger with no applied run", func(t *testing.T) {
		t.Parallel()
		fsys := fstest.MapFS{daemon.DiskMaintLedgerRelPath: {Data: ledgerBytes(t,
			diskmaint.LedgerEntry{TS: testNow, Target: "registry", Mode: "dry-run", Done: 3},
		)}}
		rows, err := List(t.TempDir(), Options{diskmaintFS: fsys}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := diskmaintRowsNamed(rows); len(got) != 0 {
			t.Errorf("a dry run gave %d rows: %+v", len(got), got)
		}
	})
}
