package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// liveDatabases is the list the fixture's fake server serves.
func liveDatabases() []string { return []string{"hq", "gastown"} }

// liveDaemon builds a daemon whose live inputs all read inside their limits at
// now: a complete backup night, a fresh archive file, a recent gc and a fresh
// town-state night, each under the test's own temp dirs. The host seams are
// fakes over those dirs, so no Dolt server or backup runs.
func liveDaemon(t *testing.T, now time.Time) *Daemon {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// file writes data at path and dates it at mod, since each duty judges its
	// result by modification time.
	file := func(path, data string, mod time.Time) {
		t.Helper()
		must(os.MkdirAll(filepath.Dir(path), 0o755))
		must(os.WriteFile(path, []byte(data), 0o644))
		must(os.Chtimes(path, mod, mod))
	}
	town, backup, archive, townState := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	night := filepath.Join(backup, "2026-11-25")
	for _, db := range liveDatabases() {
		must(os.MkdirAll(filepath.Join(night, db), 0o755))
	}
	// The night directory's own mtime dates the backup, so stamp it last.
	at := now.Add(-10 * time.Hour)
	must(os.Chtimes(night, at, at))
	file(filepath.Join(archive, "dolt-log-gt-20261125.csv.gz"), "x", at)
	file(filepath.Join(town, "daemon", maintenanceGCStateFileName),
		`{"last_gc":{"hq":"2026-11-26T09:00:00Z","gastown":"2026-11-26T09:00:00Z"}}`, now)
	file(filepath.Join(townState, "2026-11-25", "state.tar.gz"), "x", now.Add(-5*time.Hour))

	return &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			Reconciler: &config.ReconcilerConfig{},
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				ArchiveDir:          archive,
				TownStateBackupRoot: &townState,
			},
		}},
		maint: maintenanceSeams{
			serverDatabases: func(*Daemon) ([]string, error) { return liveDatabases(), nil },
			backupRoot:      func() (string, error) { return backup, nil },
		},
	}
}

// liveSignature renders a pass as sorted "duty=verdict" parts, each with "!"
// when it carries an error, so one row states every observation.
func liveSignature(obs []reconcile.Observation) string {
	parts := make([]string, 0, len(obs))
	for _, o := range obs {
		part := o.Duty + "=" + string(o.Verdict)
		if o.Err != nil {
			part += "!"
		}
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// TestReconcilerLiveInputsWired: observeDuties judges all four duty families
// from the live inputs, and a seam that fails reads unknown with its own error,
// not fresh or absent. The town-state root keeps gt-6ogbqv's judgement, so an
// unset or unreadable one reads overdue with no result.
func TestReconcilerLiveInputsWired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	listErr := errors.New("listing the databases failed")
	backupErr := errors.New("reading the backup root failed")
	tests := []struct {
		name    string
		change  func(*Daemon)
		want    string
		errText string // text one observation's error must name
	}{
		{name: "every live input is wired",
			want: "archive=ok backup=ok gc:gastown=ok gc:hq=ok town_state=ok"},
		{name: "a failed database listing is an unknown backup and gc",
			change: func(d *Daemon) {
				d.maint.serverDatabases = func(*Daemon) ([]string, error) { return nil, listErr }
			},
			want:    "archive=ok backup=unknown! gc=unknown! town_state=ok",
			errText: "listing the databases failed"},
		{name: "a failed backup root is an unknown backup with its own error",
			change: func(d *Daemon) {
				d.maint.backupRoot = func() (string, error) { return "", backupErr }
			},
			want:    "archive=ok backup=unknown! gc:gastown=ok gc:hq=ok town_state=ok",
			errText: "reading the backup root failed"},
		{name: "a refused archive setting is an unknown archive",
			change: func(d *Daemon) {
				d.patrolConfig.Patrols.ScheduledMaintenance.ArchiveDir = "relative/archive"
			},
			want:    "archive=unknown! backup=ok gc:gastown=ok gc:hq=ok town_state=ok",
			errText: "relative/archive"},
		{name: "an unreadable gc state is an unknown gc",
			change: func(d *Daemon) {
				path := maintenanceGCStatePath(d.config.TownRoot)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "archive=ok backup=ok gc=unknown! town_state=ok"},
		{name: "a daemon without a town root does not panic",
			change: func(d *Daemon) { d.config = nil },
			want:   "archive=ok backup=ok gc=unknown! town_state=ok"},
		{name: "an unset town-state root is overdue with no result",
			change: func(d *Daemon) {
				d.patrolConfig.Patrols.ScheduledMaintenance.TownStateBackupRoot = nil
			},
			want:    "archive=ok backup=ok gc:gastown=ok gc:hq=ok town_state=overdue!",
			errText: "no result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := liveDaemon(t, now)
			if tt.change != nil {
				tt.change(d)
			}
			got := d.observeDuties(t.Context(), now)
			if sig := liveSignature(got); sig != tt.want {
				t.Errorf("observeDuties = %q, want %q", sig, tt.want)
			}
			if tt.errText != "" && !slices.ContainsFunc(got, func(o reconcile.Observation) bool {
				return o.Err != nil && strings.Contains(o.Err.Error(), tt.errText)
			}) {
				t.Errorf("no observation error names %q: %+v", tt.errText, got)
			}
		})
	}
}
