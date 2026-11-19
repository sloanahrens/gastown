package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltsquash"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// squashTestNow is the fixed clock every squash test runs on.
var squashTestNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// squashEnabled is a squash block enabled for the gt database with the
// defaults otherwise.
func squashEnabled() *agentconfig.SquashConfig {
	return &agentconfig.SquashConfig{Enabled: true, Databases: []string{"gt"}}
}

// squashTickDaemon is a daemon whose scheduled_maintenance tick reaches the
// daily squash check: the patrol is on, no window is configured (so the tick
// stops after the checks that ride every tick), the dispatched work runs
// inline so a test's assertions never race it, and the check's Dolt reads go to
// a fake driver.
func squashTickDaemon(t *testing.T, squash *agentconfig.SquashConfig) (*Daemon, *squashFakeServer, *[]string, *bytes.Buffer) {
	t.Helper()
	server := &squashFakeServer{count: "15000"}
	var escalations []string
	logs := &bytes.Buffer{}
	d := &Daemon{
		logger:   log.New(logs, "", 0),
		config:   &Config{TownRoot: t.TempDir()},
		notifier: notifyfake.New(),
		patrolConfig: &DaemonPatrolConfig{
			Type: "daemon-patrol-config", Version: 1,
			Patrols: &PatrolsConfig{ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: true,
				Squash:  squash,
			}},
		},
	}
	backupRoot := t.TempDir()
	d.clock = clockwork.NewFakeClockAt(squashTestNow)
	d.maint.now = func() time.Time { return squashTestNow }
	d.maint.dispatch = func(fn func()) { fn() }
	d.maint.escalate = func(_ *Daemon, source, message string) {
		escalations = append(escalations, source+"|"+message)
	}
	d.maint.backupRoot = func() (string, error) { return backupRoot, nil }
	d.maint.squashOpenDB = func(string) (*sql.DB, error) {
		return sql.OpenDB(squashFakeConnector{server: server}), nil
	}
	return d, server, &escalations, logs
}

// squashBackupRoot is the daemon's backup root, failing the test when the seam
// cannot name one.
func squashBackupRoot(t *testing.T, d *Daemon) string {
	t.Helper()
	root, err := d.maint.backupRoot()
	if err != nil {
		t.Fatalf("backup root: %v", err)
	}
	return root
}

// writeSquashBackup writes a complete backup night holding gt under root: its
// manifest finished at finished, and its gt directory holding size bytes.
func writeSquashBackup(t *testing.T, root string, started, finished time.Time, size int) {
	t.Helper()
	dir := doltbackup.Dir(root, finished)
	if err := os.MkdirAll(filepath.Join(dir, "gt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gt", "data"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doltbackup.Manifest{
		Started: started, Finished: finished, Databases: []string{"gt"}, Method: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, doltbackup.ManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSquashCheckQuietDayDoesNothingAndLogsWhy is the bead's first box: a day
// with nothing over its threshold reads the numbers, logs why nothing is to be
// squashed, and sends no statement but a read.
func TestSquashCheckQuietDayDoesNothingAndLogsWhy(t *testing.T) {
	t.Parallel()
	d, server, escalations, logs := squashTickDaemon(t, squashEnabled())
	root := squashBackupRoot(t, d)
	// A backup a night keeps: under the default 5 GiB, minutes not hours.
	writeSquashBackup(t, root, squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: due — no last-run record") {
		t.Errorf("the tick did not say the check was due: %q", got)
	}
	if !strings.Contains(got, "squash: gt: no threshold tripped") {
		t.Errorf("the quiet day's reason is missing: %q", got)
	}
	if !strings.Contains(got, "backup 1.0MiB in 7s, 6h0m0s old; query p95 0s; main commits 15000") {
		t.Errorf("the readings are missing from the log: %q", got)
	}
	if len(*escalations) != 0 {
		t.Errorf("a quiet day escalated: %v", *escalations)
	}
	if d.squashCheckRunning.Load() {
		t.Error("the check left its running guard set")
	}
	if d.squashCheckRun.last().IsZero() {
		t.Error("the check did not record its completion")
	}
	if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); due.due {
		t.Error("a decided check was due again after the retry delay: it must keep the daily gate")
	}
	// 20 latency reads and one commit count, all reads: nothing is squashed
	// until D9b wires the run.
	if len(server.queries) != squashP95Rounds+1 {
		t.Errorf("the check sent %d statements, want %d", len(server.queries), squashP95Rounds+1)
	}
	for _, q := range server.queries {
		if !strings.HasPrefix(q, "SELECT") {
			t.Errorf("the check sent a statement that is not a read: %q", q)
		}
	}
}

// TestSquashCheckStepNotBuiltWhenOff is the bead's second box, driven through
// the tick where the guarantee has to hold: a block that is absent or off
// builds no step, opens no connection and logs nothing.
func TestSquashCheckStepNotBuiltWhenOff(t *testing.T) {
	t.Parallel()
	cases := map[string]*agentconfig.SquashConfig{
		"no block":      nil,
		"enabled false": {Enabled: false, Databases: []string{"gt"}},
	}
	for name, squash := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, server, _, logs := squashTickDaemon(t, squash)
			d.maint.squashOpenDB = func(string) (*sql.DB, error) {
				t.Error("the check opened a connection with the block off")
				return nil, errors.New("no server")
			}

			step, err := d.squashStep()
			if err != nil || step != nil {
				t.Fatalf("squashStep() = (%v, %v) with the block off, want (nil, nil)", step, err)
			}
			d.runScheduledMaintenance()

			if strings.Contains(logs.String(), "squash:") {
				t.Errorf("the tick logged about a block that is off: %q", logs.String())
			}
			if len(server.queries) != 0 {
				t.Errorf("the tick read from Dolt with the block off: %v", server.queries)
			}
		})
	}
}

// TestSquashCheckRefusesABlockOutOfBounds: an enabled block with a value past
// its bound builds no step, and the error names the daemon.json key.
func TestSquashCheckRefusesABlockOutOfBounds(t *testing.T) {
	t.Parallel()
	keepDays := 400
	d, server, _, logs := squashTickDaemon(t, &agentconfig.SquashConfig{
		Enabled: true, Databases: []string{"gt"}, KeepDays: &keepDays,
	})

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: WARNING: patrols.scheduled_maintenance.squash.keep_days") {
		t.Errorf("the refusal did not name the key: %q", got)
	}
	if len(server.queries) != 0 {
		t.Errorf("a refused block still read from Dolt: %v", server.queries)
	}
}

// TestSquashCheckRunsAnywayOnACorruptLastRun: a last-run record that cannot be
// parsed runs the check and says so, because reading broken state as "not due"
// is the starvation the daily gate must not hide (gt-ima2).
func TestSquashCheckRunsAnywayOnACorruptLastRun(t *testing.T) {
	t.Parallel()
	d, server, _, logs := squashTickDaemon(t, squashEnabled())
	root := squashBackupRoot(t, d)
	writeSquashBackup(t, root, squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)
	path := patrolLastRunPath(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: WARNING: last-run state unreadable") {
		t.Errorf("a corrupt last-run record was not reported: %q", got)
	}
	if !strings.Contains(got, "squash: gt: no threshold tripped") {
		t.Errorf("a corrupt last-run record held the check back: %q", got)
	}
	if len(server.queries) == 0 {
		t.Error("a corrupt last-run record left the numbers unread")
	}
}

// TestSquashCheckSkipsWhileOneRuns: two checks cannot read and record at once.
func TestSquashCheckSkipsWhileOneRuns(t *testing.T) {
	t.Parallel()
	d, server, _, logs := squashTickDaemon(t, squashEnabled())
	d.squashCheckRunning.Store(true)
	defer d.squashCheckRunning.Store(false)

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "squash: previous check still running") {
		t.Errorf("the tick did not say a check was already running: %q", logs.String())
	}
	if len(server.queries) != 0 {
		t.Errorf("the skipped check read from Dolt: %v", server.queries)
	}
	if !d.squashCheckRun.last().IsZero() {
		t.Error("the skipped check recorded a completion")
	}
}

// TestSquashCheckGatedOnceADay: a check recorded an hour ago holds the next one
// off, so the numbers are read once a day, and the tick says nothing about a
// hold it sees on every pass.
func TestSquashCheckGatedOnceADay(t *testing.T) {
	t.Parallel()
	d, server, _, logs := squashTickDaemon(t, squashEnabled())
	if err := savePatrolLastRun(d.config.TownRoot, squashCheckPatrol, squashTestNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	if strings.Contains(logs.String(), "squash:") {
		t.Errorf("a check inside its gate logged on every tick: %q", logs.String())
	}
	if len(server.queries) != 0 {
		t.Errorf("a check inside its gate read from Dolt: %v", server.queries)
	}
}

// TestSquashDecideRefusalLogsAWarning drives the engine's refusal: a keep
// window below one day is an error, not a verdict. The tick cannot reach it,
// because ParseSquash bounds keep_days first, so the test calls the check with
// a step built from settings the bounds would have refused.
func TestSquashDecideRefusalLogsAWarning(t *testing.T) {
	t.Parallel()
	d, _, _, logs := squashTickDaemon(t, squashEnabled())
	root := squashBackupRoot(t, d)
	writeSquashBackup(t, root, squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)
	step := &squashStep{settings: &agentconfig.SquashSettings{Databases: []string{"gt"}, KeepDays: 0}}

	d.runSquashCheck(step, squashTestNow)

	got := logs.String()
	if !strings.Contains(got, "squash: WARNING: gt: doltsquash: keep_days 0 is below 1") {
		t.Errorf("the refused decision did not log a WARNING: %q", got)
	}
	if strings.Contains(got, "no threshold tripped") {
		t.Errorf("the refused decision logged a verdict: %q", got)
	}
}

// TestSquashP95IsNearestRank pins the rank the p95 reads: ceil(95% of N) from
// the bottom, so the nineteenth of twenty samples is the p95, not the twentieth.
// The samples are given slowest first, so the sort is what puts them in rank.
func TestSquashP95IsNearestRank(t *testing.T) {
	t.Parallel()
	descending := func(n int) []time.Duration {
		out := make([]time.Duration, n)
		for i := range out {
			out[i] = time.Duration(n-i) * time.Millisecond
		}
		return out
	}
	cases := map[string]struct {
		samples []time.Duration
		want    time.Duration
	}{
		"one sample":         {descending(1), time.Millisecond},
		"ten samples":        {descending(10), 10 * time.Millisecond},
		"twenty samples":     {descending(20), 19 * time.Millisecond},
		"twenty-one samples": {descending(21), 20 * time.Millisecond},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := squashP95(tc.samples); got != tc.want {
				t.Errorf("squashP95 = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSquashCheckTrippedRunsTheDatabase: a reading over its threshold logs the
// trigger and the cut, then hands the database to the engine. The fake answers
// the run's first branch read with the one column it gives every read, which is
// not the two a recovery reads, so the run stops there and releases the marker.
// The check is due again after the retry delay rather than setting the daily
// gate.
func TestSquashCheckTrippedRunsTheDatabase(t *testing.T) {
	t.Parallel()
	bigger := int64(100)
	d, server, _, logs := squashTickDaemon(t, &agentconfig.SquashConfig{
		Enabled: true, Databases: []string{"gt"}, TriggerBackupBytes: &bigger,
	})
	root := squashBackupRoot(t, d)
	writeSquashBackup(t, root, squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: gt: due: backup bytes over the limit, cut 2026-11-09T12:00:00Z") {
		t.Errorf("the tripped reason or its cut is missing: %q", got)
	}
	if !strings.Contains(got, "reading the branches a dead run may have left") {
		t.Errorf("the due database was not run: %q", got)
	}
	// The readings, then the run's one branch read.
	if len(server.queries) != squashP95Rounds+2 {
		t.Errorf("the check sent %d statements, want %d", len(server.queries), squashP95Rounds+2)
	}
	if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter-time.Minute), squashCheckInterval); due.due {
		t.Error("a failed run was due again before the retry delay")
	}
	if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); !due.due {
		t.Error("a failed run was not due again after the retry delay: it set the daily gate")
	}
}

// squashDueDaemon is a daemon the next tick finds due: the backup-bytes trigger
// sits below the 1 MiB backup the caller writes.
func squashDueDaemon(t *testing.T) (*Daemon, *squashFakeServer, *bytes.Buffer) {
	t.Helper()
	small := int64(100)
	d, server, _, logs := squashTickDaemon(t, &agentconfig.SquashConfig{
		Enabled: true, Databases: []string{"gt"}, TriggerBackupBytes: &small,
	})
	return d, server, logs
}

// TestSquashMarkerRefusesASecondDaemon is the bead's first box: another
// daemon's run holds the town's marker, so this daemon's due run is refused
// with doltsquash.ErrRunMarkerHeld, sends no statement of its own, and is due
// again after the retry delay rather than for a day.
func TestSquashMarkerRefusesASecondDaemon(t *testing.T) {
	t.Parallel()
	d, server, logs := squashDueDaemon(t)
	writeSquashBackup(t, squashBackupRoot(t, d), squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)
	path := squashMarkerPath(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The other daemon's flock is a second open of the same file, which
	// conflicts with this process's own the way a second process's does.
	release, ok, err := lock.FlockTryAcquire(path)
	if err != nil {
		t.Fatalf("taking the other daemon's marker: %v", err)
	}
	if !ok {
		t.Fatal("the other daemon's marker was not free to take")
	}
	defer release()

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: gt: another daemon holds the run marker — skipping this check") {
		t.Errorf("the second daemon did not skip: %q", got)
	}
	if strings.Contains(got, "the run failed") || strings.Contains(got, "reading the branches") {
		t.Errorf("a refused run sent statements of its own: %q", got)
	}
	if len(server.queries) != squashP95Rounds+1 {
		t.Errorf("a refused run sent %d statements, want only the readings (%d)", len(server.queries), squashP95Rounds+1)
	}
	if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); !due.due {
		t.Error("a run refused by the marker set the daily gate: the check is not due again after the retry delay")
	}
}

// TestSquashMarkerUnavailableIsNotARun is the bead's second box: a marker that
// cannot be taken is its own error, not doltsquash.ErrRunMarkerHeld and not a
// run. The lock path here is a directory, which cannot be opened for locking,
// so the daemon cannot tell whether a run holds it.
func TestSquashMarkerUnavailableIsNotARun(t *testing.T) {
	t.Parallel()
	d, server, logs := squashDueDaemon(t)
	writeSquashBackup(t, squashBackupRoot(t, d), squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)
	if err := os.MkdirAll(squashMarkerPath(d.config.TownRoot), 0o755); err != nil {
		t.Fatal(err)
	}

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: WARNING: gt: doltsquash: the run marker: squash: the run marker cannot be taken:") {
		t.Errorf("a lock failure was not reported as its own error: %q", got)
	}
	if !strings.Contains(got, "the run did not start") {
		t.Errorf("the step did not say the run was not a run: %q", got)
	}
	if strings.Contains(got, "another daemon holds the run marker") {
		t.Errorf("a lock failure was read as another holder: %q", got)
	}
	if strings.Contains(got, "reading the branches") {
		t.Errorf("an untakeable marker still ran the engine: %q", got)
	}
	if len(server.queries) != squashP95Rounds+1 {
		t.Errorf("an untakeable marker sent %d statements, want only the readings (%d)", len(server.queries), squashP95Rounds+1)
	}
	if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); !due.due {
		t.Error("a run that could not take its marker set the daily gate")
	}
}

// TestSquashMarkerReleasedWhenTheRunEnds is the bead's third box: the run that
// took the marker gives it back when it ends, so the next check can take it.
// The fake stops the run at its first branch read; doltsquash.Run releases the
// marker on that way out too.
func TestSquashMarkerReleasedWhenTheRunEnds(t *testing.T) {
	t.Parallel()
	d, _, logs := squashDueDaemon(t)
	writeSquashBackup(t, squashBackupRoot(t, d), squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<20)

	d.runScheduledMaintenance()

	if !strings.Contains(logs.String(), "reading the branches a dead run may have left") {
		t.Fatalf("the run did not start, so there is no run end to test: %q", logs.String())
	}
	release, ok, err := lock.FlockTryAcquire(squashMarkerPath(d.config.TownRoot))
	if err != nil {
		t.Fatalf("taking the marker after the run: %v", err)
	}
	if !ok {
		t.Fatal("the run ended without releasing the marker")
	}
	release()
}

// TestSquashMarkerRefusesASecondHold: one marker value holds one flock, so a
// second Hold is the unavailable error rather than a second hold of the same
// lock.
func TestSquashMarkerRefusesASecondHold(t *testing.T) {
	t.Parallel()
	m := &squashMarker{path: filepath.Join(t.TempDir(), "daemon", squashRunMarkerFile)}
	if err := m.Hold(); err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	defer m.Release()

	err := m.Hold()
	if !errors.Is(err, errSquashMarkerUnavailable) {
		t.Errorf("second Hold = %v, want errSquashMarkerUnavailable", err)
	}
	if errors.Is(err, doltsquash.ErrRunMarkerHeld) {
		t.Errorf("second Hold = %v, want an error that is not ErrRunMarkerHeld", err)
	}
}

// TestSquashMarkerHoldsWithoutAFreshBackup: the design's first step runs only
// on a backup finished within 24 hours, so an older one holds the run and says
// why rather than squashing history the backup does not cover.
func TestSquashMarkerHoldsWithoutAFreshBackup(t *testing.T) {
	t.Parallel()
	d, server, logs := squashDueDaemon(t)
	writeSquashBackup(t, squashBackupRoot(t, d), squashTestNow.Add(-30*time.Hour), squashTestNow.Add(-30*time.Hour+7*time.Second), 1<<20)

	d.runScheduledMaintenance()

	got := logs.String()
	if !strings.Contains(got, "squash: gt: holding: the last backup finished 30h0m0s ago, and a run needs one within 24h0m0s") {
		t.Errorf("a stale backup did not hold the run: %q", got)
	}
	if strings.Contains(got, "reading the branches") {
		t.Errorf("a run started on a stale backup: %q", got)
	}
	if len(server.queries) != squashP95Rounds+1 {
		t.Errorf("a held run sent %d statements, want only the readings (%d)", len(server.queries), squashP95Rounds+1)
	}
}

// TestSquashCheckUnreadableReadingsDoNotTrip drives every way a reading can
// fail: each one leaves its input unreadable, logs a WARNING naming it, and
// logs no verdict, because a number nobody could read is not a quiet day's zero.
func TestSquashCheckUnreadableReadingsDoNotTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// noBackup leaves the backup root empty; every other case has a
		// complete, quiet backup of gt to fail a reading after.
		noBackup bool
		setup    func(t *testing.T, d *Daemon, s *squashFakeServer)
		wantLog  []string
	}{
		{
			name:     "no backup holds the database",
			noBackup: true,
			setup:    func(*testing.T, *Daemon, *squashFakeServer) {},
			wantLog:  []string{"unreadable: backup bytes, backup duration", "backup unreadable: squash: no complete backup holds gt"},
		},
		{
			name:     "the backup root cannot be read",
			noBackup: true,
			setup: func(_ *testing.T, d *Daemon, _ *squashFakeServer) {
				d.maint.backupRoot = func() (string, error) { return "", errors.New("no home") }
			},
			wantLog: []string{"unreadable: backup bytes, backup duration", "the backup root: no home"},
		},
		{
			name:     "the backup root is not a directory",
			noBackup: true,
			setup: func(t *testing.T, d *Daemon, _ *squashFakeServer) {
				notADir := filepath.Join(t.TempDir(), "backups")
				if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
				d.maint.backupRoot = func() (string, error) { return notADir, nil }
			},
			wantLog: []string{"unreadable: backup bytes, backup duration", "reading the backups under"},
		},
		{
			name: "the backup's size cannot be measured",
			setup: func(t *testing.T, d *Daemon, _ *squashFakeServer) {
				root := squashBackupRoot(t, d)
				if err := os.RemoveAll(filepath.Join(doltbackup.Dir(root, squashTestNow.Add(-6*time.Hour)), "gt")); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: []string{"unreadable: backup bytes", "measuring the backup of gt"},
		},
		{
			name: "the backup records no finish",
			setup: func(t *testing.T, d *Daemon, _ *squashFakeServer) {
				root := squashBackupRoot(t, d)
				dir := doltbackup.Dir(root, squashTestNow.Add(-6*time.Hour))
				data, err := json.Marshal(doltbackup.Manifest{Started: squashTestNow.Add(-6 * time.Hour), Databases: []string{"gt"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, doltbackup.ManifestName), data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: []string{"unreadable: backup duration", "cannot be read"},
		},
		{
			name: "the reads fail",
			setup: func(_ *testing.T, _ *Daemon, s *squashFakeServer) {
				s.queryErr = errors.New("connection refused")
			},
			wantLog: []string{"unreadable: query p95", "query p95 unreadable: squash: latency read 1 of 20: connection refused", "main commits unreadable"},
		},
		{
			name: "the connection cannot be opened",
			setup: func(_ *testing.T, d *Daemon, _ *squashFakeServer) {
				d.maint.squashOpenDB = func(string) (*sql.DB, error) {
					return nil, errors.New("bad dsn")
				}
			},
			wantLog: []string{"unreadable: query p95", "opening the connection to gt: bad dsn"},
		},
		{
			name: "the commit count is not a number",
			setup: func(_ *testing.T, _ *Daemon, s *squashFakeServer) {
				s.count = "many"
			},
			wantLog: []string{`the commit count of main read "many", want a number`},
		},
		{
			name: "the commit count is not one record",
			setup: func(_ *testing.T, _ *Daemon, s *squashFakeServer) {
				s.countRecords = 2
			},
			wantLog: []string{"returned 2 records holding 1 values in the first, want one value"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, server, escalations, logs := squashTickDaemon(t, squashEnabled())
			if !tc.noBackup {
				writeSquashBackup(t, squashBackupRoot(t, d), squashTestNow.Add(-6*time.Hour), squashTestNow.Add(-6*time.Hour+7*time.Second), 1<<10)
			}
			tc.setup(t, d, server)

			d.runScheduledMaintenance()

			got := logs.String()
			if strings.Contains(got, "no threshold tripped") || strings.Contains(got, "squash: gt: due") {
				t.Errorf("an unreadable reading logged a verdict: %q", got)
			}
			if !strings.Contains(got, "squash: WARNING: gt: the check did not decide, unreadable: ") {
				t.Errorf("an unreadable reading logged no WARNING naming it: %q", got)
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(got, want) {
					t.Errorf("the log does not hold %q: %q", want, got)
				}
			}
			if len(*escalations) != 0 {
				t.Errorf("an unreadable reading escalated: %v", *escalations)
			}
			// A check that could not decide is not a completed day: it is due
			// again after the retry delay, not after the daily gate.
			if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter-time.Minute), squashCheckInterval); due.due {
				t.Error("an undecided check was due again before the retry delay")
			}
			if due := evaluatePatrolDue(d.config.TownRoot, squashCheckPatrol, d.squashCheckRun.last(), squashTestNow.Add(squashCheckRetryAfter+time.Minute), squashCheckInterval); !due.due {
				t.Error("an undecided check was not due again after the retry delay: the unreadable day set the daily gate")
			}
		})
	}
}

// TestSquashDoltExecReportsAFailedRead drives the two read failures a result
// set can report through Exec, the call site the check reads through: a value
// it cannot scan, and a driver error after the last row.
func TestSquashDoltExecReportsAFailedRead(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*squashFakeServer){
		// driver.Value is any at compile time, so the fake can hand back a
		// value the text scanner refuses.
		"a value that cannot be scanned": func(s *squashFakeServer) { s.rowValue = struct{}{} },
		"a driver error after the last row": func(s *squashFakeServer) {
			s.nextErr = errors.New("read failed")
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := &squashFakeServer{count: "1"}
			set(server)
			db := sql.OpenDB(squashFakeConnector{server: server})
			defer db.Close()

			if _, err := (squashDoltExec{conn: db}).Exec(context.Background(), "SELECT 1"); err == nil {
				t.Error("Exec returned no error for a failed read")
			}
		})
	}
}

// TestSquashRowsRefusesAClosedResultSet drives the column read's failure, which
// a result set Exec opened itself cannot report: it is only closed before the
// read when the read gets one that is already closed.
func TestSquashRowsRefusesAClosedResultSet(t *testing.T) {
	t.Parallel()
	db := sql.OpenDB(squashFakeConnector{server: &squashFakeServer{count: "1"}})
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := squashRows(rows); err == nil {
		t.Error("squashRows read a closed result set")
	}
}

// squashFakeServer answers the check's statements without a server: the commit
// count statement returns count, and every other statement one row of rowValue
// ("1" unless a case sets it).
type squashFakeServer struct {
	queries      []string
	queryErr     error
	count        string
	countRecords int
	rowValue     driver.Value
	nextErr      error
}

// rowsFor is the result of one statement.
func (s *squashFakeServer) rowsFor(statement string) [][]driver.Value {
	if statement != squashMainCommitsSQL {
		value := s.rowValue
		if value == nil {
			value = "1"
		}
		return [][]driver.Value{{value}}
	}
	records := s.countRecords
	if records == 0 {
		records = 1
	}
	rows := make([][]driver.Value, records)
	for i := range rows {
		rows[i] = []driver.Value{s.count}
	}
	return rows
}

// squashFakeConnector opens the fake connection for the pool sql.OpenDB builds.
type squashFakeConnector struct{ server *squashFakeServer }

func (c squashFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return squashFakeConn{server: c.server}, nil
}

func (c squashFakeConnector) Driver() driver.Driver {
	return squashFakeDriver{server: c.server}
}

type squashFakeDriver struct{ server *squashFakeServer }

func (d squashFakeDriver) Open(string) (driver.Conn, error) {
	return squashFakeConn{server: d.server}, nil
}

type squashFakeConn struct{ server *squashFakeServer }

func (c squashFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("squash fake: prepare is not used")
}

func (c squashFakeConn) Close() error { return nil }

func (c squashFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("squash fake: transactions are not used")
}

func (c squashFakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.server.queries = append(c.server.queries, query)
	if c.server.queryErr != nil {
		return nil, c.server.queryErr
	}
	return &squashFakeRows{rows: c.server.rowsFor(query), nextErr: c.server.nextErr}, nil
}

type squashFakeRows struct {
	rows    [][]driver.Value
	n       int
	nextErr error
}

func (r *squashFakeRows) Columns() []string { return []string{"value"} }

func (r *squashFakeRows) Close() error { return nil }

func (r *squashFakeRows) Next(dest []driver.Value) error {
	if r.n >= len(r.rows) {
		if r.nextErr != nil {
			return r.nextErr
		}
		return io.EOF
	}
	copy(dest, r.rows[r.n])
	r.n++
	return nil
}

// The fake must implement the interfaces database/sql reaches for, or a query
// would fail for a reason other than the one a case is testing.
var (
	_ driver.QueryerContext = squashFakeConn{}
	_ driver.Connector      = squashFakeConnector{}
)
