package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// These tests drive the gc entirely through fakes: the size probe, the
// dolt_gc call, the clock, the pause marker, the quiet-window probe and the
// escalation sink are all seams. Nothing here opens a SQL connection, touches
// a real .dolt directory or needs Docker. The daemon's town root is always a
// t.TempDir().

const mib = int64(1024 * 1024)

// gcTestNow is the fake clock's reading.
var gcTestNow = time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)

// --- the triggers -------------------------------------------------------------

func TestShouldGCDatabase(t *testing.T) {
	t.Parallel()
	now := gcTestNow
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	cases := []struct {
		name     string
		lastGC   time.Time
		oldGen   int64
		baseline int64
		want     bool
	}{
		{"never gc'd", time.Time{}, 10 * mib, 0, true},
		{"weekly: 7 days since last gc", days(7), 100 * mib, 100 * mib, true},
		{"weekly: in the window a day short of a week ahead", now.Add(-maintenanceGCWeekly), 100 * mib, 100 * mib, true},
		{"6 days, old-gen unchanged", days(6), 100 * mib, 100 * mib, false},
		{"2 days, old-gen grew 10%", days(2), 110 * mib, 100 * mib, false},
		{"2 days, old-gen grew exactly 20%", days(2), 120 * mib, 100 * mib, false},
		{"2 days, old-gen grew 25%", days(2), 125 * mib, 100 * mib, true},
		{"1 day, old-gen shrank", days(1), 80 * mib, 100 * mib, false},
		{"1 day, no old-gen before or now", days(1), 0, 0, false},
		{"1 day, old-gen appeared since an empty baseline", days(1), mib, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := shouldGCDatabase(now, tc.lastGC, tc.oldGen, tc.baseline)
			if got != tc.want {
				t.Errorf("shouldGCDatabase = %v (%s), want %v", got, reason, tc.want)
			}
			if reason == "" {
				t.Error("reason is empty; the log line needs it")
			}
		})
	}
}

// A host that cannot read the server's data dir (the pod) has no old-gen size,
// so growth is unknown and the week alone decides: a database a week past its
// last gc is due, one an hour past is not, and one never gc'd is due at once.
func TestShouldGCDatabaseWithoutOldGenSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		lastGC time.Time
		want   bool
	}{
		{"never gc'd", time.Time{}, true},
		{"weekly: a week since last gc", gcTestNow.Add(-7 * 24 * time.Hour), true},
		{"weekly: exactly the weekly interval", gcTestNow.Add(-maintenanceGCWeekly), true},
		{"a day short of a week", gcTestNow.Add(-6 * 24 * time.Hour), false},
		{"an hour ago", gcTestNow.Add(-time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := shouldGCDatabaseWithoutOldGenSize(gcTestNow, tc.lastGC)
			if got != tc.want {
				t.Errorf("shouldGCDatabaseWithoutOldGenSize = %v (%s), want %v", got, reason, tc.want)
			}
			if reason == "" {
				t.Error("reason is empty; the log line needs it")
			}
		})
	}
}

// --- state file --------------------------------------------------------------

func TestMaintenanceGCStateRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	st, err := loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load of a missing state file: %v", err)
	}
	if len(st.PostGCBytes) != 0 || len(st.LastGC) != 0 {
		t.Fatalf("missing state file yields %v / %v, want none", st.PostGCBytes, st.LastGC)
	}

	at := gcTestNow
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{631 * mib, 500 * mib}, gcMeasure{97 * mib, 90 * mib}, at); err != nil {
		t.Fatalf("record hq: %v", err)
	}
	if err := recordMaintenanceGCRun(town, "gt", gcMeasure{633 * mib, 300 * mib}, gcMeasure{480 * mib, 307 * mib}, at); err != nil {
		t.Fatalf("record gt: %v", err)
	}

	st, err = loadMaintenanceGCState(town)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCBytes["gt"] != 480*mib {
		t.Errorf("post_gc_bytes = %v, want hq=97MiB gt=480MiB", st.PostGCBytes)
	}
	if st.PostGCOldGenBytes["hq"] != 90*mib || st.PostGCOldGenBytes["gt"] != 307*mib {
		t.Errorf("post_gc_oldgen_bytes = %v, want hq=90MiB gt=307MiB", st.PostGCOldGenBytes)
	}
	if st.ReclaimedBytes["hq"] != 534*mib || st.ReclaimedBytes["gt"] != 153*mib {
		t.Errorf("reclaimed_bytes = %v, want hq=534MiB gt=153MiB", st.ReclaimedBytes)
	}
	if !st.LastGC["gt"].Equal(at) {
		t.Errorf("LastGC[gt] = %v, want %v", st.LastGC["gt"], at)
	}

	// The file lives in the daemon dir and nothing else is left behind (no
	// temp file from the atomic write).
	entries, err := os.ReadDir(filepath.Join(town, "daemon"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != maintenanceGCStateFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("daemon dir holds %v, want only %s", names, maintenanceGCStateFileName)
	}
}

func TestReadMaintenanceGC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		state         string // written to the state path; empty means no file
		dirAtPath     bool   // a directory at the state path, so the read fails
		wantLast      time.Time
		wantReclaimed int64
		wantMissing   bool
		wantErr       bool
	}{
		{
			name: "newest gc and the sum of reclaimed bytes",
			state: `{"last_gc":{"hq":"2026-10-01T02:00:00Z","gt":"2026-10-01T03:00:00Z"},` +
				`"reclaimed_bytes":{"hq":10,"gt":32}}`,
			wantLast:      time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC),
			wantReclaimed: 42,
		},
		{
			name:  "state file with no gc recorded",
			state: `{"last_gc":{}}`,
		},
		{
			name:    "an empty object is a read error, not no gc recorded",
			state:   `{}`,
			wantErr: true,
		},
		{
			name:        "no state file is missing",
			wantMissing: true,
		},
		{
			name:    "state file that does not parse is an error",
			state:   `{"last_gc":`,
			wantErr: true,
		},
		{
			name:      "a directory at the state path is a read error, not an empty state",
			dirAtPath: true,
			wantErr:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			town := t.TempDir()
			//testpolicy:allow no-exec-files — the target is the TempDir root: a directory's search bit, kept private under the gate's umask 000
			if err := os.Chmod(town, 0o700); err != nil {
				t.Fatal(err)
			}
			if c.state != "" || c.dirAtPath {
				path := maintenanceGCStatePath(town)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if c.dirAtPath {
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(c.state), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			last, reclaimed, err := ReadMaintenanceGC(town)
			switch {
			case c.wantMissing:
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("err = %v, want fs.ErrNotExist", err)
				}
				return
			case c.wantErr:
				if err == nil || errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("err = %v, want a read error other than fs.ErrNotExist", err)
				}
				return
			case err != nil:
				t.Fatalf("ReadMaintenanceGC: %v", err)
			}
			if !last.Equal(c.wantLast) {
				t.Errorf("last gc = %v, want %v", last, c.wantLast)
			}
			if reclaimed != c.wantReclaimed {
				t.Errorf("reclaimed bytes = %d, want %d", reclaimed, c.wantReclaimed)
			}
		})
	}
}

func TestMaintenanceGCStateCorruptIsAnErrorAndRepairable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := maintenanceGCStatePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMaintenanceGCState(town); err == nil {
		t.Fatal("corrupt state file loaded without error")
	}
	// A record after corruption replaces the file rather than failing forever.
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
		t.Fatalf("record over corrupt file: %v", err)
	}
	st, err := loadMaintenanceGCState(town)
	if err != nil || st.PostGCBytes["hq"] != mib {
		t.Fatalf("after repair: state=%v err=%v", st, err)
	}
}

// TestMaintenanceGCStateReplacementKeepsACopy: a state write over an unreadable
// file still lands (no writer wedges), keeps one copy of the old bytes beside
// the file, and stamps when and where, so the squash gate it lost is known to be
// lost; a second corruption keeps the first copy (gt-yvh1vv).
func TestMaintenanceGCStateReplacementKeepsACopy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := maintenanceGCStatePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	first, second := []byte("{first corrupt"), []byte("{second corrupt")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
		t.Fatalf("record over corrupt file: %v", err)
	}
	after := time.Now().UTC()
	st, err := loadMaintenanceGCState(town)
	if err != nil || st.PostGCBytes["hq"] != mib {
		t.Fatalf("after the replacement: state=%v err=%v", st, err)
	}
	if st.StateResetAt.Before(before) || st.StateResetAt.After(after) || st.StateResetCopy != path+squashStateCorruptSuffix {
		t.Errorf("the stamp = %v and copy %q, want a time in [%v, %v] and %q", st.StateResetAt, st.StateResetCopy, before, after, path+squashStateCorruptSuffix)
	}
	if got, err := os.ReadFile(path + squashStateCorruptSuffix); err != nil || !bytes.Equal(got, first) {
		t.Errorf("the kept copy = %q, %v, want the first corrupt bytes", got, err)
	}
	if err := os.WriteFile(path, second, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
		t.Fatalf("record over a second corrupt file: %v", err)
	}
	if got, err := os.ReadFile(path + squashStateCorruptSuffix); err != nil || !bytes.Equal(got, first) {
		t.Errorf("after a second corruption the copy = %q, %v, want the first copy to stand", got, err)
	}
}

// TestKeptStateCopyIsOwnerOnly: the copy of an unparseable state file is created
// 0600, beside a state file of that mode, whatever the daemon's umask allows (the
// gate runs at 0022, a bare shell at 0000: 0644 shows as 0644 under both); the
// first copy stands and keeps its mode (gt-wr0yww).
func TestKeptStateCopyIsOwnerOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := maintenanceGCStatePath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyPath, err := keepStateCopy(path)
	if err != nil {
		t.Fatalf("keepStateCopy: %v", err)
	}
	if fi, err := os.Stat(copyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the copy's mode = %v, err %v, want -rw-------", fi.Mode().Perm(), err)
	}
	if _, err := keepStateCopy(path); !errors.Is(err, errStateCopyStands) {
		t.Errorf("a second copy = %v, want the first to stand", err)
	}
}

// TestMaintenanceGCStateReplacementEdges: a copy that cannot be kept does not stop
// the record and the state says so; a fresh town sets no stamp; a readable file
// keeps the stamp it has across a write.
func TestMaintenanceGCStateReplacementEdges(t *testing.T) {
	t.Parallel()
	t.Run("a copy that cannot be kept", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := maintenanceGCStatePath(town)
		if err := os.MkdirAll(path+squashStateCorruptSuffix, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
			t.Fatalf("a copy that cannot be kept stopped the record: %v", err)
		}
		st, err := loadMaintenanceGCState(town)
		if err != nil || st.PostGCBytes["hq"] != mib || st.StateResetAt.IsZero() || !strings.Contains(st.StateResetCopy, "not kept") {
			t.Errorf("state=%+v err=%v, want the record, a stamp and a copy field saying it was not kept", st, err)
		}
	})
	t.Run("a fresh town sets no stamp", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
			t.Fatal(err)
		}
		if st, err := loadMaintenanceGCState(town); err != nil || !st.StateResetAt.IsZero() || st.StateResetCopy != "" {
			t.Errorf("state=%+v err=%v, want no stamp", st, err)
		}
	})
	t.Run("a readable file keeps its stamp", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		stamp := gcTestNow.Add(-time.Hour)
		if _, err := updateMaintenanceGCState(town, func(st *maintenanceGCState) { st.StateResetAt, st.StateResetCopy = stamp, "kept" }); err != nil {
			t.Fatal(err)
		}
		if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
			t.Fatal(err)
		}
		if st, err := loadMaintenanceGCState(town); err != nil || !st.StateResetAt.Equal(stamp) || st.StateResetCopy != "kept" {
			t.Errorf("state=%+v err=%v, want the stamp kept across a write", st, err)
		}
	})
}

// TestMaintenanceGCStateUpdateLeavesAFileItCannotRead is gt-tv0919's box: a
// write over a state file it cannot READ returns the read's error and leaves
// the path as it was, keeping no copy and stamping nothing, because a
// transient EACCES must not wipe the squash records and archive cursors; a file
// it can read but not parse is still replaced, with one copy kept and the stamp.
func TestMaintenanceGCStateUpdateLeavesAFileItCannotRead(t *testing.T) {
	t.Parallel()
	t.Run("a path it cannot read", func(t *testing.T) {
		t.Parallel()
		// A symlink to itself fails os.ReadFile with ELOOP for every uid, root
		// included (a mode-000 file is readable by root), and an update that
		// replaced it would succeed, so the row tells a refusal from a replacement.
		town := t.TempDir()
		path := maintenanceGCStatePath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, path); err != nil {
			t.Fatal(err)
		}

		err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow)
		if err == nil {
			t.Fatal("recordMaintenanceGCRun wrote over a state path it cannot read")
		}
		if errors.Is(err, errMaintenanceGCStateUnparseable) {
			t.Errorf("error = %v, a read failure must not carry the unparseable sentinel", err)
		}
		if _, statErr := os.Lstat(path + squashStateCorruptSuffix); !os.IsNotExist(statErr) {
			t.Errorf("a copy exists at %s, want none when the path was never read", path+squashStateCorruptSuffix)
		}
		if target, linkErr := os.Readlink(path); linkErr != nil || target != path {
			t.Errorf("the state path changed: link %q, err %v, want the self-link as it was", target, linkErr)
		}
	})
	t.Run("a file it cannot parse", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := maintenanceGCStatePath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		corrupt := []byte(`{"post_gc_bytes":`)
		if err := os.WriteFile(path, corrupt, 0o644); err != nil {
			t.Fatal(err)
		}
		before := time.Now().UTC()
		if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
			t.Fatalf("record over an unparseable file: %v", err)
		}
		st, err := loadMaintenanceGCState(town)
		if err != nil || st.PostGCBytes["hq"] != mib {
			t.Fatalf("after the replacement: state=%v err=%v", st, err)
		}
		if st.StateResetAt.Before(before) || st.StateResetCopy != path+squashStateCorruptSuffix {
			t.Errorf("the stamp = %v and copy %q, want a time at or after %v and %q", st.StateResetAt, st.StateResetCopy, before, path+squashStateCorruptSuffix)
		}
		if got, err := os.ReadFile(path + squashStateCorruptSuffix); err != nil || !bytes.Equal(got, corrupt) {
			t.Errorf("the kept copy = %q, %v, want the unparseable bytes", got, err)
		}
	})
	// The reader refuses these as it does an unparseable file (gt-u43s2r), so the
	// write replaces them too: one definition of unparseable, or {} wedges forever.
	for _, body := range []string{`{}`, `null`, `{"post_gc_bytes":{"hq":-5},"last_gc":{}}`, `{"last_gc":{},"squash":{"gt":{"last_run":"0001-01-01T00:00:00Z","outcome":"completed"}}}`} {
		t.Run("a file the reader refuses as unparseable: "+body, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := maintenanceGCStatePath(town)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := recordMaintenanceGCRun(town, "hq", gcMeasure{total: 2 * mib}, gcMeasure{total: mib}, gcTestNow); err != nil {
				t.Fatalf("record over %s: %v", body, err)
			}
			if got, err := os.ReadFile(path + squashStateCorruptSuffix); err != nil || string(got) != body {
				t.Errorf("the kept copy = %q, %v, want %q", got, err, body)
			}
		})
	}
}

// stateCopyFault is a copy file whose write or close fails.
type stateCopyFault struct{ writeErr, closeErr error }

func (f stateCopyFault) Write(p []byte) (int, error) { return len(p), f.writeErr }
func (f stateCopyFault) Close() error                { return f.closeErr }

// create leaves a partial copy at name, as a failing write would, and hands back f.
func (f stateCopyFault) create(name string) (io.WriteCloser, error) {
	return f, os.WriteFile(name, []byte("partial"), 0o644)
}

// TestKeepStateCopyFailures: every way the copy can fail is an error naming the
// copy's path, leaves no partial copy, and carries none of the file's bytes
// (gt-yvh1vv).
func TestKeepStateCopyFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		create func(string) (io.WriteCloser, error)
		noFile bool
	}{
		{"the state file cannot be read", nil, true},
		{"the copy cannot be created", func(string) (io.WriteCloser, error) { return nil, errors.New("no room") }, false},
		{"the write fails", stateCopyFault{writeErr: errors.New("disk full")}.create, false},
		{"the close fails", stateCopyFault{closeErr: errors.New("disk full")}.create, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), maintenanceGCStateFileName)
			if !tc.noFile {
				if err := os.WriteFile(path, []byte("{SECRETBYTES"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			create := tc.create
			if create == nil {
				create = func(string) (io.WriteCloser, error) { return nil, errors.New("not reached") }
			}
			copyPath, err := keepStateCopyWith(path, create)
			if err == nil || !strings.Contains(err.Error(), copyPath) || strings.Contains(err.Error(), "SECRETBYTES") {
				t.Fatalf("error = %v, want one naming %s and none of the file's bytes", err, copyPath)
			}
			if _, statErr := os.Stat(copyPath); statErr == nil {
				t.Errorf("a partial copy was left at %s", copyPath)
			}
		})
	}
}

// TestMaintenanceGCStateRefusesAFileThatIsNotTheDaemonsState: a file that parses
// but holds nothing the daemon writes ({} or null, a size below zero, no last_gc)
// is refused like an unparseable one, so the squash check does not read it as 'no
// run recorded'; the file the daemon writes for 'none yet' (every always-written
// key, empty maps, no squash key), an unknown extra key and an absent file are
// not refused (gt-u43s2r).
func TestMaintenanceGCStateRefusesAFileThatIsNotTheDaemonsState(t *testing.T) {
	t.Parallel()
	fresh, err := json.Marshal(maintenanceGCState{
		PostGCBytes: map[string]int64{}, PostGCOldGenBytes: map[string]int64{}, ReclaimedBytes: map[string]int64{}, LastGC: map[string]time.Time{},
		StaleGCAlerts: map[string]bool{}, ArchiveWatermarks: map[string]ArchiveWatermark{}, ArchiveEventsWatermarks: map[string]EventsArchiveWatermark{}, Squash: map[string]squashRunRecord{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		content string
		refused bool
	}{
		{"an empty object", `{}`, true},
		{"null", `null`, true},
		{"a size below zero", `{"post_gc_bytes":{"hq":-5},"last_gc":{}}`, true},
		{"no last_gc", `{"post_gc_bytes":{}}`, true},
		{"last_gc null", `{"last_gc":null}`, true},
		{"a squash record that is null", `{"last_gc":{},"squash":{"db":null}}`, true},
		{"a squash record that is an empty object", `{"last_gc":{},"squash":{"db":{}}}`, true},
		{"a squash record whose last_run is null", `{"last_gc":{},"squash":{"db":{"last_run":null,"outcome":"ok"}}}`, true},
		{"a squash record whose last_run is the zero time", `{"last_gc":{},"squash":{"db":{"last_run":"0001-01-01T00:00:00Z"}}}`, true},
		{"the zero time written with a +01:00 offset", `{"last_gc":{},"squash":{"db":{"last_run":"0001-01-01T00:00:00+01:00"}}}`, true},
		{"the zero time written with a -05:00 offset", `{"last_gc":{},"squash":{"db":{"last_run":"0001-01-01T00:00:00-05:00"}}}`, true},
		{"a last_run in year 1", `{"last_gc":{},"squash":{"db":{"last_run":"0001-06-01T12:00:00Z"}}}`, true},
		{"a last_run in 1999", `{"last_gc":{},"squash":{"db":{"last_run":"1999-12-31T23:59:59Z"}}}`, true},
		{"a last_run on the floor day", `{"last_gc":{},"squash":{"db":{"last_run":"2000-01-01T00:00:00Z"}}}`, false},
		{"a squash record with a real last_run", `{"last_gc":{},"squash":{"db":{"last_run":"2026-11-21T12:00:00Z","outcome":"completed"}}}`, false},
		{"a null squash map", `{"last_gc":{},"squash":null}`, false},
		{"the state the daemon writes with no database yet", string(fresh), false},
		{"an unknown extra key", `{"post_gc_bytes":{},"last_gc":{},"future_key":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := maintenanceGCStatePath(town)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadMaintenanceGCState(town)
			if (err != nil) != tc.refused {
				t.Fatalf("loading %s = %v, want refused=%v", tc.content, err, tc.refused)
			}
			// A refused squash record names the file and the database, never the value.
			if err != nil && strings.Contains(tc.content, `"squash":{"db"`) {
				msg := err.Error()
				if !strings.Contains(msg, maintenanceGCStateFileName) || !strings.Contains(msg, "db") || strings.Contains(msg, "0001") || strings.Contains(msg, "1999") {
					t.Errorf("refusal %q must name the file and the database and carry no value", msg)
				}
			}
		})
	}
	if _, err := readMaintenanceGCState(t.TempDir()); !errors.Is(err, errMaintenanceGCStateAbsent) {
		t.Errorf("an absent file read = %v, want errMaintenanceGCStateAbsent", err)
	}
}

// --- size probe --------------------------------------------------------------

func TestMaintenanceMeasure(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	noms := filepath.Join(dataDir, "hq", ".dolt", "noms")
	if err := os.MkdirAll(filepath.Join(noms, "oldgen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noms, "a"), make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noms, "oldgen", "b"), make([]byte, 234), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "fresh", ".dolt", "noms"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := maintenanceMeasure(dataDir, "hq")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if got.total != 1234 || got.oldGen != 234 {
		t.Errorf("measure = %+v, want total 1234 (oldgen counts) and oldGen 234", got)
	}

	// A database that has never had a gc --full has no oldgen directory.
	if got, err := maintenanceMeasure(dataDir, "fresh"); err != nil || got.oldGen != 0 {
		t.Errorf("measure of a database without oldgen = %+v, %v; want oldGen 0, no error", got, err)
	}
	if _, err := maintenanceMeasure(dataDir, "missing"); err == nil {
		t.Error("measure of a missing database returned no error")
	}
	// A name that escapes the data dir is refused rather than walked.
	if _, err := maintenanceMeasure(dataDir, "../etc"); err == nil {
		t.Error("measure of a path-escaping database name returned no error")
	}
}

// --- the cycle ----------------------------------------------------------------

// gcFakes records what a gc cycle did through its seams.
type gcFakes struct {
	sizes       map[string]gcMeasure // current sizes per database
	shrinkTo    map[string]gcMeasure // sizes after a successful gc
	gcErr       map[string]error
	gcCalls     []string
	quietCalls  int
	quietUntil  int // quiet for this many calls, then busy; -1 = always quiet
	escalations []string

	// The nightly backup: backupRoot is a temp dir, backupCalls the
	// databases copied, backupErr a failure per database.
	backupRoot  string
	backupCalls []string
	backupErr   map[string]error

	// serverDBs is what an external server's SHOW DATABASES returns (serverErr
	// fails it); serverCalls counts the listings.
	serverDBs   []string
	serverErr   error
	serverCalls int

	// The pause marker: events records "write <db>", "gc <db>" and
	// "remove" in order; marker is the one on "disk".
	events       []string
	marker       *doltpause.Marker
	written      []doltpause.Marker
	foreignPause *doltpause.Marker
	writeErr     error
	removeErr    error
}

func withGCFakes(t *testing.T, d *Daemon) *gcFakes {
	t.Helper()
	f := &gcFakes{sizes: map[string]gcMeasure{}, shrinkTo: map[string]gcMeasure{}, gcErr: map[string]error{}, quietUntil: -1,
		backupRoot: t.TempDir(), backupErr: map[string]error{}}

	// Discovery returns the databases the fake sizes know about.
	d.maint.gcDatabases = func(string) ([]string, error) {
		var dbs []string
		for db := range f.sizes {
			dbs = append(dbs, db)
		}
		sort.Strings(dbs)
		return dbs, nil
	}
	d.maint.gcExternal = func(*Daemon) (bool, string) { return false, "" }
	d.maint.serverDatabases = func(*Daemon) ([]string, error) {
		f.serverCalls++
		return f.serverDBs, f.serverErr
	}
	d.maint.now = func() time.Time { return gcTestNow }
	d.maint.pauseCurrent = func(string, time.Time) *doltpause.Marker { return f.foreignPause }
	d.maint.pauseWrite = func(_ string, m doltpause.Marker) error {
		if f.writeErr != nil {
			return f.writeErr
		}
		f.events = append(f.events, "write "+strings.TrimPrefix(m.Reason, "dolt_gc --full on "))
		f.marker = &m
		f.written = append(f.written, m)
		return nil
	}
	d.maint.pauseRemove = func(string) (bool, error) {
		f.events = append(f.events, "remove")
		if f.removeErr != nil {
			return false, f.removeErr
		}
		had := f.marker != nil
		f.marker = nil
		return had, nil
	}

	d.maint.measure = func(_ string, db string) (gcMeasure, error) {
		s, ok := f.sizes[db]
		if !ok {
			return gcMeasure{}, os.ErrNotExist
		}
		return s, nil
	}
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		f.events = append(f.events, "gc "+db)
		if f.marker == nil {
			t.Errorf("gc of %s ran with no pause marker written", db)
		}
		if err := f.gcErr[db]; err != nil {
			return err
		}
		if s, ok := f.shrinkTo[db]; ok {
			f.sizes[db] = s
		}
		return nil
	}
	d.maint.backupRoot = func() (string, error) { return f.backupRoot, nil }
	d.maint.backupExec = func(_ context.Context, _ *Daemon, db, dest string) error {
		f.backupCalls = append(f.backupCalls, db)
		f.events = append(f.events, "backup "+db)
		if f.marker == nil {
			t.Errorf("backup of %s ran with no pause marker written", db)
		}
		if err := f.backupErr[db]; err != nil {
			return err
		}
		// What the server's sync-url leaves behind.
		return os.MkdirAll(dest, 0o755)
	}
	d.maint.quiet = func(*Daemon) (bool, string) {
		f.quietCalls++
		if f.quietUntil >= 0 && f.quietCalls > f.quietUntil {
			return false, "refinery holds gate slot 0"
		}
		return true, ""
	}
	// The demand line's polecat count: a file read over the town's temp root
	// in production, stubbed here so a case can set the count.
	d.maint.workingPolecats = func(*Daemon) ([]string, error) { return nil, nil }
	d.maint.escalate = func(_ *Daemon, source, message string) {
		f.escalations = append(f.escalations, source+"|"+message)
	}
	// Run the dispatched cycle inline so the test observes its result.
	d.maint.dispatch = func(fn func()) { fn() }

	return f
}

func gcTestDaemon(t *testing.T) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	// Escalations go to a recorder, never to the gt CLI: a maintenance reading
	// that alerts (the stale-gc check, maintenance_gc_stale.go) must not spawn
	// a real gt from the unit tier (docs/testing.md, "The rules").
	return &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&buf, "", 0),
		notifier: notifyfake.New(),
	}, &buf
}

// recordRun seeds db's last patrol gc: daysAgo before the fake clock, with
// oldGen as its post-gc old-gen size.
func recordRun(t *testing.T, d *Daemon, db string, daysAgo int, oldGen int64) {
	t.Helper()
	at := gcTestNow.Add(-time.Duration(daysAgo) * 24 * time.Hour)
	if err := recordMaintenanceGCRun(d.config.TownRoot, db, gcMeasure{}, gcMeasure{total: oldGen, oldGen: oldGen}, at); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceGCCycleCollectsDueLargestFirstUnderPause(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"gt": {633 * mib, 300 * mib}, "hq": {631 * mib, 500 * mib}}
	f.shrinkTo = map[string]gcMeasure{"gt": {480 * mib, 307 * mib}, "hq": {97 * mib, 90 * mib}}

	out := d.maintenanceGCCycle([]string{"hq", "gt"}, "/unused", true)

	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	// Largest first, each gc between its own marker write and remove.
	want := "write gt,gc gt,remove,write hq,gc hq,remove"
	if got := strings.Join(f.events, ","); got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
	if f.marker != nil {
		t.Errorf("pause marker left behind: %+v", f.marker)
	}
	for _, m := range f.written {
		if m.Actor != maintenancePauseActor {
			t.Errorf("marker actor = %q, want %q", m.Actor, maintenancePauseActor)
		}
		if !m.Since.Equal(gcTestNow) || !m.Until.Equal(gcTestNow.Add(maintenanceGCTimeout+maintenancePauseSlack)) {
			t.Errorf("marker since/until = %v/%v, want now and now+gc timeout+slack", m.Since, m.Until)
		}
		if m.Until.Sub(m.Since) > doltpause.MaxDuration {
			t.Errorf("marker runs %v, over the doltpause cap", m.Until.Sub(m.Since))
		}
	}
	if len(f.escalations) != 0 {
		t.Errorf("successful gc escalated: %v", f.escalations)
	}
	if f.quietCalls != 2 {
		t.Errorf("quiet probe ran %d time(s), want 2 (once before each gc)", f.quietCalls)
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if st.PostGCBytes["hq"] != 97*mib || st.PostGCOldGenBytes["gt"] != 307*mib || st.ReclaimedBytes["hq"] != 534*mib {
		t.Errorf("state = %+v, want hq post 97MiB reclaimed 534MiB, gt old-gen 307MiB", st)
	}
	if !st.LastGC["gt"].Equal(gcTestNow) {
		t.Errorf("LastGC[gt] = %v, want the fake clock", st.LastGC["gt"])
	}
	for _, want := range []string{"gc hq: 631.0MiB -> 97.0MiB (old-gen 500.0MiB -> 90.0MiB)", "no patrol gc recorded yet"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log missing %q:\n%s", want, logs.String())
		}
	}
}

// Equal sizes go by name, whatever order discovery handed them over in.
func TestMaintenanceGCCycleLargestFirstTiesByName(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{
		"hq": {700 * mib, 0},
		"gt": {700 * mib, 0},
		"om": {700 * mib, 0},
		"sl": {100 * mib, 0},
	}

	d.maintenanceGCCycle([]string{"sl", "om", "hq", "gt"}, "/unused", true)

	if got := strings.Join(f.gcCalls, ","); got != "gt,hq,om,sl" {
		t.Errorf("gc calls = %s, want gt,hq,om,sl — ties by name, then the smaller database last", got)
	}
}

// candidateNames renders the pending databases of a deferral, in order.
func candidateNames(cs []gcCandidate) string {
	var names []string
	for _, c := range cs {
		names = append(names, c.name)
	}
	return strings.Join(names, ",")
}

func TestMaintenanceGCCycleWeeklyAndOldGenTriggers(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "weekly", 7, 100*mib) // a week old, old-gen flat
	recordRun(t, d, "grown", 2, 100*mib)  // two days old, old-gen +30%
	recordRun(t, d, "steady", 2, 100*mib) // two days old, old-gen +5%
	f.sizes = map[string]gcMeasure{
		"weekly": {300 * mib, 100 * mib},
		"grown":  {200 * mib, 130 * mib},
		"steady": {150 * mib, 105 * mib},
	}

	if out := d.maintenanceGCCycle([]string{"grown", "steady", "weekly"}, "/unused", true); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "weekly,grown" {
		t.Errorf("gc calls = %s, want weekly,grown largest first (steady is neither a week old nor >20%% grown)", got)
	}
}

func TestMaintenanceGCCycleNothingDueSkipsQuietProbeAndPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "om", 1, 42*mib)
	f.sizes = map[string]gcMeasure{"om": {50 * mib, 42 * mib}}

	if out := d.maintenanceGCCycle([]string{"om"}, "/unused", true); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if len(f.gcCalls) != 0 || f.quietCalls != 0 || len(f.events) != 0 {
		t.Errorf("nothing due, yet gc=%v quietCalls=%d events=%v", f.gcCalls, f.quietCalls, f.events)
	}
}

func TestMaintenanceGCCycleDefersWhenNotQuiet(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}, "om": {700 * mib, 0}}
	f.quietUntil = 1 // quiet before om, busy before gt

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused", true)

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "om" {
		t.Errorf("gc calls = %s, want om only — the run must stop when the town gets busy", got)
	}
	if got := candidateNames(out.pending); got != "gt,hq" {
		t.Errorf("pending = %s, want gt,hq — the ones the deferral left, largest first", got)
	}
	if len(f.escalations) != 0 {
		t.Errorf("a deferral escalated: %v", f.escalations)
	}
	if !strings.Contains(logs.String(), "refinery holds gate slot 0") {
		t.Errorf("deferral log does not say why:\n%s", logs.String())
	}
}

// TestMaintenanceGCStillDefersWhileALandingPassIsInFlight: the cache trim's own
// guard did not loosen the Dolt window's. A due gc still defers while a landing
// pass is in flight, and the reason still names the pass (gt-gg9hv.48).
func TestMaintenanceGCStillDefersWhileALandingPassIsInFlight(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	// The real guard over the fakes' seams, with a landing pass in flight.
	d.maint.quiet = (*Daemon).maintenanceQuiet
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.landingStates.setBead("gastown", "dv-x", gcTestNow)

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", out.outcome)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v, want none while a landing pass is in flight", f.gcCalls)
	}
	if !strings.Contains(out.reason, "daemon has work in flight (landing pass dv-x)") {
		t.Errorf("deferral reason %q does not name the landing pass", out.reason)
	}
}

// Someone else's pause (the nightly backup, an operator) defers the gc and is
// left exactly as it was: the gc neither overwrites nor removes it.
func TestMaintenanceGCCycleDefersToAnotherActorsPause(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.foreignPause = &doltpause.Marker{Actor: "operator", Reason: "restore drill", Until: gcTestNow.Add(time.Hour)}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeDeferred {
		t.Fatalf("outcome = %v, want deferred", out.outcome)
	}
	if len(f.gcCalls) != 0 || len(f.events) != 0 {
		t.Errorf("gc touched a pause it does not own: gc=%v events=%v", f.gcCalls, f.events)
	}
	if !strings.Contains(out.reason, "Dolt paused by operator") {
		t.Errorf("deferral reason %q does not name the pause", out.reason)
	}
}

func TestMaintenanceGCCycleStopsEscalatesAndUnpausesOnError(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}, "om": {700 * mib, 0}}
	f.gcErr["gt"] = errors.New("Error 1105: gc failed: boom")

	out := d.maintenanceGCCycle([]string{"hq", "gt", "om"}, "/unused", true)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "om,gt" {
		t.Errorf("gc calls = %s, want om,gt — a failure must stop the run", got)
	}
	if f.marker != nil || f.events[len(f.events)-1] != "remove" {
		t.Errorf("failed gc left the pause marker: events=%v", f.events)
	}
	if len(f.escalations) != 1 {
		t.Fatalf("escalations = %d, want exactly 1: %v", len(f.escalations), f.escalations)
	}
	for _, want := range []string{"scheduled_maintenance|", "gt", "boom", "600.0MiB"} {
		if !strings.Contains(f.escalations[0], want) {
			t.Errorf("escalation missing %q:\n%s", want, f.escalations[0])
		}
	}
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if _, ok := st.LastGC["gt"]; ok {
		t.Error("a failed gc recorded a run")
	}
	if _, ok := st.LastGC["om"]; !ok {
		t.Error("om's successful gc was not recorded")
	}
	if _, ok := st.LastGC["hq"]; ok {
		t.Error("hq ran after a failure — the run must stop at the failed database")
	}
}

// A failure on the first database stops the run before any other is touched.
func TestMaintenanceGCCycleFirstDatabaseFailureStopsTheRest(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}, "om": {700 * mib, 0}}
	f.gcErr["om"] = errors.New("Error 1105: gc failed: boom")

	out := d.maintenanceGCCycle([]string{"hq", "om", "gt"}, "/unused", true)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "om" {
		t.Errorf("gc calls = %s, want om only — the largest database failed first and nothing followed it", got)
	}
	if len(f.escalations) != 1 {
		t.Errorf("escalations = %d, want exactly 1: %v", len(f.escalations), f.escalations)
	}
	st, _ := loadMaintenanceGCState(d.config.TownRoot)
	if len(st.LastGC) != 0 {
		t.Errorf("a failed first gc recorded runs: %+v", st.LastGC)
	}
}

// No marker, no gc: a gc the clients were not told about is the outage the
// marker exists to prevent.
func TestMaintenanceGCCycleRefusesGCWhenPauseCannotBeWritten(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.writeErr = errors.New("read-only file system")

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran without a pause marker: %v", f.gcCalls)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "pause marker") {
		t.Errorf("escalations = %v, want one naming the pause marker", f.escalations)
	}
}

// A marker the daemon cannot remove is logged and left to lapse at its until,
// which the marker sets within the gc bound: the gc itself still counts.
func TestMaintenanceGCCycleUnremovablePauseLapses(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	f.removeErr = errors.New("permission denied")

	if out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if !strings.Contains(logs.String(), "cannot remove pause marker") {
		t.Errorf("log does not report the stuck marker:\n%s", logs.String())
	}
	if f.marker == nil || f.marker.Active(gcTestNow.Add(maintenanceGCTimeout+maintenancePauseSlack)) {
		t.Errorf("stuck marker %+v must lapse by the gc timeout plus slack", f.marker)
	}
}

func TestMaintenanceGCCycleSizeErrorSkipsDatabase(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}} // "ghost" has no directory

	out := d.maintenanceGCCycle([]string{"ghost", "hq"}, "/unused", true)
	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
}

// A pod's cycle has no data dir on this host, so it reads none: the week alone
// decides, name order replaces largest-first, and the run records its time
// with no sizes, leaving the old-gen baselines unknown rather than zero
// (gt-lc16x3).
func TestMaintenanceGCCycleWithoutDataDirRunsWeekly(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	d.maint.measure = func(string, string) (gcMeasure, error) {
		t.Error("measured a data dir the host does not have")
		return gcMeasure{}, os.ErrNotExist
	}
	recordRun(t, d, "weekly", 7, 100*mib)
	recordRun(t, d, "fresh", 1, 100*mib)
	// "never" has no recorded gc: due at once.

	out := d.maintenanceGCCycle([]string{"fresh", "weekly", "never"}, "/not/on/this/host", false)
	if out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	if got := strings.Join(f.gcCalls, ","); got != "never,weekly" {
		t.Errorf("gc calls = %s, want never,weekly — the week alone, name order with no size", got)
	}
	if !strings.Contains(logs.String(), "size not readable from this host") {
		t.Errorf("log does not say the size is unreadable:\n%s", logs.String())
	}

	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastGC["weekly"].Equal(gcTestNow) || !st.LastGC["never"].Equal(gcTestNow) {
		t.Errorf("LastGC = %v, want the fake clock for weekly and never", st.LastGC)
	}
	// The seeded baselines are the values right after the database's last gc;
	// an unmeasured run must leave them alone rather than write zeros.
	if got := st.PostGCBytes["weekly"]; got != 100*mib {
		t.Errorf("post_gc_bytes[weekly] = %d, want the seeded 100MiB", got)
	}
	if got := st.PostGCOldGenBytes["weekly"]; got != 100*mib {
		t.Errorf("post_gc_oldgen_bytes[weekly] = %d, want the seeded 100MiB", got)
	}
	if _, ok := st.PostGCBytes["never"]; ok {
		t.Error("recorded a size for a database this host never measured")
	}
}

// --- wiring through runScheduledMaintenance ---------------------------------

// gcModeConfig is a config whose window is open at the fake clock's reading.
// The nightly town-state backup is off by an explicit empty string: these tests
// are about the Dolt backup, gc and the alert under test, and a section with
// the key left out now alerts (gt-l9qnva), which would add a second escalation
// to every tick they observe.
func gcModeConfig() *DaemonPatrolConfig {
	window := gcTestNow.Truncate(time.Hour).Format("15:04")
	return &DaemonPatrolConfig{
		Type: "daemon-patrol-config", Version: 1,
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:             true,
				Window:              window,
				TownStateBackupRoot: nightlyRoot(""),
			},
		},
	}
}

func TestScheduledMaintenanceRunsGCAndMarksRun(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 500 * mib}}
	f.shrinkTo = map[string]gcMeasure{"hq": {97 * mib, 90 * mib}}
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
	if d.maintenanceGCRunning.Load() {
		t.Error("gc cycle still marked running after it returned")
	}

	// The completion is folded into lastMaintenanceRun, so a second tick in
	// the same window does nothing even though old-gen grew.
	f.sizes["hq"] = gcMeasure{900 * mib, 800 * mib}
	d.runScheduledMaintenance()
	if len(f.gcCalls) != 1 {
		t.Errorf("second tick in the same window ran gc again: %v", f.gcCalls)
	}
}

// A live daemon.json still carries the retired mode keys: they decode, change
// nothing, and are named in the log.
func TestScheduledMaintenanceIgnoresAndNamesRetiredKeys(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	recordRun(t, d, "hq", 1, 100*mib)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 105 * mib}}
	cfg := gcModeConfig()
	threshold, minBytes, ratio := 1, int64(1), 1.0
	sm := cfg.Patrols.ScheduledMaintenance
	sm.Interval, sm.Threshold, sm.Mode, sm.GCMinBytes, sm.GCGrowthRatio = "daily", &threshold, "flatten", &minBytes, &ratio
	d.patrolConfig = cfg

	d.runScheduledMaintenance()

	if len(f.gcCalls) != 0 {
		t.Errorf("retired keys changed the schedule: gc ran on %v", f.gcCalls)
	}
	if !strings.Contains(logs.String(), "deprecated config ignored — patrols.scheduled_maintenance.{interval,threshold,mode,gc_min_bytes,gc_growth_ratio}") {
		t.Errorf("log does not name the retired keys:\n%s", logs.String())
	}
}

func TestScheduledMaintenanceDeferralRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.quietUntil = 0 // busy
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Fatalf("gc ran while the town was busy: %v", f.gcCalls)
	}

	f.quietUntil = -1 // town goes quiet
	d.runScheduledMaintenance()
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("after a deferral the next tick ran gc on %q, want hq", got)
	}
}

func TestScheduledMaintenanceFailureDoesNotRetryInWindow(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	f.gcErr["hq"] = errors.New("boom")
	d.patrolConfig = gcModeConfig()

	d.runScheduledMaintenance()
	d.runScheduledMaintenance()

	if len(f.gcCalls) != 1 || len(f.escalations) != 1 {
		t.Errorf("after a failure: gc calls %v, escalations %d — want one of each per window",
			f.gcCalls, len(f.escalations))
	}
}

func TestScheduledMaintenanceSkipsWhileCycleRunning(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()
	d.maintenanceGCRunning.Store(true)

	d.runScheduledMaintenance()
	if len(f.gcCalls) != 0 {
		t.Errorf("a second cycle started while one was running: %v", f.gcCalls)
	}
}

// --- the gc's guard ----------------------------------------------------------

// The gc waits for the hard holds only (gt-mdejo): a landing pass, a scheduled
// slings cycle, a held install lock, a held container-gate slot. Soft work and
// a working polecat do not defer it — every case here runs with a polecat
// working, and the ones marked soft run beside the daemon's own ticks too.
func TestMaintenanceQuiet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		set    func(t *testing.T, d *Daemon)
		slots  func(string) ([]string, error)
		want   bool
		reason string
	}{
		{"all clear", func(*testing.T, *Daemon) {}, nil, true, ""},
		{"soft work: plugin run, compactor, dispatch, scan", func(_ *testing.T, d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
			d.compactorDogMu.Lock()
			d.compactorDogRunning = true
			d.compactorDogMu.Unlock()
			d.specDispatchRunning.Store(true)
			d.patrolScanRunning.Store(true)
		}, nil, true, ""},
		{"gate slot held", func(*testing.T, *Daemon) {},
			func(string) ([]string, error) { return []string{"gastown/refinery"}, nil }, false, "slot held by gastown/refinery"},
		{"slot probe fails", func(*testing.T, *Daemon) {},
			func(string) ([]string, error) { return nil, errors.New("flock: permission denied") }, false, "slot"},
		{"landing pass in flight", func(_ *testing.T, d *Daemon) {
			d.landingStates.setBead("gastown", "gt-a", gcTestNow)
		}, nil, false, "daemon has work in flight (landing pass gt-a)"},
		{"scheduled slings cycle", func(_ *testing.T, d *Daemon) {
			d.scheduledSlingsRunning.Store(true)
		}, nil, false, "daemon has work in flight (a scheduled slings cycle)"},
		{"install lock held", func(t *testing.T, d *Daemon) {
			holdInstallLock(t, writeInstallLock(t, d))
		}, nil, false, "daemon has work in flight (the install lock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := idleTestDaemon(t)
			d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
			if tc.slots != nil {
				d.maint.slotHolders = tc.slots
			}
			// The guard must not read a working polecat, so the seam answers
			// with one in every case: a quiet verdict here is a verdict reached
			// without asking.
			d.maint.workingPolecats = func(*Daemon) ([]string, error) { return []string{"gastown/opal"}, nil }
			tc.set(t, d)
			quiet, reason := d.maintenanceQuiet()
			if quiet != tc.want {
				t.Fatalf("maintenanceQuiet() = %v (%s), want %v", quiet, reason, tc.want)
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", reason, tc.reason)
			}
		})
	}
}

// The gc cycle holds off a daemon upgrade-restart, but must not block its own
// quiet probe (which would make every run defer forever).
func TestMaintenanceGCRunningBlocksUpgradeButNotItsOwnQuietProbe(t *testing.T) {
	t.Parallel()
	d := idleTestDaemon(t)
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.maintenanceGCRunning.Store(true)
	if d.isIdleForUpgrade() {
		t.Error("isIdleForUpgrade() = true while a gc cycle runs")
	}
	if quiet, reason := d.maintenanceQuiet(); !quiet {
		t.Errorf("maintenanceQuiet() = false (%s) — the cycle is blocking itself", reason)
	}
	// gt-oucgn: the hold is the cycle, not the flag; a finished cycle leaves
	// the daemon idle again.
	d.maintenanceGCRunning.Store(false)
	if !d.isIdleForUpgrade() {
		t.Error("isIdleForUpgrade() = false after the gc cycle finished")
	}
}

// writeTestRigsJSON registers rigs in <town>/mayor/rigs.json.
func writeTestRigsJSON(t *testing.T, town string, rigNames ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := map[string]any{}
	for _, r := range rigNames {
		entries[r] = map[string]any{}
	}
	data, _ := json.Marshal(map[string]any{"rigs": entries})
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The cycle runs in a town that is only soft-busy: two polecats working with a
// plugin run and the compactor dog in flight no longer defer it, and the run's
// own line says what it ran beside (gt-mdejo).
func TestMaintenanceGCRunsBesideSoftWorkAndWorkingPolecats(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()
	// The real guard over the seams the fakes set.
	d.maint.quiet = (*Daemon).maintenanceQuiet
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.maint.workingPolecats = func(*Daemon) ([]string, error) {
		return []string{"gastown/dementus", "gastown/furiosa"}, nil
	}
	d.scripts = newScriptRunner()
	d.scripts.tryStart("rebuild-gt")
	d.compactorDogMu.Lock()
	d.compactorDogRunning = true
	d.compactorDogMu.Unlock()

	d.runScheduledMaintenance()

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Fatalf("gc calls = %q, want hq: soft work and working polecats must not defer it", got)
	}
	want := "gc hq: 2 polecats working: gastown/dementus, gastown/furiosa; " +
		"soft work in flight: a plugin run rebuild-gt, the compactor dog cycle; CALL dolt_gc('--full')"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want it to carry %q", logs.String(), want)
	}
}

// A polecat listing the daemon cannot read leaves the count unknown, and only
// the count: the gc still runs (gt-mdejo).
func TestMaintenanceGCDemandCountIsNotAGate(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}
	d.patrolConfig = gcModeConfig()
	d.maint.quiet = (*Daemon).maintenanceQuiet
	d.maint.slotHolders = func(string) ([]string, error) { return nil, nil }
	d.maint.workingPolecats = func(*Daemon) ([]string, error) {
		return nil, errors.New("rig gastown: permission denied")
	}

	d.runScheduledMaintenance()

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Fatalf("gc calls = %q, want hq: an unreadable polecat listing must not defer", got)
	}
	if want := "cannot count working polecats: rig gastown: permission denied"; !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want it to carry %q", logs.String(), want)
	}
}

func TestWorkingPolecatsFromHeartbeats(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := "testrig"
	writeTestRigsJSON(t, town, rig)
	for _, p := range []string{"fresh", "stale", "idle", "nobeat"} {
		if err := os.MkdirAll(filepath.Join(town, rig, "polecats", p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sess := func(p string) string { return session.PolecatSessionName(session.DefaultPrefix, p) }
	polecat.TouchSessionHeartbeatWithState(town, sess("fresh"), polecat.HeartbeatWorking, "", "")
	polecat.TouchSessionHeartbeatWithState(town, sess("idle"), polecat.HeartbeatIdle, "", "")
	stale := polecat.SessionHeartbeat{Timestamp: time.Now().Add(-2 * maintenancePolecatFreshness), State: polecat.HeartbeatWorking}
	staleData, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(town, ".runtime", "heartbeats", sess("stale")+".json"), staleData, 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	got, err := d.workingPolecats()
	if err != nil {
		t.Fatalf("workingPolecats() error: %v", err)
	}
	if strings.Join(got, ",") != rig+"/fresh" {
		t.Errorf("workingPolecats() = %v, want [%s/fresh]", got, rig)
	}
}

// TestDirSizeIgnoresDoltJournalFiles pins the size measure the gc trigger and
// the compactor dog both read: Dolt's journal files are not part of the store.
//
// The journal is the append-only buffer of writes already in the table files,
// replaced wholesale by a gc, and a fixed-capacity mapping besides — so it
// tracks how long it has been since the last truncation, not what a database
// holds. Counting it read hq at 1,708.9 MiB against 235 MB on disk (gt-7yejc).
func TestDirSizeIgnoresDoltJournalFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	noms := filepath.Join(root, ".dolt", "noms")
	for _, dir := range []string{noms, filepath.Join(noms, "oldgen")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFile := func(path string, size int) {
		t.Helper()
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeFile(filepath.Join(noms, "4vrlfm9eipve1in8d57vm4t5s2po57jq.darc"), 700)
	// A live journal far larger than the store it belongs to.
	writeFile(filepath.Join(noms, doltJournalFileName), int(4*mib))
	writeFile(filepath.Join(noms, doltJournalIndexFileName), 128)
	writeFile(filepath.Join(noms, "oldgen", "aaaa"), 300)

	got, err := dirSize(root)
	if err != nil {
		t.Fatalf("dirSize: %v", err)
	}
	if want := int64(1000); got != want {
		t.Errorf("dirSize = %d, want %d (table file plus old gen, journal files out)", got, want)
	}

	// The old-gen measure walks the same helper: a journal there would be as
	// foreign as one in the store.
	got, err = dirSize(filepath.Join(noms, "oldgen"))
	if err != nil {
		t.Fatalf("dirSize(oldgen): %v", err)
	}
	if want := int64(300); got != want {
		t.Errorf("dirSize(oldgen) = %d, want %d", got, want)
	}
}

// maintenanceDSN, which the gc and backup calls share, authenticates with the
// town's configured Dolt password (gt-napz3).
func TestMaintenanceDSNCarriesPassword(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTownDoltSettings(t, townRoot, `{"password":"secret"}`)
	d := compactorDogTestDaemon(townRoot, nil)
	if got := d.maintenanceDSN("hq", time.Minute); !strings.Contains(got, ":secret@") {
		t.Errorf("maintenanceDSN = %q, want it to hold :secret@", got)
	}
}

// A shutdown cancels the daemon's context, and with it the gc call. That is
// the daemon leaving, not a gc that failed: nothing escalates, and the cycle
// is not marked finished, so the database stays due for the next daemon.
func TestMaintenanceGCCycleShutdownNoEscalation(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "gt": {600 * mib, 0}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.maint.gcExec = func(ctx context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		cancel() // shutdown() cancels d.ctx while the gc runs
		return ctx.Err()
	}

	// The window's dispatch: backup, then the cycle, then the finished mark.
	d.startMaintenanceGC([]string{"hq", "gt"}, gcTestNow.Add(time.Hour), nil, true)

	if got := strings.Join(f.gcCalls, ","); got != "gt" {
		t.Errorf("gc calls = %s, want gt only — the run must stop at the shutdown", got)
	}
	if len(f.escalations) != 0 {
		t.Errorf("a shutdown escalated as a gc failure: %v", f.escalations)
	}
	if d.maintenanceGCFinishedAt.Load() != 0 {
		t.Error("a gc cut off by shutdown marked the cycle finished")
	}
	if !strings.Contains(logs.String(), "cycle interrupted") {
		t.Errorf("log does not name the interruption:\n%s", logs.String())
	}
	if f.marker != nil {
		t.Errorf("pause marker left behind: %+v", f.marker)
	}
}

// writeWindowNightBackup leaves a complete night on disk holding databases, as
// a tick earlier tonight left it for the Taken shortcut to find.
func writeWindowNightBackup(t *testing.T, root string, started time.Time, databases ...string) {
	t.Helper()
	if err := os.MkdirAll(doltbackup.PartialDir(root, started), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := doltbackup.Commit(root, doltbackup.Manifest{
		Started: started, Finished: started, Databases: databases,
	}); err != nil {
		t.Fatal(err)
	}
}

// A retried tick whose night is already on disk backs nothing up, so a database
// created after that backup is in no backup at all; the gc skips it and runs
// over the database the manifest holds (gt-gd75is).
func TestMaintenanceGCSkipsADatabaseTheNightsBackupLacks(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "newdb": {10 * mib, 0}}
	writeWindowNightBackup(t, f.backupRoot, gcTestNow, "hq")

	d.startMaintenanceGC([]string{"hq", "newdb"}, gcTestNow.Add(time.Hour), nil, true)

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq only — newdb is in no backup tonight", got)
	}
	if !strings.Contains(logs.String(), "newdb") {
		t.Errorf("log does not name newdb as unbacked:\n%s", logs.String())
	}
}

// A database the previous night's backup holds is still unbacked tonight: the
// night already on disk is the one that counts, and only its manifest may
// decide what this gc runs over. The catch-up's age rule accepts yesterday's
// backup, so applying it here would gc newdb with no backup of it tonight
// (gt-gd75is).
func TestMaintenanceGCWindowIgnoresAPreviousNightsBackup(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}, "newdb": {10 * mib, 0}}
	writeWindowNightBackup(t, f.backupRoot, gcTestNow.AddDate(0, 0, -1), "newdb")
	writeWindowNightBackup(t, f.backupRoot, gcTestNow, "hq")

	d.startMaintenanceGC([]string{"hq", "newdb"}, gcTestNow.Add(time.Hour), nil, true)

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq only — newdb is in yesterday's backup, not tonight's", got)
	}
	if !strings.Contains(logs.String(), "newdb") {
		t.Errorf("log does not name newdb as unbacked tonight:\n%s", logs.String())
	}
}

// A night whose backup holds the database still gc's it: filtering the window's
// list leaves a normal night unchanged (gt-gd75is).
func TestMaintenanceGCWindowRunsADatabaseTheBackupHolds(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	writeWindowNightBackup(t, f.backupRoot, gcTestNow, "hq")

	d.startMaintenanceGC([]string{"hq"}, gcTestNow.Add(time.Hour), nil, true)

	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
}

// A night that backs up nothing the tick lists leaves nothing to gc: that is a
// completed night, not a failed one, so nothing escalates (gt-gd75is).
func TestMaintenanceGCWindowWithNothingBackedUpIsNotAFailure(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"newdb": {10 * mib, 0}}
	writeWindowNightBackup(t, f.backupRoot, gcTestNow, "hq")

	d.startMaintenanceGC([]string{"newdb"}, gcTestNow.Add(time.Hour), nil, true)

	if len(f.gcCalls) != 0 {
		t.Errorf("gc calls = %v, want none — no backup holds newdb", f.gcCalls)
	}
	if len(f.escalations) != 0 {
		t.Errorf("skipping an unbacked database escalated: %v", f.escalations)
	}
	if !strings.Contains(logs.String(), "cycle completed") {
		t.Errorf("the night did not complete without a gc:\n%s", logs.String())
	}
}

// A backup root the window cannot read fails the night before any gc: the
// backup step itself reads the root, so there is no list of databases tonight's
// backup holds to gc over, and a completed outcome would report a night the gc
// never checked (gt-gd75is).
func TestMaintenanceGCWindowRunsNoGCWhenTheBackupRootCannotBeRead(t *testing.T) {
	t.Parallel()
	d, logs := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	d.maint.backupRoot = func() (string, error) {
		return "", errors.New("injected: the backup root went away")
	}

	d.startMaintenanceGC([]string{"hq"}, gcTestNow.Add(time.Hour), nil, true)

	if len(f.gcCalls) != 0 {
		t.Errorf("gc ran with no readable backup root: %v", f.gcCalls)
	}
	if len(f.escalations) != 1 || !strings.Contains(f.escalations[0], "injected: the backup root went away") {
		t.Errorf("escalations = %v, want one naming the backup root failure", f.escalations)
	}
	if strings.Contains(logs.String(), "cycle completed") {
		t.Errorf("the night reported completed with no readable backup root:\n%s", logs.String())
	}
}

// A normal night takes its own backup and then gc's every database in it: the
// databases the backup copied are the ones the gc certifies it may run over
// (gt-gd75is).
func TestMaintenanceGCWindowTakesItsOwnBackupThenRunsGC(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}

	d.startMaintenanceGC([]string{"hq"}, gcTestNow.Add(time.Hour), nil, true)

	if got := strings.Join(f.backupCalls, ","); got != "hq" {
		t.Errorf("backup calls = %s, want hq", got)
	}
	if got := strings.Join(f.gcCalls, ","); got != "hq" {
		t.Errorf("gc calls = %s, want hq", got)
	}
}

// A gc that times out while the daemon is still running is a real failure and
// escalates: the interruption branch keys on the daemon leaving, not on any
// context error.
func TestMaintenanceGCCycleTimeoutStillEscalates(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	d.ctx = context.Background()
	f.gcErr = map[string]error{"hq": context.DeadlineExceeded}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if len(f.escalations) != 1 {
		t.Errorf("escalations = %v, want one", f.escalations)
	}
}

// A gc that fails for a reason of its own while the daemon is leaving is still
// a failure: only the call's cancellation is the shutdown's doing.
func TestMaintenanceGCCycleShutdownDoesNotMaskARealFailure(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	f.sizes = map[string]gcMeasure{"hq": {300 * mib, 0}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.ctx = ctx
	d.maint.gcExec = func(_ context.Context, _ *Daemon, db string) error {
		f.gcCalls = append(f.gcCalls, db)
		cancel() // the shutdown begins, but the gc fails on its own
		return errors.New("dolt_gc --full: connection refused")
	}

	out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true)

	if out.outcome != gcOutcomeFailed {
		t.Fatalf("outcome = %v, want failed", out.outcome)
	}
	if len(f.escalations) != 1 {
		t.Errorf("escalations = %v, want one", f.escalations)
	}
}

// --- the gc statement ----------------------------------------------------------------

// gcFakeRecorder collects the statements a fake gc connection executes.
type gcFakeRecorder struct {
	queries []string
}

// gcFakeOpener returns a fake connection for one test whose statements fail
// with execErr (nil: they succeed), and the pool opener the daemon should use.
// The fake is a driver.Connector, so the test's own pool owns it and nothing is
// registered process-wide.
func gcFakeOpener(t *testing.T, execErr error) (*gcFakeRecorder, func(string) (*sql.DB, error)) {
	t.Helper()
	rec := &gcFakeRecorder{}
	return rec, func(string) (*sql.DB, error) {
		return sql.OpenDB(gcFakeConnector{rec: rec, execErr: execErr}), nil
	}
}

// gcFakeConnector opens the fake connection for the one pool sql.OpenDB builds.
type gcFakeConnector struct {
	rec     *gcFakeRecorder
	execErr error
}

func (c gcFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return gcFakeConn{rec: c.rec, execErr: c.execErr}, nil
}

// Driver satisfies driver.Connector; connections arrive through Connect.
func (c gcFakeConnector) Driver() driver.Driver {
	return gcFakeDriver{rec: c.rec, execErr: c.execErr}
}

type gcFakeDriver struct {
	rec     *gcFakeRecorder
	execErr error
}

func (d gcFakeDriver) Open(string) (driver.Conn, error) {
	return gcFakeConn{rec: d.rec, execErr: d.execErr}, nil
}

// deadlinePassedContext reports an expired deadline while its Done channel
// stays open. database/sql refuses a call on an already-done context before it
// reaches the driver, so driving a driver error under an expired deadline needs
// Err to answer for the deadline while Done never fires.
type deadlinePassedContext struct{}

func (deadlinePassedContext) Deadline() (time.Time, bool) { return time.Unix(0, 0), true }
func (deadlinePassedContext) Done() <-chan struct{}       { return nil }
func (deadlinePassedContext) Err() error                  { return context.DeadlineExceeded }
func (deadlinePassedContext) Value(any) any               { return nil }

type gcFakeConn struct {
	rec     *gcFakeRecorder
	execErr error
}

func (c gcFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("gc fake: prepare is not used")
}

func (c gcFakeConn) Close() error { return nil }

func (c gcFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("gc fake: transactions are not used")
}

func (c gcFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.rec.queries = append(c.rec.queries, query)
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(0), nil
}

func TestGCArchiveLevelFullCallPassesLevelZero(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	rec, open := gcFakeOpener(t, nil)
	d.maint.gcOpenDB = open

	if err := d.doltGCFull(context.Background(), "hq"); err != nil {
		t.Fatalf("doltGCFull(hq) = %v, want nil", err)
	}
	const want = "CALL dolt_gc('--full', '--archive-level=0')"
	if len(rec.queries) != 1 || rec.queries[0] != want {
		t.Fatalf("executed %q, want exactly [%q]", rec.queries, want)
	}
}

// The scheduled cycle's own call, through its production path (gcExec unset, so
// maintenanceGCOne reaches doltGCFull), is exactly the explicit level-zero
// statement: a bare CALL dolt_gc() is archive level 1, and that measured
// 22.6 GiB against 7.1 GiB (gt-k3u2e, gt-lc16x3).
func TestMaintenanceGCCallsArchiveLevelZero(t *testing.T) {
	t.Parallel()
	d, _ := gcTestDaemon(t)
	f := withGCFakes(t, d)
	d.maint.gcExec = nil // the production call, on a fake driver
	rec, open := gcFakeOpener(t, nil)
	d.maint.gcOpenDB = open
	f.sizes = map[string]gcMeasure{"hq": {631 * mib, 0}}

	if out := d.maintenanceGCCycle([]string{"hq"}, "/unused", true); out.outcome != gcOutcomeCompleted {
		t.Fatalf("outcome = %v, want completed", out.outcome)
	}
	const want = "CALL dolt_gc('--full', '--archive-level=0')"
	if len(rec.queries) != 1 || rec.queries[0] != want {
		t.Fatalf("executed %q, want exactly [%q]", rec.queries, want)
	}
}

func TestGCArchiveLevelTimeoutAndErrorWrappingUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		driverErr := errors.New("gc fake: cut off")
		_, open := gcFakeOpener(t, driverErr)
		d.maint.gcOpenDB = open

		err := d.doltGCFull(deadlinePassedContext{}, "hq")
		if err == nil || !strings.Contains(err.Error(), "dolt_gc --full: timeout:") {
			t.Fatalf("doltGCFull() = %v, want a dolt_gc --full: timeout error", err)
		}
		if !errors.Is(err, driverErr) {
			t.Errorf("doltGCFull() = %v, want it to wrap the driver error", err)
		}
	})

	t.Run("other failure", func(t *testing.T) {
		t.Parallel()
		d, _ := gcTestDaemon(t)
		sentinel := errors.New("gc fake: connection reset")
		_, open := gcFakeOpener(t, sentinel)
		d.maint.gcOpenDB = open

		err := d.doltGCFull(context.Background(), "hq")
		if err == nil || !strings.Contains(err.Error(), "dolt_gc --full: ") || strings.Contains(err.Error(), "timeout") {
			t.Fatalf("doltGCFull() = %v, want a dolt_gc --full: error that is not a timeout", err)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("doltGCFull() = %v, want it to wrap the driver error", err)
		}
	})
}
