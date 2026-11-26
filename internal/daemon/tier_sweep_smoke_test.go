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

// newSweepSmokeDaemon is a temp-town daemon whose runner takes the shell stage
// with runnerTestSHA as the rig's origin/main tip.
func newSweepSmokeDaemon(t *testing.T, now time.Time) *Daemon {
	t.Helper()
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.patrolConfig.Patrols.TierSweep.Runner = runnerTestConfig(t)
	d.patrolConfig.Patrols.TierSweep.Runner.Shell = true
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
	return d
}

// TestSweepSmokeLaunch pins the record each launch gives, that one launch runs
// over the first stage the runner takes at the main tip, that no wait for a
// result starts, and that a launch which cannot be rendered fails before it
// launches (gt-d7qez9).
func TestSweepSmokeLaunch(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name   string
		setup  func(*Daemon)
		launch tierSweepLaunch
		tiers  string
		want   sweepSmokeRecord
	}{
		{"started", nil, tierSweepLaunch{exitCode: 0}, "shell", sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"busy", nil, tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}, "shell", sweepSmokeRecord{Time: now, Outcome: "busy"}},
		{"declined", nil, tierSweepLaunch{exitCode: -1, declined: true}, "shell", sweepSmokeRecord{Time: now, Outcome: "declined"}},
		{"failed", nil, tierSweepLaunch{exitCode: -1, err: errors.New("killed\nmore")}, "shell", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "killed"}},
		{"host shell skipped", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.Shell = false }, tierSweepLaunch{exitCode: 0}, "integration,race", sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"main unreadable", func(d *Daemon) {
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "", errors.New("boom") }
		}, tierSweepLaunch{}, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "boom"}},
		{"runner cannot render", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.CPUs = 0 }, tierSweepLaunch{}, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "sweeprunner: spec.cpus: 0 is outside 1-16"}},
		{"no runner", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner = nil }, tierSweepLaunch{}, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "no runner is configured for the rig's sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newSweepSmokeDaemon(t, now)
			if tc.setup != nil {
				tc.setup(d)
			}
			launches, st := 0, tierSweepStage{}
			d.tierSweepSeams.launch = func(_ context.Context, s tierSweepStage, _ sweeprunner.Spec) tierSweepLaunch {
				launches++
				st = s
				return tc.launch
			}
			d.tierSweepSeams.budgetTimer = func(time.Duration) (<-chan time.Time, func()) {
				t.Error("waited for a result")
				return runnerBudgetSpent(0)
			}
			if rec := d.sweepSmokeLaunch(context.Background(), "gastown"); rec != tc.want {
				t.Errorf("sweepSmokeLaunch = %+v, want %+v", rec, tc.want)
			}
			if (launches == 1) != (tc.tiers != "") || strings.Join(st.tiers, ",") != tc.tiers || launches == 1 && st.tested != runnerTestSHA {
				t.Errorf("launches = %d, stage %+v; want the launch of tiers %q at the main tip, or none", launches, st, tc.tiers)
			}
		})
	}
}

// TestSweepSmokeLaunchStops pins that the launch's stop runs once a started or
// failed launch returns, and never for a busy one: it created nothing, and its
// stop could end a run the smoke did not start (gt-d7qez9).
func TestSweepSmokeLaunchStops(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name   string
		launch tierSweepLaunch
		want   int
	}{
		{"started", tierSweepLaunch{exitCode: 0}, 1},
		{"failed", tierSweepLaunch{exitCode: -1, err: errors.New("killed")}, 1},
		{"busy", tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newSweepSmokeDaemon(t, now)
			stops := 0
			d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				l := tc.launch
				l.stop = func() { stops++ }
				return l
			}
			d.sweepSmokeLaunch(context.Background(), "gastown")
			if stops != tc.want {
				t.Errorf("stop ran %d times, want %d", stops, tc.want)
			}
		})
	}
}
