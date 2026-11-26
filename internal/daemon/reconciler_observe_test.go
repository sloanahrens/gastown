package daemon

import (
	"errors"
	"io/fs"
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
			if tt.wantNone {
				if len(got) != 0 {
					t.Fatalf("observeDatabaseDuties = %+v, want no observation", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("observeDatabaseDuties returned %d observations, want 1: %+v", len(got), got)
			}
			obs := got[0]
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
