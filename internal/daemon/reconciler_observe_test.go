package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// observeFailingFS fails every open with err, so a read that fails for a reason
// other than a missing directory is reachable without a disk.
type observeFailingFS struct{ err error }

func (f observeFailingFS) Open(string) (fs.File, error) { return nil, f.err }

func listDatabases(dbs []string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return dbs, nil }
}

func observationsFor(got []reconcile.Observation, duty string) []reconcile.Observation {
	var found []reconcile.Observation
	for _, obs := range got {
		if obs.Duty == duty || strings.HasPrefix(obs.Duty, duty+":") {
			found = append(found, obs)
		}
	}
	return found
}

func TestReconcilerObserveDatabaseBackup(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const (
		backupRoot = "/home/gt/.gt/backups"
		relRoot    = "home/gt/.gt/backups"
		archiveDir = "/home/gt/.runtime/dolt-archive"
		relArchive = "home/gt/.runtime/dolt-archive"
		nightName  = "2026-11-25"
	)
	dbs := []string{"hq", "gastown"}
	// night is one complete backup night committed age before now.
	night := func(age time.Duration) fstest.MapFS {
		dir := relRoot + "/" + nightName
		tree := fstest.MapFS{
			relRoot: {Mode: fs.ModeDir | 0o755},
			dir:     {Mode: fs.ModeDir | 0o755, ModTime: now.Add(-age)},
		}
		for _, db := range dbs {
			tree[dir+"/"+db] = &fstest.MapFile{Mode: fs.ModeDir | 0o755}
		}
		return tree
	}
	freshArchive := fstest.MapFS{relArchive + "/dolt-log-gt-20261125.csv.gz": {Data: []byte("x"), ModTime: now.Add(-time.Hour)}}
	listErr := errors.New("listing the databases failed")
	tests := []struct {
		name        string
		cfg         *config.ReconcilerConfig
		root        string // empty means backupRoot
		fsys        fs.FS
		databases   func(context.Context) ([]string, error)
		want        reconcile.Verdict
		wantAge     time.Duration
		wantErr     error
		wantAnyErr  bool
		wantArchive reconcile.Verdict
	}{
		{name: "fresh night is ok", fsys: night(10 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictOK, wantAge: 10 * time.Hour},
		{name: "past the limit is due", fsys: night(25 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictDue, wantAge: 25 * time.Hour},
		{name: "past the limit plus grace is overdue", fsys: night(27 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictOverdue, wantAge: 27 * time.Hour},
		{name: "a configured limit is the one judged", cfg: &config.ReconcilerConfig{BackupLimit: "2h"}, fsys: night(3 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictDue, wantAge: 3 * time.Hour},
		{name: "no complete night is overdue with no result", fsys: fstest.MapFS{relRoot: {Mode: fs.ModeDir}}, databases: listDatabases(dbs), want: reconcile.VerdictOverdue, wantErr: reconcile.ErrNoResult},
		{name: "a failed listing is unknown with its error", fsys: freshArchive, databases: func(context.Context) ([]string, error) { return nil, listErr }, want: reconcile.VerdictUnknown, wantErr: listErr, wantArchive: reconcile.VerdictOK},
		{name: "no listing wired is unknown", fsys: night(10 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "unparseable backup limit is unknown", cfg: &config.ReconcilerConfig{BackupLimit: "soon"}, fsys: night(10 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "unparseable grace is unknown", cfg: &config.ReconcilerConfig{Grace: "later"}, fsys: night(10 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "relative backup root is unknown", root: "relative/backups", fsys: night(10 * time.Hour), databases: listDatabases(dbs), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "no filesystem is unknown", fsys: nil, databases: listDatabases(dbs), want: reconcile.VerdictUnknown, wantAnyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeDatabaseDuties(t.Context(), now, tt.cfg, dutyInputs{
				databases: tt.databases, backupRoot: cmp.Or(tt.root, backupRoot), archiveDir: archiveDir, fsys: tt.fsys,
			})
			backups := observationsFor(got, config.ReconcilerDutyBackup)
			archives := observationsFor(got, config.ReconcilerDutyArchive)
			if len(backups) != 1 {
				t.Fatalf("observeDatabaseDuties returned %d backup observations, want 1: %+v", len(backups), got)
			}
			obs := backups[0]
			if obs.Verdict != tt.want {
				t.Errorf("backup verdict = %q, want %q (observation %+v)", obs.Verdict, tt.want, obs)
			}
			if tt.wantAge != 0 && obs.Age != tt.wantAge {
				t.Errorf("backup Age = %v, want %v", obs.Age, tt.wantAge)
			}
			if tt.wantErr != nil && !errors.Is(obs.Err, tt.wantErr) {
				t.Errorf("backup Err = %v, want it to wrap %v", obs.Err, tt.wantErr)
			}
			if tt.wantAnyErr && obs.Err == nil {
				t.Errorf("backup Err = nil, want the refusal kept (observation %+v)", obs)
			}
			if tt.wantErr == nil && !tt.wantAnyErr && obs.Err != nil {
				t.Errorf("backup Err = %v, want nil", obs.Err)
			}
			if tt.wantArchive != "" && (len(archives) != 1 || archives[0].Verdict != tt.wantArchive) {
				t.Errorf("archive observations = %+v, want one %q", archives, tt.wantArchive)
			}
		})
	}
}

func TestReconcilerObserveDatabaseArchive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const archiveDir = "/home/gt/.runtime/dolt-archive"
	// The fake filesystem stands for os.DirFS("/"), so the archive directory
	// is named without its leading slash.
	const relDir = "home/gt/.runtime/dolt-archive"
	archiveFile := func(age time.Duration) fstest.MapFS {
		return fstest.MapFS{relDir + "/dolt-log-gt-20261125.csv.gz": {Data: []byte("x"), ModTime: now.Add(-age)}}
	}
	tests := []struct {
		name       string
		archiveDir string
		cfg        *config.ReconcilerConfig
		fsys       fs.FS
		wantNone   bool
		want       reconcile.Verdict
		wantAge    time.Duration
		wantErr    error
		wantAnyErr bool
	}{
		{name: "fresh archive is ok", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: archiveFile(10 * time.Hour), want: reconcile.VerdictOK, wantAge: 10 * time.Hour},
		{name: "nil config reads the default limit", archiveDir: archiveDir, cfg: nil, fsys: archiveFile(10 * time.Hour), want: reconcile.VerdictOK, wantAge: 10 * time.Hour},
		{name: "past the default limit is due", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: archiveFile(25 * time.Hour), want: reconcile.VerdictDue, wantAge: 25 * time.Hour},
		{name: "past the limit plus grace is overdue", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: archiveFile(27 * time.Hour), want: reconcile.VerdictOverdue, wantAge: 27 * time.Hour},
		{name: "a configured limit is the one judged", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{ArchiveLimit: "2h"}, fsys: archiveFile(3 * time.Hour), want: reconcile.VerdictDue, wantAge: 3 * time.Hour},
		{name: "no archive file is overdue with no result", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: fstest.MapFS{relDir: {Mode: fs.ModeDir}}, want: reconcile.VerdictOverdue, wantErr: reconcile.ErrNoResult},
		{name: "missing archive directory is overdue with no result", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: fstest.MapFS{}, want: reconcile.VerdictOverdue, wantErr: reconcile.ErrNoResult},
		{name: "unreadable archive directory is unknown", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: observeFailingFS{err: fs.ErrPermission}, want: reconcile.VerdictUnknown, wantErr: fs.ErrPermission},
		{name: "archive dated after now is unknown", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: archiveFile(-time.Hour), want: reconcile.VerdictUnknown},
		{name: "unparseable limit is unknown", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{ArchiveLimit: "soon"}, fsys: archiveFile(10 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "unparseable grace is unknown", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{Grace: "later"}, fsys: archiveFile(10 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "relative archive directory is unknown", archiveDir: "relative/dolt-archive", cfg: &config.ReconcilerConfig{}, fsys: archiveFile(10 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "no filesystem is unknown", archiveDir: archiveDir, cfg: &config.ReconcilerConfig{}, fsys: nil, want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "empty archive directory observes nothing", archiveDir: "", cfg: &config.ReconcilerConfig{}, fsys: archiveFile(10 * time.Hour), wantNone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeDatabaseDuties(t.Context(), now, tt.cfg, dutyInputs{archiveDir: tt.archiveDir, fsys: tt.fsys})
			archive := observationsFor(got, config.ReconcilerDutyArchive)
			if tt.wantNone {
				if len(archive) != 0 {
					t.Fatalf("observeDatabaseDuties = %+v, want no archive observation", got)
				}
				return
			}
			if len(archive) != 1 {
				t.Fatalf("observeDatabaseDuties returned %d archive observations, want 1: %+v", len(archive), got)
			}
			obs := archive[0]
			if obs.Duty != config.ReconcilerDutyArchive || obs.Verdict != tt.want {
				t.Errorf("observation = %q %q, want %q %q", obs.Duty, obs.Verdict, config.ReconcilerDutyArchive, tt.want)
			}
			if tt.wantAge != 0 && obs.Age != tt.wantAge {
				t.Errorf("Age = %v, want %v", obs.Age, tt.wantAge)
			}
			if tt.wantErr != nil && !errors.Is(obs.Err, tt.wantErr) {
				t.Errorf("Err = %v, want it to wrap %v", obs.Err, tt.wantErr)
			}
			if tt.wantErr == nil && !tt.wantAnyErr && obs.Err != nil {
				t.Errorf("Err = %v, want nil", obs.Err)
			}
			if tt.wantAnyErr && obs.Err == nil {
				t.Errorf("Err = nil, want the refusal kept (observation %+v)", obs)
			}
		})
	}
}

func TestReconcilerObserveTownStateExplicitOff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		rootErr error
	}{
		{name: "the off error", rootErr: errTownStateNightlyOff},
		{name: "a wrapped off error", rootErr: fmt.Errorf("reading the nightly root: %w", errTownStateNightlyOff)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeTownStateDuty(now, &config.ReconcilerConfig{}, "/home/gt/.gt/town-state", tt.rootErr, fstest.MapFS{})
			if len(got) != 0 {
				t.Fatalf("observeTownStateDuty = %+v, want no observation for an explicitly empty root", got)
			}
		})
	}
}

func TestReconcilerObserveTownStateUnsetIsOverdue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		rootErr error
	}{
		{name: "an unset root", rootErr: fmt.Errorf("%s is unset", config.MaintenanceTownStateBackupRootKey)},
		{name: "a relative root", rootErr: fmt.Errorf("%s = %q: want an absolute path", config.MaintenanceTownStateBackupRootKey, "relative/town-state")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeTownStateDuty(now, &config.ReconcilerConfig{}, "", tt.rootErr, fstest.MapFS{})
			if len(got) != 1 || got[0].Duty != config.ReconcilerDutyTownState {
				t.Fatalf("%s: observeTownStateDuty = %+v, want one %s observation", tt.name, got, config.ReconcilerDutyTownState)
			}
			obs := got[0]
			if obs.Verdict != reconcile.VerdictOverdue {
				t.Errorf("%s: verdict = %q, want %q (observation %+v)", tt.name, obs.Verdict, reconcile.VerdictOverdue, obs)
			}
			if !errors.Is(obs.Err, reconcile.ErrNoResult) {
				t.Errorf("%s: Err = %v, want ErrNoResult", tt.name, obs.Err)
			}
			if !obs.LastGood.IsZero() {
				t.Errorf("%s: LastGood = %v, want the zero time", tt.name, obs.LastGood)
			}
		})
	}
}

func TestReconcilerObserveTownStateReadsNights(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const (
		root = "/home/gt/.gt/town-state"
		// The fake filesystem stands for os.DirFS("/"), so the root is named
		// without its leading slash.
		relRoot = "home/gt/.gt/town-state"
	)
	// night is one complete town-state night committed age before now.
	night := func(age time.Duration) fstest.MapFS {
		return fstest.MapFS{relRoot + "/2026-11-25/state.tar.gz": {Data: []byte("x"), ModTime: now.Add(-age)}}
	}
	readErr := errors.New("reading the town-state nights failed")
	tests := []struct {
		name       string
		cfg        *config.ReconcilerConfig
		fsys       fs.FS
		want       reconcile.Verdict
		wantAge    time.Duration
		wantErr    error
		wantAnyErr bool // want an error whose identity the row does not name
	}{
		{name: "a night 5h old is ok", fsys: night(5 * time.Hour), want: reconcile.VerdictOK, wantAge: 5 * time.Hour},
		{name: "a configured limit is the one judged", cfg: &config.ReconcilerConfig{TownStateLimit: "2h"}, fsys: night(3 * time.Hour), want: reconcile.VerdictDue, wantAge: 3 * time.Hour},
		{name: "past the limit plus grace is overdue", fsys: night(27 * time.Hour), want: reconcile.VerdictOverdue, wantAge: 27 * time.Hour},
		{name: "no complete night is overdue with no result", fsys: fstest.MapFS{relRoot: {Mode: fs.ModeDir}}, want: reconcile.VerdictOverdue, wantErr: reconcile.ErrNoResult},
		{name: "a missing root is overdue with no result", fsys: fstest.MapFS{}, want: reconcile.VerdictOverdue, wantErr: reconcile.ErrNoResult},
		{name: "an unreadable root is unknown with its error", fsys: observeFailingFS{err: readErr}, want: reconcile.VerdictUnknown, wantErr: readErr},
		{name: "no filesystem is unknown", fsys: nil, want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "unparseable town-state limit is unknown", cfg: &config.ReconcilerConfig{TownStateLimit: "soon"}, fsys: night(5 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
		{name: "unparseable grace is unknown", cfg: &config.ReconcilerConfig{Grace: "later"}, fsys: night(5 * time.Hour), want: reconcile.VerdictUnknown, wantAnyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeTownStateDuty(now, tt.cfg, root, nil, tt.fsys)
			if len(got) != 1 || got[0].Duty != config.ReconcilerDutyTownState {
				t.Fatalf("%s: observeTownStateDuty = %+v, want one %s observation", tt.name, got, config.ReconcilerDutyTownState)
			}
			obs := got[0]
			if obs.Verdict != tt.want {
				t.Errorf("%s: verdict = %q, want %q (observation %+v)", tt.name, obs.Verdict, tt.want, obs)
			}
			if tt.wantAge != 0 && obs.Age != tt.wantAge {
				t.Errorf("%s: Age = %v, want %v", tt.name, obs.Age, tt.wantAge)
			}
			if tt.wantErr != nil && !errors.Is(obs.Err, tt.wantErr) {
				t.Errorf("%s: Err = %v, want it to wrap %v", tt.name, obs.Err, tt.wantErr)
			}
			wantErr := tt.wantErr != nil || tt.wantAnyErr
			if (obs.Err != nil) != wantErr {
				t.Errorf("%s: Err = %v, want an error present: %v", tt.name, obs.Err, wantErr)
			}
			if wantErr && !obs.LastGood.IsZero() {
				t.Errorf("%s: LastGood = %v, want the zero time on an error", tt.name, obs.LastGood)
			}
		})
	}
}

func TestReconcilerObserveDatabaseGC(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const backupRoot = "/home/gt/.gt/backups"
	dbs := []string{"hq", "gastown"}
	// state is a daemon maintenance state file holding the given last_gc entries.
	state := func(lastGC string) []byte { return []byte(`{"last_gc":{` + lastGC + `}}`) }
	read := func(data []byte, err error) func() ([]byte, error) {
		return func() ([]byte, error) { return data, err }
	}
	// Ages are named against now, 2026-11-26T12:00:00Z. ok is inside a 2h limit;
	// stale is past it and its grace.
	const ok, stale = `"hq":"2026-11-26T10:00:00Z"`, `"gastown":"2026-11-26T07:00:00Z"`
	listErr := errors.New("listing the databases failed")
	readErr := errors.New("reading the maintenance state failed")
	tests := []struct {
		name       string
		cfg        *config.ReconcilerConfig
		read       func() ([]byte, error)
		databases  func(context.Context) ([]string, error)
		want       map[string]reconcile.Verdict // database name, or "" for the duty-wide observation
		wantErr    error
		wantAnyErr bool // want an error whose identity the row does not name
	}{
		{name: "each database is judged by its own last gc under the configured limit", cfg: &config.ReconcilerConfig{GCLimit: "2h"}, read: read(state(ok+","+stale), nil), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"hq": reconcile.VerdictOK, "gastown": reconcile.VerdictOverdue}},
		{name: "an absent state file is overdue for every database", read: read(nil, &fs.PathError{Op: "open", Path: "maintenance_state.json", Err: fs.ErrNotExist}), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"hq": reconcile.VerdictOverdue, "gastown": reconcile.VerdictOverdue}, wantErr: reconcile.ErrNoResult},
		{name: "an unparseable state is unknown", read: read([]byte(`{"last_gc": `), nil), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantAnyErr: true},
		{name: "a failed listing is unknown", read: read(state(ok), nil), databases: func(context.Context) ([]string, error) { return nil, listErr }, want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantErr: listErr},
		{name: "an unreadable state file is unknown", read: read(nil, readErr), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantErr: readErr},
		{name: "no gc state reader is unknown", databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantAnyErr: true},
		{name: "unparseable gc limit is unknown", cfg: &config.ReconcilerConfig{GCLimit: "soon"}, read: read(state(ok), nil), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantAnyErr: true},
		{name: "unparseable grace is unknown", cfg: &config.ReconcilerConfig{Grace: "later"}, read: read(state(ok), nil), databases: listDatabases(dbs), want: map[string]reconcile.Verdict{"": reconcile.VerdictUnknown}, wantAnyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := observeDatabaseDuties(t.Context(), now, tt.cfg, dutyInputs{
				databases: tt.databases, backupRoot: backupRoot, readGCState: tt.read,
			})
			gc := observationsFor(got, config.ReconcilerDutyGC)
			if len(gc) != len(tt.want) {
				t.Fatalf("observeDatabaseDuties returned %d gc observations, want %d: %+v", len(gc), len(tt.want), gc)
			}
			byDuty := make(map[string]reconcile.Observation, len(gc))
			for _, obs := range gc {
				byDuty[obs.Duty] = obs
			}
			for db, verdict := range tt.want {
				duty := config.ReconcilerDutyGC
				if db != "" {
					duty += ":" + db
				}
				obs, ok := byDuty[duty]
				if !ok {
					t.Fatalf("no observation named %q in %+v", duty, gc)
				}
				if obs.Verdict != verdict {
					t.Errorf("%s verdict = %q, want %q (observation %+v)", duty, obs.Verdict, verdict, obs)
				}
				if wantErr := tt.wantErr != nil || tt.wantAnyErr; (obs.Err != nil) != wantErr {
					t.Errorf("%s Err = %v, want an error present: %v", duty, obs.Err, wantErr)
				}
				if tt.wantErr != nil && !errors.Is(obs.Err, tt.wantErr) {
					t.Errorf("%s Err = %v, want it to wrap %v", duty, obs.Err, tt.wantErr)
				}
			}
		})
	}
}
