package daemon

import (
	"context"
	"errors"
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
			launches, stops, st := 0, 0, tierSweepStage{}
			d.tierSweepSeams.launch = func(_ context.Context, s tierSweepStage, _ sweeprunner.Spec) tierSweepLaunch {
				launches++
				st = s
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
			if checkStops && stops != tc.stops {
				t.Errorf("stop ran %d times, want %d", stops, tc.stops)
			}
		})
	}
}
