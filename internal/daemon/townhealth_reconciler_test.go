package daemon

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestHealthTicksReconcilerEnabled: with the reconciler patrol enabled, Ticks
// lists a reconciler tick carrying its configured interval and the last run the
// record holds, so the town health judges a stalled reconciler (gt-v71maj).
func TestHealthTicksReconcilerEnabled(t *testing.T) {
	t.Parallel()

	lastRun := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		interval     string
		recordedAt   time.Time
		wantInterval time.Duration
		wantLastRun  time.Time
	}{
		{"a configured interval is the tick's interval", "10m", lastRun, 10 * time.Minute, lastRun},
		{"an unset interval reads the default", "", lastRun, config.DefaultReconcilerInterval, lastRun},
		{"no run recorded reads a zero last run", "10m", time.Time{}, 10 * time.Minute, time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if !tt.recordedAt.IsZero() {
				if err := savePatrolLastRun(root, "reconciler", tt.recordedAt); err != nil {
					t.Fatalf("savePatrolLastRun: %v", err)
				}
			}
			d := &Daemon{
				config: &Config{TownRoot: root},
				logger: log.New(io.Discard, "", 0),
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					Reconciler: &config.ReconcilerConfig{Enabled: true, Interval: tt.interval},
				}},
			}

			ticks, err := (&healthSources{d: d}).Ticks()
			if err != nil {
				t.Fatalf("Ticks: %v", err)
			}
			var got *townhealth.Tick
			for i := range ticks {
				if ticks[i].Name == "reconciler" {
					got = &ticks[i]
				}
			}
			if got == nil {
				t.Fatalf("Ticks = %+v, want a reconciler tick listed while the patrol is enabled", ticks)
			}
			if got.Err != nil {
				t.Errorf("reconciler tick Err = %v, want none", got.Err)
			}
			if got.Interval != tt.wantInterval {
				t.Errorf("reconciler tick Interval = %v, want %v", got.Interval, tt.wantInterval)
			}
			if !got.LastFired.Equal(tt.wantLastRun) {
				t.Errorf("reconciler tick LastFired = %v, want %v", got.LastFired, tt.wantLastRun)
			}
		})
	}
}

// TestHealthTicksReconcilerDisabled: a town that has not turned the reconciler
// on lists no reconciler tick, so the town health never reports it red for a
// patrol that is not running (gt-39od6h).
func TestHealthTicksReconcilerDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *DaemonPatrolConfig
	}{
		{"a disabled reconciler block lists no tick", &DaemonPatrolConfig{Patrols: &PatrolsConfig{Reconciler: &config.ReconcilerConfig{Enabled: false}}}},
		{"an absent reconciler block lists no tick", &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{
				config:       &Config{TownRoot: t.TempDir()},
				logger:       log.New(io.Discard, "", 0),
				patrolConfig: tt.cfg,
			}

			ticks, err := (&healthSources{d: d}).Ticks()
			if err != nil {
				t.Fatalf("Ticks: %v", err)
			}
			for _, tick := range ticks {
				if tick.Name == "reconciler" {
					t.Fatalf("Ticks listed a reconciler tick while the patrol is off: %+v", tick)
				}
			}
		})
	}
}

// TestHealthTicksReconcilerStale: a reconciler whose recorded last run is three
// of its intervals old reads DEGRADED, with the same wording a stalled
// jsonl_git_backup or wisp_reaper tick gets, so a reconciler that stopped firing
// is judged stale like every other tick (gt-r3tm7k).
func TestHealthTicksReconcilerStale(t *testing.T) {
	t.Parallel()

	// Each interval is spelled once, here: the config, the recorded last run and
	// the expected wording all follow from it. ago is the three-interval age.
	const (
		reconcilerEvery = "10m"
		jsonlEvery      = "15m"
		wispEvery       = "1h"
	)
	rows := []struct{ name, every, ago string }{
		{"reconciler", reconcilerEvery, "30m"},
		{"jsonl_git_backup", jsonlEvery, "45m"},
		{"wisp_reaper", wispEvery, "3h"},
	}

	now := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)
	root := t.TempDir()
	for _, row := range rows {
		every, err := time.ParseDuration(row.every)
		if err != nil {
			t.Fatalf("ParseDuration(%s): %v", row.every, err)
		}
		if err := savePatrolLastRun(root, row.name, now.Add(-3*every)); err != nil {
			t.Fatalf("savePatrolLastRun(%s): %v", row.name, err)
		}
	}
	d := &Daemon{
		config: &Config{TownRoot: root},
		logger: log.New(io.Discard, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			Reconciler:     &config.ReconcilerConfig{Enabled: true, Interval: reconcilerEvery},
			JsonlGitBackup: &config.JsonlGitBackupConfig{Enabled: true, IntervalStr: jsonlEvery},
			WispReaper:     &config.WispReaperConfig{Enabled: true, IntervalStr: wispEvery},
		}},
	}

	r := townhealth.Compute(context.Background(), townhealth.Inputs{
		Now: now, Thresholds: townhealth.DefaultThresholds(), Ticks: &healthSources{d: d},
	})

	for _, row := range rows {
		got := healthField(t, r, "tick:"+row.name)
		value := row.ago + "/" + row.every
		detail := "last fired " + row.ago + " ago, interval " + row.every
		if got.Tag != townhealth.Recorded || got.Verdict != townhealth.Degraded ||
			got.Value != value || got.Detail != detail {
			t.Errorf("tick:%s = %s %s %q %q, want %s %s %q %q",
				row.name, got.Tag, got.Verdict, got.Value, got.Detail,
				townhealth.Recorded, townhealth.Degraded, value, detail)
		}
	}
}
