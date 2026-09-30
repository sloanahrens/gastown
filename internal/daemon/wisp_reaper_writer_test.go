package daemon

import (
	"database/sql/driver"
	"errors"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reaper"
)

// recordingReaperWriter stands in for the bd writer: it records what the
// sweep asked bd to close.
type recordingReaperWriter struct {
	mu     sync.Mutex
	closed []string
	reason string
}

func (w *recordingReaperWriter) CloseWithReason(reason string, ids ...string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reason = reason
	w.closed = append(w.closed, ids...)
	return nil
}

func (w *recordingReaperWriter) ForceCloseWithReason(reason string, ids ...string) error {
	return w.CloseWithReason("force:"+reason, ids...)
}

func (w *recordingReaperWriter) DeleteIssues(...string) error { return nil }

// TestAutoCloseDBLiveClosesThroughBd: the daemon's live auto-close hands the
// ids to bd through a writer pinned to the swept database, never to SQL
// (gt-fcxe9.12). The fake driver fails any Exec.
//
// Not parallel: it swaps reaperWriterFor.
func TestAutoCloseDBLiveClosesThroughBd(t *testing.T) {
	db, fake := openReaperSweepFake(t, [][]driver.Value{
		{"hq-a", "abandoned hq-a", time.Now().UTC().Add(-60 * 24 * time.Hour)},
		{"hq-b", "abandoned hq-b", time.Now().UTC().Add(-60 * 24 * time.Hour)},
	})
	t.Cleanup(func() { _ = db.Close() })

	w := &recordingReaperWriter{}
	var gotTown, gotDB string
	orig := reaperWriterFor
	reaperWriterFor = func(townRoot, dbName string) (reaper.Writer, error) {
		gotTown, gotDB = townRoot, dbName
		return w, nil
	}
	t.Cleanup(func() { reaperWriterFor = orig })

	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0), config: &Config{TownRoot: "/town"}}

	closed, err := d.autoCloseDB(db, "hq", reaper.MinStaleIssueAge, false)
	if err != nil {
		t.Fatalf("autoCloseDB: %v", err)
	}
	if closed != 2 {
		t.Errorf("closed = %d, want 2", closed)
	}
	if gotTown != "/town" || gotDB != "hq" {
		t.Errorf("writer resolved for (%q, %q), want (/town, hq)", gotTown, gotDB)
	}
	if !reflect.DeepEqual(w.closed, []string{"hq-a", "hq-b"}) || w.reason != reaper.StaleAutoCloseReason {
		t.Errorf("bd close got %v reason %q", w.closed, w.reason)
	}
	if writes := fake.recordedWrites(); len(writes) != 0 {
		t.Errorf("auto-close wrote SQL: %v", writes)
	}
}

// TestReaperWriterDryRunNeedsNoBd: a dry run resolves no writer at all.
func TestReaperWriterDryRunNeedsNoBd(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	w, err := d.reaperWriter("hq", true)
	if err != nil || w != nil {
		t.Errorf("dry-run reaperWriter = %v, %v; want nil, nil", w, err)
	}
	if _, err := d.reaperWriter("no_such_db", false); err == nil {
		t.Error("live reaperWriter for an unmapped database returned no error")
	}
}

// TestAutoCloseDBLiveUnmappedDatabaseIsAnError: when no beads dir names the
// swept database there is no bd to write through; with candidates to close,
// the sweep for that database fails (the step reports it) and nothing is
// written. Not parallel: it swaps reaperWriterFor.
func TestAutoCloseDBLiveUnmappedDatabaseIsAnError(t *testing.T) {
	db, fake := openReaperSweepFake(t, [][]driver.Value{
		{"orphan-a", "abandoned", time.Now().UTC().Add(-60 * 24 * time.Hour)},
	})
	t.Cleanup(func() { _ = db.Close() })
	orig := reaperWriterFor
	reaperWriterFor = func(string, string) (reaper.Writer, error) {
		return nil, errors.New("no beads directory names database \"orphan_db\"")
	}
	t.Cleanup(func() { reaperWriterFor = orig })

	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0), config: &Config{TownRoot: "/town"}}
	closed, err := d.autoCloseDB(db, "orphan_db", reaper.MinStaleIssueAge, false)
	if err == nil || !strings.Contains(err.Error(), "orphan_db") {
		t.Errorf("autoCloseDB error = %v, want one naming the unmapped database", err)
	}
	if closed != 0 {
		t.Errorf("closed = %d, want 0", closed)
	}
	if writes := fake.recordedWrites(); len(writes) != 0 {
		t.Errorf("wrote SQL: %v", writes)
	}
}
