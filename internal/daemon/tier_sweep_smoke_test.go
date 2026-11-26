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
	cfg := runnerTestConfig(t)
	cfg.Shell = true
	d.patrolConfig.Patrols.TierSweep.Runner = cfg
	d.tierSweepSeams.localMainSHA = func(string) (string, error) { return runnerTestSHA, nil }
	return d
}

// TestSweepSmokeLaunch pins the record each launch result gives, that the
// launch runs exactly once over the cheapest stage the runner takes at the
// rig's main tip, and that no wait for a result starts; a launch that cannot
// be rendered is failed with the first line of why and launches nothing
// (gt-d7qez9).
func TestSweepSmokeLaunch(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	for _, tc := range []struct {
		name   string
		setup  func(*Daemon)
		launch tierSweepLaunch
		runs   bool
		tiers  string // the launched stage's tiers, comma-joined
		want   sweepSmokeRecord
	}{
		{"started", nil, tierSweepLaunch{exitCode: 0}, true, "shell", sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"busy", nil, tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}, true, "shell", sweepSmokeRecord{Time: now, Outcome: "busy"}},
		{"declined", nil, tierSweepLaunch{exitCode: -1, declined: true}, true, "shell", sweepSmokeRecord{Time: now, Outcome: "declined"}},
		{"failed", nil, tierSweepLaunch{exitCode: -1, err: errors.New("killed\nmore")}, true, "shell", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "killed"}},
		{"host shell skipped", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.Shell = false }, tierSweepLaunch{exitCode: 0}, true, "integration,race", sweepSmokeRecord{Time: now, Outcome: "started"}},
		{"main unreadable", func(d *Daemon) {
			d.tierSweepSeams.localMainSHA = func(string) (string, error) { return "", errors.New("resolving origin/main: boom") }
		}, tierSweepLaunch{}, false, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "resolving origin/main: boom"}},
		{"runner cannot render", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner.CPUs = 0 }, tierSweepLaunch{}, false, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "sweeprunner: spec.cpus: 0 is outside 1-16"}},
		{"no runner", func(d *Daemon) { d.patrolConfig.Patrols.TierSweep.Runner = nil }, tierSweepLaunch{}, false, "", sweepSmokeRecord{Time: now, Outcome: "failed", Detail: "no runner is configured for the rig's sweep"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newSweepSmokeDaemon(t, now)
			if tc.setup != nil {
				tc.setup(d)
			}
			launches := 0
			var st tierSweepStage
			d.tierSweepSeams.launch = func(_ context.Context, s tierSweepStage, _ sweeprunner.Spec) tierSweepLaunch {
				launches++
				st = s
				if !tc.runs {
					t.Error("a launch ran before anything could be rendered")
				}
				return tc.launch
			}
			d.tierSweepSeams.budgetTimer = func(time.Duration) (<-chan time.Time, func()) {
				t.Error("sweepSmokeLaunch started a wait for a result")
				return runnerBudgetSpent(0)
			}
			if rec := d.sweepSmokeLaunch(context.Background(), "gastown"); rec != tc.want {
				t.Errorf("sweepSmokeLaunch = %+v, want %+v", rec, tc.want)
			}
			if tc.runs && launches != 1 {
				t.Errorf("launches = %d, want one", launches)
			}
			if tc.runs && (strings.Join(st.tiers, ",") != tc.tiers || st.tested != runnerTestSHA) {
				t.Errorf("launched stage %+v, want tiers %q at the main tip", st, tc.tiers)
			}
		})
	}
}

// TestSweepSmokeLaunchStops pins that a launch's teardown runs after it
// returns for a started and a failed launch, and never for a busy one: a busy
// launch created nothing, so its teardown could end a run the smoke did not
// start (gt-d7qez9).
func TestSweepSmokeLaunchStops(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	busy := tierSweepLaunch{exitCode: -1, err: &sweeprunner.ActiveJob{Name: "sweep-gastown-abc"}}
	for _, tc := range []struct {
		name   string
		launch tierSweepLaunch
		stop   bool
		want   int
	}{
		{"started", tierSweepLaunch{exitCode: 0}, true, 1},
		{"failed", tierSweepLaunch{exitCode: -1, err: errors.New("killed")}, true, 1},
		{"busy has no stop", busy, false, 0},
		{"busy keeps its stop", busy, true, 0},
		{"declined", tierSweepLaunch{exitCode: -1, declined: true}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newSweepSmokeDaemon(t, now)
			launched, stops, early := false, 0, false
			d.tierSweepSeams.launch = func(context.Context, tierSweepStage, sweeprunner.Spec) tierSweepLaunch {
				launched = true
				l := tc.launch
				if tc.stop {
					l.stop = func() { stops++; early = !launched }
				}
				return l
			}
			d.sweepSmokeLaunch(context.Background(), "gastown")
			if early || stops != tc.want {
				t.Errorf("stop ran %d times (before the launch: %v), want %d after it", stops, early, tc.want)
			}
		})
	}
}
