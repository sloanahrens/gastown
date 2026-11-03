package daemon

import (
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

func TestParseWindowTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input      string
		wantHour   int
		wantMinute int
		wantErr    bool
	}{
		{"03:00", 3, 0, false},
		{"00:00", 0, 0, false},
		{"23:59", 23, 59, false},
		{"12:30", 12, 30, false},
		{"3:00", 3, 0, false},
		// Invalid
		{"24:00", 0, 0, true},
		{"12:60", 0, 0, true},
		{"-1:00", 0, 0, true},
		{"abc", 0, 0, true},
		{"", 0, 0, true},
		{"12", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			hour, minute, err := parseWindowTime(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseWindowTime(%q) expected error, got hour=%d minute=%d", tt.input, hour, minute)
				}
				return
			}
			if err != nil {
				t.Errorf("parseWindowTime(%q) unexpected error: %v", tt.input, err)
				return
			}
			if hour != tt.wantHour || minute != tt.wantMinute {
				t.Errorf("parseWindowTime(%q) = (%d, %d), want (%d, %d)", tt.input, hour, minute, tt.wantHour, tt.wantMinute)
			}
		})
	}
}

// maintenanceWindowEnd is the first instant isInMaintenanceWindow reports
// false: the deferral streak and the window share one length.
func TestMaintenanceWindowEndMatchesWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 2, 28, 3, 20, 0, 0, time.Local)
	end := maintenanceWindowEnd(now, "03:00")
	if want := time.Date(2026, 2, 28, 3, 0, 0, 0, time.Local).Add(maintenanceWindowLength); !end.Equal(want) {
		t.Fatalf("maintenanceWindowEnd = %v, want %v", end, want)
	}
	if !isInMaintenanceWindow(end.Add(-time.Nanosecond), "03:00") {
		t.Error("isInMaintenanceWindow is false just before maintenanceWindowEnd")
	}
	if isInMaintenanceWindow(end, "03:00") {
		t.Error("isInMaintenanceWindow is true at maintenanceWindowEnd")
	}

	// A window that starts before midnight closes after it: the end is the next
	// day, and the last instant before it is still in the window.
	late := time.Date(2026, 2, 28, 23, 50, 0, 0, time.Local)
	lateEnd := maintenanceWindowEnd(late, "23:45")
	if want := time.Date(2026, 3, 1, 0, 45, 0, 0, time.Local); !lateEnd.Equal(want) {
		t.Fatalf("maintenanceWindowEnd (23:45) = %v, want %v", lateEnd, want)
	}
	if !isInMaintenanceWindow(lateEnd.Add(-time.Nanosecond), "23:45") {
		t.Error("isInMaintenanceWindow (23:45) is false just before maintenanceWindowEnd")
	}
	if isInMaintenanceWindow(lateEnd, "23:45") {
		t.Error("isInMaintenanceWindow (23:45) is true at maintenanceWindowEnd")
	}
	// After midnight the end is still the one the window opened with.
	if got := maintenanceWindowEnd(time.Date(2026, 3, 1, 0, 10, 0, 0, time.Local), "23:45"); !got.Equal(lateEnd) {
		t.Errorf("maintenanceWindowEnd (23:45) after midnight = %v, want %v", got, lateEnd)
	}
}

func TestIsInMaintenanceWindow(t *testing.T) {
	t.Parallel()
	loc := time.Local

	tests := []struct {
		name   string
		now    time.Time
		window string
		want   bool
	}{
		{
			name:   "exactly at window start",
			now:    time.Date(2026, 2, 28, 3, 0, 0, 0, loc),
			window: "03:00",
			want:   true,
		},
		{
			name:   "during window",
			now:    time.Date(2026, 2, 28, 3, 30, 0, 0, loc),
			window: "03:00",
			want:   true,
		},
		{
			name:   "just before window end",
			now:    time.Date(2026, 2, 28, 3, 59, 59, 0, loc),
			window: "03:00",
			want:   true,
		},
		{
			name:   "at window end (1 hour later)",
			now:    time.Date(2026, 2, 28, 4, 0, 0, 0, loc),
			window: "03:00",
			want:   false,
		},
		{
			name:   "before window",
			now:    time.Date(2026, 2, 28, 2, 59, 0, 0, loc),
			window: "03:00",
			want:   false,
		},
		{
			name:   "much later",
			now:    time.Date(2026, 2, 28, 15, 0, 0, 0, loc),
			window: "03:00",
			want:   false,
		},
		{
			name:   "midnight window",
			now:    time.Date(2026, 2, 28, 0, 15, 0, 0, loc),
			window: "00:00",
			want:   true,
		},
		{
			name:   "window from the previous evening still open after midnight",
			now:    time.Date(2026, 3, 1, 0, 10, 0, 0, loc),
			window: "23:30",
			want:   true,
		},
		{
			name:   "window from the previous evening closed",
			now:    time.Date(2026, 3, 1, 0, 30, 0, 0, loc),
			window: "23:30",
			want:   false,
		},
		{
			name:   "late window open before midnight",
			now:    time.Date(2026, 2, 28, 23, 45, 0, 0, loc),
			window: "23:30",
			want:   true,
		},
		{
			name:   "invalid window",
			now:    time.Date(2026, 2, 28, 3, 0, 0, 0, loc),
			window: "bad",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isInMaintenanceWindow(tt.now, tt.window)
			if got != tt.want {
				t.Errorf("isInMaintenanceWindow(%v, %q) = %v, want %v", tt.now, tt.window, got, tt.want)
			}
		})
	}
}

func TestShouldRunMaintenanceCycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 2, 28, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		lastRun time.Time
		want    bool
	}{
		{"never run before", time.Time{}, true},
		{"ran in yesterday's window", now.Add(-24*time.Hour + 5*time.Minute), true},
		{"ran 10 hours ago", now.Add(-10 * time.Hour), false},
		{"ran earlier in this window", now.Add(-30 * time.Minute), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRunMaintenanceCycle(now, tt.lastRun); got != tt.want {
				t.Errorf("shouldRunMaintenanceCycle(now, %v) = %v, want %v", tt.lastRun, got, tt.want)
			}
		})
	}
}

func TestMaintenanceWindow(t *testing.T) {
	t.Parallel()
	// Nil config returns empty
	if got := maintenanceWindow(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}

	// Configured window
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: true,
				Window:  "03:00",
			},
		},
	}
	if got := maintenanceWindow(config); got != "03:00" {
		t.Errorf("expected 03:00, got %q", got)
	}
}

func TestIsPatrolEnabledScheduledMaintenance(t *testing.T) {
	t.Parallel()
	// Nil config — disabled (opt-in)
	if IsPatrolEnabled(nil, "scheduled_maintenance") {
		t.Error("expected scheduled_maintenance disabled with nil config")
	}

	// Explicitly disabled
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: false,
			},
		},
	}
	if IsPatrolEnabled(config, "scheduled_maintenance") {
		t.Error("expected scheduled_maintenance disabled when Enabled=false")
	}

	// Enabled
	config.Patrols.ScheduledMaintenance.Enabled = true
	if !IsPatrolEnabled(config, "scheduled_maintenance") {
		t.Error("expected scheduled_maintenance enabled when Enabled=true")
	}
}

// updateOnWindow is a 03:00 maintenance window with the update trigger on: the
// update job owns the nightly cycle.
func updateOnWindow(window string) *DaemonPatrolConfig {
	cfg := maintenanceWindowAt(window)
	cfg.Patrols.Update = &agentconfig.UpdateConfig{Enabled: true}
	return cfg
}

// TestMaintenanceWindowTickDefersWhenUpdateEnabled: the window tick starts no
// cycle of its own once the update trigger is on; a B11 request still runs one,
// and the feature off leaves the tick starting the cycle as before.
func TestMaintenanceWindowTickDefersWhenUpdateEnabled(t *testing.T) {
	t.Parallel()

	t.Run("no request starts no cycle", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow)
		d.patrolConfig = updateOnWindow("03:00")

		d.runScheduledMaintenance()

		if len(f.gcCalls) != 0 {
			t.Errorf("gc calls = %v with the update trigger on and no request, want none", f.gcCalls)
		}
		if d.maintenanceGCRunning.Load() {
			t.Error("the tick left a cycle running")
		}
	})

	t.Run("a request still runs the cycle", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow)
		d.patrolConfig = updateOnWindow("03:00")
		writeRequestFile(t, d, "run-update", gcTestNow)

		requestTickAndCycle(t, d)

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Errorf("gc calls = %q, want hq for the request", got)
		}
		if res := readRequestResult(t, d); res.RunID != "run-update" || res.Outcome != "completed" {
			t.Errorf("result = %+v, want run-update completed", res)
		}
	})

	t.Run("the feature off starts the cycle as before", func(t *testing.T) {
		t.Parallel()
		d, f := requestDaemon(t, gcTestNow)
		d.patrolConfig = maintenanceWindowAt("03:00")
		d.patrolConfig.Patrols.Update = &agentconfig.UpdateConfig{Enabled: false}

		requestTickAndCycle(t, d)

		if got := strings.Join(f.gcCalls, ","); got != "hq" {
			t.Errorf("gc calls = %q with the update trigger off, want hq", got)
		}
	})
}

// TestCatchUpPathsIgnoreTheFeatureSwitch: a missed night is still caught up
// outside the window with the update trigger on. The catch-ups are the safety
// net for a missed window, not a scheduled cycle the update job owns.
func TestCatchUpPathsIgnoreTheFeatureSwitch(t *testing.T) {
	t.Parallel()
	d, f := catchUpDaemon(t)
	d.patrolConfig = updateOnWindow("03:00")

	d.runScheduledMaintenance()

	if len(f.backupCalls) != 2 {
		t.Fatalf("backup calls = %v with the update trigger on, want gt and hq", f.backupCalls)
	}
	if len(f.gcCalls) != 2 {
		t.Fatalf("gc calls = %v, want the catch-up gc for gt and hq", f.gcCalls)
	}
}
