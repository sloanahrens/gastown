package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			var launched sweeprunner.Spec
			d.tierSweepSeams.launch = func(_ context.Context, s tierSweepStage, sp sweeprunner.Spec) tierSweepLaunch {
				launches++
				st, outDir, launched = s, sp.OutDir, sp
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
			if launches == 1 && (launched.RunID != tierSweepRunnerRunID(st.rig, st.tested, st.tiers)+"-smoke" ||
				filepath.Base(launched.OutDir) != launched.RunID) {
				t.Errorf("smoke launched run %q writing to %q, want a run of its own named beside the sweep's", launched.RunID, launched.OutDir)
			}
			if checkStops && stops != tc.stops {
				t.Errorf("stop ran %d times, want %d", stops, tc.stops)
			}
		})
	}
}

// TestSweepSmokeLeavesASweepsRunAlone: the smoke's run has a name of its own, so
// a sweep Job of the same rig, commit and tiers that is running is a foreign Job
// to it. The claim guard refuses it as busy and the smoke removes nothing, where
// a shared run id let the launch adopt that Job and its stop take the running
// sweep down (gt-9ieuas).
func TestSweepSmokeLeavesASweepsRunAlone(t *testing.T) {
	t.Parallel()
	g := newGuardFixture(t, atHour(time.Now(), 15))
	g.d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
	running := sweeprunner.JobName(g.spec)
	g.cluster.jobs = running + "\n"

	rec := g.d.sweepSmokeLaunch(context.Background(), "gastown")

	if rec.Outcome != "busy" {
		t.Errorf("smoke outcome = %q, want busy: a running sweep of this rig must be a foreign Job", rec.Outcome)
	}
	for _, ran := range g.cmds.ran {
		if strings.Contains(ran, " delete ") {
			t.Errorf("the smoke ran %q, want no delete: it must leave the running sweep alone", ran)
		}
	}
}

// TestSweepSmokeSparesAnInterruptedSweep: a sweep mark younger than the run
// budget is an interrupted cycle whose finished Job's output claims wait for the
// resumed cycle. The smoke records busy naming the commit and launches nothing;
// no mark, an old one, or an unreadable one (logged) launches as before (gt-rp65v0).
func TestSweepSmokeSparesAnInterruptedSweep(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name     string
		started  time.Time
		unread   bool
		launches int
		outcome  string
	}{
		{"young mark", now.Add(-time.Minute), false, 0, "busy"},
		{"no mark", time.Time{}, false, 1, "started"},
		{"mark at the budget", now.Add(-tierSweepRunBudget), false, 1, "started"},
		{"unreadable mark", time.Time{}, true, 1, "started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
			d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
			d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
			switch {
			case tc.unread:
				plantMark(t, tierSweepInFlightPath(d.config.TownRoot, "gastown"), "{not json", false)
			case !tc.started.IsZero():
				writeErr := writeTierSweepInFlight(d.config.TownRoot, "gastown", tierSweepInFlightMark{SHA: "abc123", Started: tc.started})
				if writeErr != nil {
					t.Fatalf("write mark: %v", writeErr)
				}
			}
			launches := 0
			d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				launches++
				return tierSweepLaunch{exitCode: 0}
			}
			rec := d.sweepSmokeLaunch(context.Background(), "gastown")
			named := tc.outcome == "busy" && strings.Contains(rec.Detail, "abc123")
			if launches != tc.launches || !rec.Time.Equal(now) || rec.Outcome != tc.outcome || tc.outcome == "busy" && !named {
				t.Errorf("sweepSmokeLaunch = %+v after %d launches, want %s with the marked commit named when it spared the sweep (%q)",
					rec, launches, tc.outcome, rec.Detail)
			}
			if logged := strings.Contains(logs.String(), "mark not read"); logged != tc.unread {
				t.Errorf("log = %q, want the unreadable mark named: %v", logs.String(), tc.unread)
			}
		})
	}
}

// TestSweepSmokeHoldsTheSweepGuard: the smoke holds the cycle's single-flight
// guard from before its launch to after its stop. A cycle holding it is not run
// beside: nothing launches, the record says busy naming the cycle, guard held.
func TestSweepSmokeHoldsTheSweepGuard(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	for _, cycle := range []bool{true, false} {
		t.Run(fmt.Sprintf("a cycle holds the guard: %v", cycle), func(t *testing.T) {
			t.Parallel()
			g := newGuardFixture(t, now)
			g.d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
			g.d.tierSweepRunning.Store(cycle)
			launches, held := 0, [2]bool{}
			g.d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				launches++
				held[0] = g.d.tierSweepRunning.Load() && g.d.tierSweepStartedAt.Load() != 0
				return tierSweepLaunch{exitCode: 0, stop: func() { held[1] = g.d.tierSweepRunning.Load() }}
			}
			g.d.recordSweepSmokeAtStart(context.Background(), now.Add(-time.Minute))
			data, err := os.ReadFile(sweepSmokeRecordPath(g.d.config.TownRoot))
			if err != nil {
				t.Fatalf("read record: %v", err)
			}
			var got sweepSmokeRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode record %s: %v", data, err)
			}
			if (got.Outcome == "busy") != cycle || (launches == 0) != cycle ||
				strings.Contains(got.Detail, "holds the guard") != cycle ||
				g.d.tierSweepRunning.Load() != cycle || !cycle && !(held[0] && held[1]) {
				t.Errorf("record %+v after %d launches, guard %v, held %v; want busy with the cycle named and its guard held, else started holding it through the stop",
					got, launches, g.d.tierSweepRunning.Load(), held)
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

// TestRecordSweepSmokeAtStartIsAwaited: the start-time smoke launch runs on a
// tracked goroutine, so waitForSweepSmoke does not return while the launch is
// blocked and returns once it is released, with the record written; a launch
// still blocked at the bound is named and left behind (gt-fgglou).
func TestRecordSweepSmokeAtStartIsAwaited(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	daemonStart := now.Add(-time.Minute)
	// aBlockedSmoke is a daemon whose smoke launch signals launchStarted, then
	// blocks until release is closed.
	aBlockedSmoke := func(t *testing.T) (*Daemon, *bytes.Buffer, chan struct{}, chan struct{}) {
		t.Helper()
		d, _, _, logs := newTierSweepDaemon(t, now, "gastown")
		d.ctx = context.Background()
		d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
		d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
		d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
		release := make(chan struct{})
		launchStarted := make(chan struct{})
		d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
			close(launchStarted)
			<-release
			return tierSweepLaunch{exitCode: 0}
		}
		return d, logs, release, launchStarted
	}

	t.Run("the wait joins the launch", func(t *testing.T) {
		t.Parallel()
		d, _, release, launchStarted := aBlockedSmoke(t)
		// The bound is beyond the test, so only the launch finishing returns it.
		d.sweepSmokeWait = signalWait
		d.startSweepSmoke(daemonStart)
		reached(t, "the smoke launch", launchStarted)

		begun := make(chan struct{})
		d.sweepSmokeWaitBegun = func() { close(begun) }
		waited := make(chan struct{})
		go func() {
			d.waitForSweepSmoke()
			close(waited)
		}()
		reached(t, "the wait", begun)
		select {
		case <-waited:
			t.Fatal("waitForSweepSmoke returned while the launch was blocked")
		default:
		}
		close(release)
		within(t, signalWait, "the wait after the release", waited)

		data, err := os.ReadFile(sweepSmokeRecordPath(d.config.TownRoot))
		if err != nil {
			t.Fatalf("read record after the wait returned: %v", err)
		}
		var got sweepSmokeRecord
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("decode record %s: %v", data, err)
		}
		if got.Outcome != "started" || !got.DaemonStart.Equal(daemonStart) {
			t.Errorf("record after the wait = %+v, want started stamped with the daemon's start %v", got, daemonStart)
		}
	})

	t.Run("a launch past the bound is logged and left behind", func(t *testing.T) {
		t.Parallel()
		d, logs, release, launchStarted := aBlockedSmoke(t)
		d.sweepSmokeWait = 20 * time.Millisecond
		d.startSweepSmoke(daemonStart)
		reached(t, "the smoke launch", launchStarted)

		d.waitForSweepSmoke()
		if !strings.Contains(logs.String(), "still running 20ms after the wait") {
			t.Errorf("waitForSweepSmoke returned without naming the launch it left behind:\n%s", logs.String())
		}
		// Release and join so the record write finishes inside the test's town.
		close(release)
		d.sweepSmokeWait = signalWait
		d.waitForSweepSmoke()
	})
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
