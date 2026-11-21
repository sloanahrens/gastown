package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
)

// readNodeDiskState reads the town's node disk record, failing the test when it
// cannot be read or parsed.
func readNodeDiskState(t *testing.T, townRoot string) nodeDiskState {
	t.Helper()
	data, err := os.ReadFile(nodeDiskRecordPath(townRoot))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var state nodeDiskState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parsing the record: %v\n%s", err, data)
	}
	return state
}

// byteCount is the address of a byte count, for a nodeDiskEntry that keeps one.
func byteCount(v int64) *int64 { return &v }

// wantNodeDiskReading asserts a stored entry holds the given figures and read
// time, and no error.
func wantNodeDiskReading(t *testing.T, state nodeDiskState, node string, percent int, used, capacity int64, readAt time.Time) {
	t.Helper()
	rec, ok := state.Nodes[node]
	if !ok {
		t.Fatalf("record holds no entry for %s: %+v", node, state.Nodes)
	}
	if rec.UsedPercent == nil || *rec.UsedPercent != percent {
		t.Errorf("%s used_percent = %v, want %d", node, rec.UsedPercent, percent)
	}
	if rec.UsedBytes == nil || *rec.UsedBytes != used {
		t.Errorf("%s used_bytes = %v, want %d", node, rec.UsedBytes, used)
	}
	if rec.CapacityBytes == nil || *rec.CapacityBytes != capacity {
		t.Errorf("%s capacity_bytes = %v, want %d", node, rec.CapacityBytes, capacity)
	}
	if rec.ReadAt == nil || !rec.ReadAt.Equal(readAt) {
		t.Errorf("%s read_at = %v, want %v", node, rec.ReadAt, readAt)
	}
	if rec.Error != "" || rec.ErrorAt != nil {
		t.Errorf("%s holds error %q at %v, want none", node, rec.Error, rec.ErrorAt)
	}
}

// A pass that returns readings records each node's whole-percent share, byte
// counts and read time; a later pass whose node fails keeps that node's figures
// and adds the error and its time.
func TestHostNodeDiskRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	listings := 0
	failWorker := false
	cli := newFakeCLIFor(func(c cliCall) cliReply {
		if failWorker && cacheTrimVerb(c.args) == "get --raw /api/v1/nodes/colima-gt-worker/proxy/stats/summary" {
			return cliReply{code: 1}
		}
		return nodeDiskTwoNodes(&listings)(c)
	})
	d := nodeDiskDaemon(t, cli, nodeDiskPatrol(ciCacheTestContext))
	fc := clockwork.NewFakeClockAt(now)
	d.clock = fc

	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.config.TownRoot, ".runtime", "jobs", "node_disk.json")); err != nil {
		t.Fatalf("record is not at .runtime/jobs/node_disk.json: %v", err)
	}
	state := readNodeDiskState(t, d.config.TownRoot)
	// nodeSummary(capacity, available) is the byte counts the read takes.
	wantNodeDiskReading(t, state, "colima-gt-control", 20, 20, 100, now)
	wantNodeDiskReading(t, state, "colima-gt-worker", 90, 90, 100, now)

	failWorker = true
	later := now.Add(5 * time.Minute)
	fc.Advance(5 * time.Minute)
	if _, err := d.controlNodeDisk(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	state = readNodeDiskState(t, d.config.TownRoot)
	wantNodeDiskReading(t, state, "colima-gt-control", 20, 20, 100, later)

	worker, ok := state.Nodes["colima-gt-worker"]
	if !ok {
		t.Fatalf("record holds no entry for the failed node: %+v", state.Nodes)
	}
	if worker.UsedPercent == nil || *worker.UsedPercent != 90 ||
		worker.UsedBytes == nil || *worker.UsedBytes != 90 ||
		worker.CapacityBytes == nil || *worker.CapacityBytes != 100 {
		t.Errorf("failed node figures = %+v, want its previous 90%% of 100 bytes kept", worker)
	}
	if worker.ReadAt == nil || !worker.ReadAt.Equal(now) {
		t.Errorf("failed node read_at = %v, want the previous reading's %v", worker.ReadAt, now)
	}
	if worker.Error == "" {
		t.Errorf("failed node holds no error, want the failure recorded")
	}
	if worker.ErrorAt == nil || !worker.ErrorAt.Equal(later) {
		t.Errorf("failed node error_at = %v, want %v", worker.ErrorAt, later)
	}
}

// A record that cannot be written logs one line and leaves the collector's
// items exactly as a writable town's.
func TestHostNodeDiskRecordWriteFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	collect := func(t *testing.T, breakJobs bool) ([]attention.Item, string) {
		t.Helper()
		f := newAttentionFixture(t, now)
		d := f.d
		listings := 0
		cli := newFakeCLIFor(nodeDiskTwoNodes(&listings))
		d.execCmd = cli.run
		d.patrolConfig = nodeDiskPatrol(ciCacheTestContext)
		d.diskMaint.getenv = func(string) string { return "" }
		d.diskMaint.environ = func() []string { return []string{"PATH=/usr/bin", "HOME=/home/polecat"} }
		if breakJobs {
			// The record's directory cannot be created: the real function runs and
			// its mkdir fails, as it does for a read-only town root, as root too.
			d.diskMaint.nodeDiskFS = nodeDiskFS{mkdirAll: func(string, os.FileMode) error {
				return &fs.PathError{Op: "mkdir", Path: d.config.TownRoot, Err: fs.ErrPermission}
			}}
		}
		f.src.host = newFileHostProbes(d.config.TownRoot, nil, d.controlNodeDisk)
		return f.collect(t, f.src.collectHostNodeDisk), f.logs.String()
	}

	items, logs := collect(t, false)
	if logs != "" {
		t.Fatalf("a writable town logged %q, want nothing", logs)
	}
	if len(items) == 0 {
		t.Fatalf("items = none, want the node's item to compare")
	}

	broken, brokenLogs := collect(t, true)
	if !reflect.DeepEqual(broken, items) {
		t.Errorf("items with an unwritable record = %+v, want the same as with a writable one: %+v", broken, items)
	}
	lines := strings.Split(strings.TrimSpace(brokenLogs), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "not recorded") || !strings.Contains(lines[0], "permission denied") {
		t.Errorf("log = %q, want one line saying the readings were not recorded and why", brokenLogs)
	}
}

// recordNodeDiskIn is the real function: a failing mkdir or a failing write
// reaches it through the seam and comes back as the error the step returned.
func TestRecordNodeDiskStepFailures(t *testing.T) {
	t.Parallel()
	denied := func(op string) error { return &fs.PathError{Op: op, Path: "x", Err: fs.ErrPermission} }
	for _, tc := range []struct {
		name string
		fs   nodeDiskFS
		op   string
	}{
		{"mkdir", nodeDiskFS{mkdirAll: func(string, os.FileMode) error { return denied("mkdir") }}, "mkdir"},
		{"write", nodeDiskFS{writeJSON: func(string, any) error { return denied("open") }}, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := recordNodeDiskIn(t.TempDir(), map[string]nodeDiskEntry{"n": {UsedPercent: 1}}, tc.fs)
			var pe *fs.PathError
			if !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pe) || pe.Op != tc.op {
				t.Fatalf("recordNodeDiskIn = %v, want the %s step's permission error", err, tc.name)
			}
		})
	}
}

// recordNodeDisk keeps an entry it was not given, replaces the one it was, and
// two concurrent calls lose neither.
func TestRecordNodeDiskMerge(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := recordNodeDisk(town, map[string]nodeDiskEntry{
		"colima-gt-control": {UsedPercent: 20, UsedBytes: byteCount(20), CapacityBytes: byteCount(100), ReadAt: now},
		"colima":            {UsedPercent: 84, UsedBytes: byteCount(840), CapacityBytes: byteCount(1000), ReadAt: now},
	}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	later := now.Add(time.Minute)
	if err := recordNodeDisk(town, map[string]nodeDiskEntry{
		"colima-gt-control": {UsedPercent: 67, UsedBytes: byteCount(67), CapacityBytes: byteCount(100), ReadAt: later},
	}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	state := readNodeDiskState(t, town)
	wantNodeDiskReading(t, state, "colima-gt-control", 67, 67, 100, later)
	wantNodeDiskReading(t, state, "colima", 84, 840, 1000, now)

	// Two writers at a time, repeated with fresh names, so a lost read-modify-
	// write update shows up rather than depending on one lucky interleaving.
	var nodes []string
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		for _, suffix := range []string{"one", "two"} {
			node := fmt.Sprintf("%s-%d", suffix, round)
			nodes = append(nodes, node)
			wg.Add(1)
			go func(node string) {
				defer wg.Done()
				if err := recordNodeDisk(town, map[string]nodeDiskEntry{
					node: {UsedPercent: 50, UsedBytes: byteCount(500), CapacityBytes: byteCount(1000), ReadAt: later},
				}); err != nil {
					t.Errorf("concurrent write of %s: %v", node, err)
				}
			}(node)
		}
		wg.Wait()
	}
	state = readNodeDiskState(t, town)
	for _, node := range nodes {
		wantNodeDiskReading(t, state, node, 50, 500, 1000, later)
	}
	wantNodeDiskReading(t, state, "colima", 84, 840, 1000, now)
}

// A record that cannot be read is refused rather than overwritten, so one
// pass's readings do not stand in for the entries the file already held.
func TestRecordNodeDiskRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := nodeDiskRecordPath(town)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	err := recordNodeDisk(town, map[string]nodeDiskEntry{
		"colima-gt-control": {UsedPercent: 20, UsedBytes: byteCount(20), CapacityBytes: byteCount(100), ReadAt: time.Now()},
	})
	if err == nil || !strings.Contains(err.Error(), "read "+nodeDiskRecordFile) {
		t.Fatalf("recordNodeDisk = %v, want the read failure", err)
	}
	// Nothing may be written after the refusal: no temp file left beside the
	// refused record.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != nodeDiskRecordFile {
			t.Errorf("jobs holds %s after the refused write, want only the refused record", e.Name())
		}
	}
}

// A corrupt record is replaced, as savePatrolLastRun replaces a corrupt
// last-run file: the pass has run and the record must be repairable.
func TestRecordNodeDiskReplacesACorruptFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := nodeDiskRecordPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if err := recordNodeDisk(town, map[string]nodeDiskEntry{
		"colima-gt-control": {UsedPercent: 20, UsedBytes: byteCount(20), CapacityBytes: byteCount(100), ReadAt: now},
	}); err != nil {
		t.Fatalf("recordNodeDisk: %v", err)
	}
	wantNodeDiskReading(t, readNodeDiskState(t, town), "colima-gt-control", 20, 20, 100, now)
}
