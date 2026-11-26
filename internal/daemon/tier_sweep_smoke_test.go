package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// TestClassifySweepSmoke pins what each launch result names: started, busy
// while a sweep Job holds the rig, declined by the capacity preflight, and
// failed with the error's first line only (gt-8tntnn).
func TestClassifySweepSmoke(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		launch tierSweepLaunch
		want   sweepSmokeRecord
	}{
		{"started", tierSweepLaunch{exitCode: 0}, sweepSmokeRecord{Outcome: "started"}},
		{"busy", tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}, sweepSmokeRecord{Outcome: "busy"}},
		{"declined", tierSweepLaunch{exitCode: -1, declined: true}, sweepSmokeRecord{Outcome: "declined"}},
		{"failed keeps the first line", tierSweepLaunch{exitCode: -1, err: errors.New("sync step: signal: killed\nsecond line")}, sweepSmokeRecord{Outcome: "failed", Detail: "sync step: signal: killed"}},
		{"failed with an empty first line keeps no detail", tierSweepLaunch{exitCode: -1, err: errors.New("\nsecond line")}, sweepSmokeRecord{Outcome: "failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifySweepSmoke(tc.launch); got != tc.want {
				t.Errorf("classifySweepSmoke(%+v) = %+v, want %+v", tc.launch, got, tc.want)
			}
		})
	}
}

// TestSweepSmokeLaunch pins the record each launch gives, that one launch runs
// over the first stage the runner takes at the main tip, and that a launch
// which cannot be rendered fails before it launches (gt-d7qez9). The launch
// writes no result, so a smoke that waited for one trips the hang guard.
func TestSweepSmokeLaunch(t *testing.T) { t.Parallel(); testSweepSmokeLaunch(t, false) }

// TestSweepSmokeLaunchStops also pins that the launch's stop runs once a
// non-busy launch returns, and never for a busy one: it created nothing, and
// its stop could end a run the smoke did not start (gt-d7qez9).
func TestSweepSmokeLaunchStops(t *testing.T) { t.Parallel(); testSweepSmokeLaunch(t, true) }

func testSweepSmokeLaunch(t *testing.T, checkStops bool) {
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name      string
		setup     func(*Daemon)
		launch    tierSweepLaunch
		tiers     string
		detailHas string
		stops     int
		want      sweepSmokeRecord
	}{
		{"started", nil, tierSweepLaunch{exitCode: 0}, "shell", "", 1, sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"busy", nil, tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}, "shell", "", 0, sweepSmokeRecord{Time: now, Outcome: "busy"}},
		{"declined", nil, tierSweepLaunch{exitCode: -1, declined: true}, "shell", "", 1, sweepSmokeRecord{Time: now, Outcome: "declined"}},
		{"failed", nil, tierSweepLaunch{exitCode: -1, err: errors.New("killed\nmore")}, "shell", "", 1, sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "killed"}},
		{"host shell skipped", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.Shell = false }, tierSweepLaunch{exitCode: 0}, "integration,race", "", 1, sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"main unreadable", func(d *Daemon) {
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "", errors.New("boom") }
		}, tierSweepLaunch{}, "", "", 0, sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "boom"}},
		{"runner cannot render", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.CPUs = 0 }, tierSweepLaunch{}, "", "cpus", 0, sweepSmokeRecord{Time: now, Outcome: "failed"}},
		{"no runner", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner = nil }, tierSweepLaunch{}, "", "", 0, sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "no runner is configured for the rig's sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A temp-town daemon whose runner takes the shell stage at runnerTestSHA.
			d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
			d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
			d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
			if tc.setup != nil {
				tc.setup(d)
			}
			// A run that waited would read the run's status through a subprocess
			// and write its launch record into the output directory; the smoke does
			// neither, so either one is a failure here (om finding 337a6914fe0f).
			d.execCmd = func(cmd *exec.Cmd) ([]byte, []byte, error) {
				t.Errorf("the smoke ran %v: it only launches and never reads a run's status", cmd.Args)
				return nil, nil, errors.New("unexpected command")
			}
			// A spent budget ends any wait at once, so a smoke that waited returns
			// with its launch record on disk and fails the directory check below
			// instead of hanging; the hang guard stays as the backstop.
			d.tierSweepSeams.budgetTimer = func(time.Duration) (<-chan time.Time, func()) { return runnerBudgetSpent(0) }
			launches, stops, st, outDir := 0, 0, tierSweepStage{}, ""
			d.tierSweepSeams.launch = func(_ context.Context, s tierSweepStage, sp sweeprunner.Spec) tierSweepLaunch {
				launches++
				st, outDir = s, sp.OutDir
				l := tc.launch
				l.stop = func() { stops++ }
				return l
			}
			returned := make(chan sweepSmokeRecord, 1)
			go func() { returned <- d.sweepSmokeLaunch(context.Background(), "gastown") }()
			var rec sweepSmokeRecord
			select {
			case rec = <-returned:
			case <-time.After(10 * time.Second): //testpolicy:allow no-sleep — a hang guard only: the launch writes no result, so a smoke that waits for one fails here rather than hanging the test
				t.Fatal("sweepSmokeLaunch waited for the run's result")
			}
			if rec.Time != tc.want.Time || rec.Outcome != tc.want.Outcome ||
				tc.detailHas == "" && rec.Detail != tc.want.Detail || tc.detailHas != "" && !strings.Contains(rec.Detail, tc.detailHas) {
				t.Errorf("sweepSmokeLaunch = %+v, want %+v (detail contains %q)", rec, tc.want, tc.detailHas)
			}
			if (launches == 1) != (tc.tiers != "") || strings.Join(st.tiers, ",") != tc.tiers || launches == 1 && st.tested != runnerTestSHA {
				t.Errorf("launches = %d, stage %+v; want the launch of tiers %q at the main tip, or none", launches, st, tc.tiers)
			}
			if entries, err := os.ReadDir(outDir); outDir != "" && err == nil && len(entries) > 0 {
				t.Errorf("the output directory %s holds %d entries after the smoke, want none: a launch record means the smoke ran the waiting stage", outDir, len(entries))
			}
			if checkStops && stops != tc.stops {
				t.Errorf("stop ran %d times, want %d", stops, tc.stops)
			}
		})
	}
}

// TestSweepSmokeRecord pins the record the daemon's start writes: with tier_sweep
// active and a runner, one launch of the first stage leaves launch-smoke.json
// with its outcome, the launch time and the daemon's start. With tier_sweep
// disabled, or with no runner, nothing is launched and nothing is written
// (gt-r2497d).
func TestSweepSmokeRecord(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	daemonStart := now.Add(-time.Minute)
	for _, tc := range []struct {
		name     string
		setup    func(*Daemon)
		launches int
		want     *sweepSmokeRecord
	}{
		{"active", nil, 1, &sweepSmokeRecord{Time: now, DaemonStart: daemonStart, Outcome: "started"}},
		{"disabled", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Enabled = false }, 0, nil},
		{"no runner", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner = nil }, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
			d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
			d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
			if tc.setup != nil {
				tc.setup(d)
			}
			launches := 0
			d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				launches++
				return tierSweepLaunch{exitCode: 0}
			}
			d.recordSweepSmokeAtStart(context.Background(), daemonStart)
			if launches != tc.launches {
				t.Errorf("launches = %d, want %d", launches, tc.launches)
			}
			data, err := os.ReadFile(sweepSmokeRecordPath(d.config.TownRoot))
			if tc.want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("record read = %v, want no record file", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read record: %v", err)
			}
			var got sweepSmokeRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode record %s: %v", data, err)
			}
			if !got.Time.Equal(tc.want.Time) || !got.DaemonStart.Equal(tc.want.DaemonStart) ||
				got.Outcome != tc.want.Outcome || got.Detail != tc.want.Detail {
				t.Errorf("record = %+v, want %+v", got, *tc.want)
			}
		})
	}
}

// TestSweepSmokeRecordWriteError pins that a record the daemon cannot write is
// logged and the start goes on: the launch still ran, and the log names the
// failed write (gt-r2497d).
func TestSweepSmokeRecordWriteError(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
	d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
	// The record's directory is a file, so the write cannot create it.
	runtimeDir := filepath.Dir(sweepSmokeRecordPath(d.config.TownRoot))
	if err := os.MkdirAll(filepath.Dir(runtimeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	launches := 0
	d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
		launches++
		return tierSweepLaunch{exitCode: 0}
	}
	d.recordSweepSmokeAtStart(context.Background(), now.Add(-time.Minute))
	if launches != 1 {
		t.Errorf("launches = %d, want 1: a failed record write must not stop the launch", launches)
	}
	if !strings.Contains(logs.String(), "launch smoke record not written") {
		t.Errorf("log = %q, want the failed record write", logs.String())
	}
}
