package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/daemon"
)

// fakeArchiveGapReader answers the archive counts from maps, and records what
// archiveGap asked, so a test can see which watermark reached each database and
// that no database is read after one fails.
type fakeArchiveGapReader struct {
	commits   map[string]int64
	events    map[string]int64
	commitErr map[string]error
	eventErr  map[string]error
	asked     []string
	// watermarks records the commit and events watermark each database was
	// counted against, by database name.
	commitWatermarks map[string]daemon.ArchiveWatermark
	eventWatermarks  map[string]daemon.EventsArchiveWatermark
}

func (r *fakeArchiveGapReader) commitsBehind(_ context.Context, db string, wm daemon.ArchiveWatermark) (int64, error) {
	r.asked = append(r.asked, db+"/commits")
	if r.commitWatermarks == nil {
		r.commitWatermarks = map[string]daemon.ArchiveWatermark{}
	}
	r.commitWatermarks[db] = wm
	if err := r.commitErr[db]; err != nil {
		return 0, err
	}
	return r.commits[db], nil
}

func (r *fakeArchiveGapReader) eventsBehind(_ context.Context, db string, wm daemon.EventsArchiveWatermark) (int64, error) {
	r.asked = append(r.asked, db+"/events")
	if r.eventWatermarks == nil {
		r.eventWatermarks = map[string]daemon.EventsArchiveWatermark{}
	}
	r.eventWatermarks[db] = wm
	if err := r.eventErr[db]; err != nil {
		return 0, err
	}
	return r.events[db], nil
}

// TestArchiveGapSumsCommitsAndEvents: every database contributes both its
// commit count and its events count, each counted against that database's own
// watermark; nothing is dropped and nothing is counted twice.
func TestArchiveGapSumsCommitsAndEvents(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	reader := &fakeArchiveGapReader{
		commits: map[string]int64{"gt": 2, "hq": 0, "rig-a": 5},
		events:  map[string]int64{"gt": 1, "hq": 3, "rig-a": 0},
	}
	wms := daemon.ArchiveWatermarks{
		Commits: map[string]daemon.ArchiveWatermark{
			"gt":    {Date: when, Hashes: []string{"a"}},
			"rig-a": {Date: when.Add(24 * time.Hour), Hashes: []string{"b"}},
		},
		Events: map[string]daemon.EventsArchiveWatermark{
			"gt":    {CreatedAt: "2026-11-02T09:00:00Z"},
			"rig-a": {CreatedAt: "2026-11-01T09:00:00Z"},
		},
	}

	commits, events, err := archiveGap(context.Background(), reader, wms, []string{"gt", "hq", "rig-a"})
	if err != nil {
		t.Fatalf("archive gap: %v", err)
	}
	if commits != 7 {
		t.Errorf("commits behind = %d, want 7 (2+0+5)", commits)
	}
	if events != 4 {
		t.Errorf("events behind = %d, want 4 (1+3+0)", events)
	}
	if got := reader.commitWatermarks["rig-a"].Date; !got.Equal(when.Add(24 * time.Hour)) {
		t.Errorf("rig-a counted against watermark %v, want its own %v", got, when.Add(24*time.Hour))
	}
	if got := reader.eventWatermarks["gt"].CreatedAt; got != "2026-11-02T09:00:00Z" {
		t.Errorf("gt events counted against watermark %q, want its own", got)
	}
}

// TestArchiveGapUncountedEventsCountAsOne: an events answer of
// eventsBehindUncounted says the table holds rows the reader did not count. It
// adds one behind, never the sentinel's -1, which would cancel another
// database's backlog out of the sum.
func TestArchiveGapUncountedEventsCountAsOne(t *testing.T) {
	t.Parallel()
	reader := &fakeArchiveGapReader{
		commits: map[string]int64{"gt": 0, "hq": 0},
		events:  map[string]int64{"gt": eventsBehindUncounted, "hq": 2},
	}

	commits, events, err := archiveGap(context.Background(), reader, daemon.ArchiveWatermarks{}, []string{"gt", "hq"})
	if err != nil {
		t.Fatalf("archive gap: %v", err)
	}
	if commits != 0 {
		t.Errorf("commits behind = %d, want 0", commits)
	}
	if events != 3 {
		t.Errorf("events behind = %d, want 3 (one uncounted plus 2)", events)
	}
}

// TestArchiveGapErrorNamesTheDatabase: a count that cannot be read returns
// zeros — not a partial sum that reads as a smaller gap — and an error naming
// the database and whether commits or events failed. No later database is read
// after the failure.
func TestArchiveGapErrorNamesTheDatabase(t *testing.T) {
	t.Parallel()

	t.Run("a commits count", func(t *testing.T) {
		t.Parallel()
		reader := &fakeArchiveGapReader{
			commits:   map[string]int64{"gt": 1},
			commitErr: map[string]error{"hq": errors.New("connection reset")},
		}
		commits, events, err := archiveGap(context.Background(), reader, daemon.ArchiveWatermarks{}, []string{"gt", "hq", "rig-a"})
		if err == nil {
			t.Fatal("an unreadable commits count reported no error")
		}
		if commits != 0 || events != 0 {
			t.Errorf("counts = (%d, %d), want zeros, not a partial sum", commits, events)
		}
		if !strings.Contains(err.Error(), "hq") || !strings.Contains(err.Error(), "commits") {
			t.Errorf("error = %q, want the database and the commits kind named", err)
		}
		if slices.Contains(reader.asked, "rig-a/commits") {
			t.Errorf("rig-a was read after hq failed: asked %v", reader.asked)
		}
	})

	t.Run("an events count", func(t *testing.T) {
		t.Parallel()
		reader := &fakeArchiveGapReader{
			commits:  map[string]int64{"gt": 1},
			eventErr: map[string]error{"rig-b": errors.New("dial timeout")},
		}
		commits, events, err := archiveGap(context.Background(), reader, daemon.ArchiveWatermarks{}, []string{"gt", "rig-b"})
		if err == nil {
			t.Fatal("an unreadable events count reported no error")
		}
		if commits != 0 || events != 0 {
			t.Errorf("counts = (%d, %d), want zeros, not a partial sum", commits, events)
		}
		if !strings.Contains(err.Error(), "rig-b") || !strings.Contains(err.Error(), "events") {
			t.Errorf("error = %q, want the database and the events kind named", err)
		}
	})
}

// writeCorruptMaintenanceState writes a maintenance_state.json the archive
// watermark loader cannot parse.
func writeCorruptMaintenanceState(t *testing.T, townRoot string) {
	t.Helper()
	dir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maintenance_state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
}

const archiveGapConfigOff = `{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00"}}}`

func archiveGapConfigWith(dir string) string {
	return fmt.Sprintf(
		`{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"03:00","archive_dir":%q}}}`,
		dir)
}

// TestArchiveGapSwitchedOffIsZero: with maintenance.archive_dir unset the
// archive is switched off, so the probe is (0, 0, nil) and reads nothing else —
// proven by a corrupt watermark file it never reaches. Once the directory is
// set, an unreadable watermark file is an error rather than a zero that reads
// as caught up.
func TestArchiveGapSwitchedOffIsZero(t *testing.T) {
	t.Parallel()

	t.Run("no archive directory is zero and reads no further", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, archiveGapConfigOff)
		writeCorruptMaintenanceState(t, townRoot)

		commits, events, err := newArchiveGapFunc(townRoot)(context.Background())
		if err != nil {
			t.Fatalf("archive gap with the archive switched off: %v", err)
		}
		if commits != 0 || events != 0 {
			t.Errorf("counts = (%d, %d), want (0, 0)", commits, events)
		}
	})

	t.Run("an unreadable watermark file is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, archiveGapConfigWith(t.TempDir()))
		writeCorruptMaintenanceState(t, townRoot)

		if _, _, err := newArchiveGapFunc(townRoot)(context.Background()); err == nil {
			t.Error("unreadable archive watermarks reported caught up")
		}
	})

	t.Run("an unreadable daemon config is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, "{not json")

		if _, _, err := newArchiveGapFunc(townRoot)(context.Background()); err == nil {
			t.Error("an unreadable daemon config reported caught up")
		}
	})

	t.Run("an unreadable database list is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		writeDaemonConfig(t, townRoot, archiveGapConfigWith(t.TempDir()))
		// A data directory that exists as a file cannot be listed.
		if err := os.WriteFile(filepath.Join(townRoot, ".dolt-data"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, _, err := newArchiveGapFunc(townRoot)(context.Background()); err == nil {
			t.Error("an unreadable database list reported caught up")
		}
	})
}

// writeDoltDatabase lays out a database directory the local listing accepts: a
// .dolt/noms/manifest under the town's Dolt data dir.
func writeDoltDatabase(t *testing.T, townRoot, name string) {
	t.Helper()
	dir := filepath.Join(townRoot, ".dolt-data", name, ".dolt", "noms")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestArchiveGapMissingDataDirIsAnError: with the archive on, a town whose
// Dolt data directory does not exist is an error. Listing it would give no
// databases, and an empty sum reads as caught up.
func TestArchiveGapMissingDataDirIsAnError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonConfig(t, townRoot, archiveGapConfigWith(t.TempDir()))

	commits, events, err := newArchiveGapFunc(townRoot)(context.Background())
	if err == nil {
		t.Fatalf("a missing data directory reported (%d, %d) with no error", commits, events)
	}
	if !strings.Contains(err.Error(), ".dolt-data") {
		t.Errorf("error = %q, want the data directory named", err)
	}
}

// TestArchiveGapSuccessPathCountsEveryDatabase: the probe's success path, run
// through the production closure with a fake pool. Each database contributes
// its commit count and its events count, so two databases give 2 and 2.
func TestArchiveGapSuccessPathCountsEveryDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeDaemonConfig(t, townRoot, archiveGapConfigWith(t.TempDir()))
	writeDoltDatabase(t, townRoot, "gt")
	writeDoltDatabase(t, townRoot, "rig_a")

	// The fake answers each count with one unarchived commit and an
	// uncounted events table, which the probe reads as one event behind.
	probe := newArchiveGapFuncWith(townRoot, archiveCountOpener(t, &archiveCountServer{}))
	commits, events, err := probe(context.Background())
	if err != nil {
		t.Fatalf("archive gap: %v", err)
	}
	if commits != 2 {
		t.Errorf("commits behind = %d, want 2 (one per database)", commits)
	}
	if events != 2 {
		t.Errorf("events behind = %d, want 2 (one per database)", events)
	}
}
