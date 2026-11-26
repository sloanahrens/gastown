package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
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
// rigs to scan, on the given fake clock. bdPath names a path that does not
// exist, so a tick that uses the real host's gate check fails it without
// starting a tool, and the rogue-bd walk is dated to now so it is not due.
func scanRecorderDaemon(t *testing.T, clock clockwork.Clock) *Daemon {
	t.Helper()
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true},
		}},
		bdPath: filepath.Join(t.TempDir(), "no-such-bd"),
		clock:  clock,
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
			clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 26, 6, 45, 0, 0, time.UTC))
			d := scanRecorderDaemon(t, clock)
			// Each gate check moves the fake clock on, as a real check takes
			// time, so the run's record must be the instant it read, not the
			// instant the tick started.
			host := &patrolScanHost{d: d, gateCheck: func(string, string) (string, error) {
				clock.Advance(40 * time.Second)
				return "", tc.gateErr
			}}
			d.runPatrolScanOn(host)
			want := clock.Now()

			at, ok, err := loadPatrolLastRun(d.config.TownRoot, "patrol_scan")
			if err != nil {
				t.Fatalf("loadPatrolLastRun: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("patrol_scan recorded a run at %v: recorded = %v, want %v", at, ok, tc.want)
			}
			if ok && !at.Equal(want) {
				t.Errorf("patrol_scan last run = %v, want the fake clock's instant %v", at, want)
			}
		})
	}
}

// TestPatrolLastRunScanDispatchRecordsATick pins gt-7f014p: a spec dispatch
// tick that ran its phases and reached its report records spec_dispatch, so gt
// jobs stops reading the row as never_run; a held tick, a failed tick, one whose
// report cannot be read and one that does not run at all record nothing, since
// none of them did the tick's work and a record would suppress the next tick.
func TestPatrolLastRunScanDispatchRecordsATick(t *testing.T) {
	t.Parallel()
	// The held and completed rows differ only in the report's hold field, so
	// the held row failing to record is the hold, not a different report.
	completed := `{}`
	held := `{"hold": "town parked by the operator"}`
	for _, tc := range []struct {
		name      string
		reply     cliReply
		patrolOff bool
		want      bool
	}{
		{"completed tick", cliReply{stdout: string(wireOut(completed))}, false, true},
		{"held tick", cliReply{stdout: string(wireOut(held))}, false, false},
		{"failed tick", cliReply{code: 1, stderr: "dolt: connection refused"}, false, false},
		{"unreadable report", cliReply{stdout: "not a report"}, false, false},
		{"patrol off", cliReply{stdout: string(wireOut(completed))}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := clockwork.NewFakeClockAt(time.Date(2026, 11, 26, 6, 45, 0, 0, time.UTC))
			// The fake CLI moves the clock inside the tick, as a real tick takes
			// time, so the record must be the instant the tick read.
			fake := newFakeCLI(func([]string) cliReply {
				clock.Advance(40 * time.Second)
				return tc.reply
			})
			logger, _ := lineLogger()
			d := specDispatchFake(t, fake, logger)
			d.clock = clock
			if tc.patrolOff {
				d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(false)},
				}}
			}

			d.runSpecDispatch()
			want := clock.Now()

			at, ok, err := loadPatrolLastRun(d.config.TownRoot, "spec_dispatch")
			if err != nil {
				t.Fatalf("loadPatrolLastRun: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("spec_dispatch recorded a run at %v: recorded = %v, want %v", at, ok, tc.want)
			}
			if ok && !at.Equal(want) {
				t.Errorf("spec_dispatch last run = %v, want the fake clock's instant %v", at, want)
			}
			if mem := d.specDispatchRun.last(); mem.IsZero() == tc.want {
				t.Errorf("in-memory last run = %v, want recorded = %v", mem, tc.want)
			}
		})
	}
}

// TestPatrolLastRunScanDispatchEarlyReturnIsNotARun pins that a runPatrolScan
// that returns before doing the tick's work records nothing: a skipped tick is
// not a run. The cancelled-context row is the return inside the rig loop: the
// town registers a rig whose identity bead answers as active, so the tick has
// work in front of it and the cancelled context is what stops it.
func TestPatrolLastRunScanDispatchEarlyReturnIsNotARun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		config *Config
		patrol *DaemonPatrolConfig
		// rigs registers these rigs (name -> beads prefix) as active in the
		// town, so the tick's rig list is not empty; nil leaves it empty.
		rigs map[string]string
		// cancelCtx cancels d.ctx before the tick, so the rig loop's first
		// iteration hits its early return.
		cancelCtx bool
	}{
		{"patrol off", &Config{}, &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: false}}}, nil, false},
		{"no town config", nil, &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true}}}, nil, false},
		{"cancelled context before the first rig", &Config{}, &DaemonPatrolConfig{Patrols: &PatrolsConfig{PatrolScan: &PatrolScanConfig{Enabled: true, Rigs: []string{"alpha"}}}}, map[string]string{"alpha": "al"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.config != nil {
				tc.config.TownRoot = town
			}
			d := &Daemon{config: tc.config, logger: log.New(io.Discard, "", 0), patrolConfig: tc.patrol}
			if len(tc.rigs) > 0 {
				registerTestRigs(t, town, tc.rigs)
				d.rigBeadShowFn = func(_, id string) (*beads.Issue, error) {
					return &beads.Issue{ID: id}, nil
				}
			}
			if tc.cancelCtx {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				d.ctx = ctx
				// The ctx return is only reachable when the rig survives the
				// operational filter; without this the row would pass on an
				// empty rig list.
				if rigs := d.getPatrolRigs("patrol_scan"); !slices.Equal(rigs, []string{"alpha"}) {
					t.Fatalf("fixture: getPatrolRigs = %v, want [alpha]", rigs)
				}
			}

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
