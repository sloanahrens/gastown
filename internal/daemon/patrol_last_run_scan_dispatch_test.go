package daemon

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"
)

// TestPatrolLastRunScanDispatchFieldsRecord pins gt-y872pt: Daemon carries a
// last-run memory for patrol_scan and for spec_dispatch, keyed as gt jobs reads
// them, so a completed pass can be recorded there rather than reading never_run.
func TestPatrolLastRunScanDispatchFieldsRecord(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 24, 18, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		patrol string
		mem    func(*Daemon) *patrolLastRunMemory
		// at differs per row, so a run recorded through the wrong key shows as
		// the sibling's time rather than passing on an equal one.
		at time.Time
	}{
		{patrol: "patrol_scan", mem: func(d *Daemon) *patrolLastRunMemory { return &d.patrolScanRun }, at: base},
		{patrol: "spec_dispatch", mem: func(d *Daemon) *patrolLastRunMemory { return &d.specDispatchRun }, at: base.Add(time.Hour)},
	} {
		t.Run(tc.patrol, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{
				config: &Config{TownRoot: t.TempDir()},
				logger: log.New(io.Discard, "", 0),
			}

			d.recordPatrolRun(tc.mem(d), tc.patrol, tc.at)

			if got := tc.mem(d).last(); !got.Equal(tc.at) {
				t.Errorf("%s in-memory last run = %v, want %v", tc.patrol, got, tc.at)
			}
			runs, err := ReadPatrolLastRuns(d.config.TownRoot)
			if err != nil {
				t.Fatalf("ReadPatrolLastRuns: %v", err)
			}
			if got, ok := runs[tc.patrol]; !ok || !got.Equal(tc.at) {
				t.Errorf("%s last run = %v (recorded %v), want %v; runs: %v", tc.patrol, got, ok, tc.at, runs)
			}
			for _, other := range []string{"patrol_scan", "spec_dispatch"} {
				if other == tc.patrol {
					continue
				}
				if got, ok := runs[other]; ok {
					t.Errorf("recording %s also wrote %s = %v; each field keeps its own entry", tc.patrol, other, got)
				}
			}
		})
	}
}

// scanRecorderDaemon is a daemon whose patrol_scan is on and whose town has no
// rigs to scan. bdPath names a path that does not exist, so a tick that uses the
// real host's gate check fails it without starting a tool, and the rogue-bd walk
// is dated to now so it is not due.
func scanRecorderDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true},
		}},
		bdPath: filepath.Join(t.TempDir(), "no-such-bd"),
	}
	d.rogueBDRun.record(d.clk().Now())
	return d
}

// hostWith is the tick's host with every gate check returning err (nil answers).
func hostWith(d *Daemon, err error) *patrolScanHost {
	return &patrolScanHost{d: d, gateCheck: func(string, string) (string, error) { return "", err }}
}

// TestPatrolLastRunScanDispatchRecordsAScan pins gt-xittvy: a runPatrolScan
// whose gate checks answered records patrol_scan in patrol_last_run.json, and
// one whose every check failed records nothing, so gt jobs sees a pass that did
// its work as run and a pass that reached no database as due.
func TestPatrolLastRunScanDispatchRecordsAScan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		gateErr error
		want    bool
	}{
		{"checks answered", nil, true},
		{"every check failed", errors.New("bd gate check: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := scanRecorderDaemon(t)

			before := d.clk().Now()
			d.runPatrolScanOn(hostWith(d, tc.gateErr))
			after := d.clk().Now()

			at, ok, err := loadPatrolLastRun(d.config.TownRoot, "patrol_scan")
			if err != nil {
				t.Fatalf("loadPatrolLastRun: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("patrol_scan recorded a run at %v: recorded = %v, want %v", at, ok, tc.want)
			}
			if ok && (at.Before(before) || at.After(after)) {
				t.Errorf("patrol_scan last run = %v, want a time within the scan (%v..%v)", at, before, after)
			}
		})
	}
}

// TestPatrolLastRunScanDispatchEarlyReturnIsNotARun pins that a runPatrolScan
// that returns before doing the tick's work records nothing: a skipped tick is
// not a run.
func TestPatrolLastRunScanDispatchEarlyReturnIsNotARun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *Config
		patrol *DaemonPatrolConfig
	}{
		{"patrol off", &Config{}, &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: false}}}},
		{"no town config", nil, &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.config != nil {
				tc.config.TownRoot = town
			}
			d := &Daemon{config: tc.config, logger: log.New(io.Discard, "", 0), patrolConfig: tc.patrol}

			d.runPatrolScanOn(hostWith(d, nil))

			if at, ok, err := loadPatrolLastRun(town, "patrol_scan"); err != nil {
				t.Fatalf("loadPatrolLastRun: %v", err)
			} else if ok {
				t.Errorf("an early return recorded patrol_scan = %v", at)
			}
			if at := d.patrolScanRun.last(); !at.IsZero() {
				t.Errorf("an early return recorded patrol_scan in memory = %v", at)
			}
		})
	}
}
