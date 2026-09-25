package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestParseWindowTime(t *testing.T) {
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
}

func TestIsInMaintenanceWindow(t *testing.T) {
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

func TestShouldRunMaintenance(t *testing.T) {
	now := time.Date(2026, 2, 28, 3, 0, 0, 0, time.Local)

	tests := []struct {
		name     string
		lastRun  time.Time
		interval string
		want     bool
	}{
		{
			name:     "never run before",
			lastRun:  time.Time{},
			interval: "daily",
			want:     true,
		},
		{
			name:     "daily - ran 25 hours ago",
			lastRun:  now.Add(-25 * time.Hour),
			interval: "daily",
			want:     true,
		},
		{
			name:     "daily - ran 10 hours ago",
			lastRun:  now.Add(-10 * time.Hour),
			interval: "daily",
			want:     false,
		},
		{
			name:     "weekly - ran 7 days ago",
			lastRun:  now.Add(-7 * 24 * time.Hour),
			interval: "weekly",
			want:     true,
		},
		{
			name:     "weekly - ran 3 days ago",
			lastRun:  now.Add(-3 * 24 * time.Hour),
			interval: "weekly",
			want:     false,
		},
		{
			name:     "monthly - ran 30 days ago",
			lastRun:  now.Add(-30 * 24 * time.Hour),
			interval: "monthly",
			want:     true,
		},
		{
			name:     "monthly - ran 10 days ago",
			lastRun:  now.Add(-10 * 24 * time.Hour),
			interval: "monthly",
			want:     false,
		},
		{
			name:     "custom duration 48h - ran 50h ago",
			lastRun:  now.Add(-50 * time.Hour),
			interval: "48h",
			want:     true,
		},
		{
			name:     "custom duration 48h - ran 30h ago",
			lastRun:  now.Add(-30 * time.Hour),
			interval: "48h",
			want:     false,
		},
		{
			name:     "invalid interval - falls back to daily",
			lastRun:  now.Add(-25 * time.Hour),
			interval: "nope",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRunMaintenance(now, tt.lastRun, tt.interval)
			if got != tt.want {
				t.Errorf("shouldRunMaintenance(now, %v, %q) = %v, want %v", tt.lastRun, tt.interval, got, tt.want)
			}
		})
	}
}

func TestMaintenanceThreshold(t *testing.T) {
	// Nil config returns default
	if got := maintenanceThreshold(nil); got != defaultMaintenanceThreshold {
		t.Errorf("expected default %d, got %d", defaultMaintenanceThreshold, got)
	}

	// Configured threshold
	threshold := 500
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:   true,
				Threshold: &threshold,
			},
		},
	}
	if got := maintenanceThreshold(config); got != 500 {
		t.Errorf("expected 500, got %d", got)
	}
}

func TestMaintenanceWindow(t *testing.T) {
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

func TestMaintenanceInterval(t *testing.T) {
	// Nil config returns "daily"
	if got := maintenanceInterval(nil); got != "daily" {
		t.Errorf("expected daily, got %q", got)
	}

	// Configured interval
	config := &DaemonPatrolConfig{
		Patrols: &PatrolsConfig{
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:  true,
				Interval: "weekly",
			},
		},
	}
	if got := maintenanceInterval(config); got != "weekly" {
		t.Errorf("expected weekly, got %q", got)
	}

	// Empty interval returns default
	config.Patrols.ScheduledMaintenance.Interval = ""
	if got := maintenanceInterval(config); got != "daily" {
		t.Errorf("expected daily for empty, got %q", got)
	}
}

func TestIsPatrolEnabledScheduledMaintenance(t *testing.T) {
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

func TestMaintenanceMode(t *testing.T) {
	withMode := func(mode string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{
				ScheduledMaintenance: &ScheduledMaintenanceConfig{Enabled: true, Mode: mode},
			},
		}
	}

	tests := []struct {
		name   string
		config *DaemonPatrolConfig
		want   string
	}{
		{"nil config defaults to monitor", nil, MaintenanceModeMonitor},
		{"empty config defaults to monitor", &DaemonPatrolConfig{}, MaintenanceModeMonitor},
		{"empty mode defaults to monitor", withMode(""), MaintenanceModeMonitor},
		{"explicit monitor", withMode("monitor"), MaintenanceModeMonitor},
		{"explicit flatten", withMode("flatten"), MaintenanceModeFlatten},
		{"flatten is matched case-insensitively", withMode("FLATTEN"), MaintenanceModeFlatten},
		{"flatten tolerates surrounding space", withMode("  flatten  "), MaintenanceModeFlatten},
		// The load-bearing cases: only the exact word arms the destructive
		// path. A typo at 03:00 must escalate, not rewrite every database.
		{"typo stays monitor", withMode("flaten"), MaintenanceModeMonitor},
		{"unknown mode stays monitor", withMode("compact"), MaintenanceModeMonitor},
		{"monitor with trailing junk stays monitor", withMode("monitor flatten"), MaintenanceModeMonitor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maintenanceMode(tt.config); got != tt.want {
				t.Errorf("maintenanceMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMaintenanceMonitorMessage(t *testing.T) {
	targets := []maintenanceTarget{
		{name: "gastown", commits: 1524},
		{name: "hq", commits: 1204},
	}
	msg := maintenanceMonitorMessage(targets, 1000)

	// Every over-threshold database must be named with its count: the
	// escalation is the only thing a human sees, and "something is over
	// threshold" is not actionable without the numbers.
	for _, want := range []string{"gastown", "1524", "hq", "1204", "1000", MaintenanceModeMonitor} {
		if !strings.Contains(msg, want) {
			t.Errorf("monitor message missing %q:\n%s", want, msg)
		}
	}
	// And it must say the thing that matters: nothing was rewritten.
	if !strings.Contains(msg, "Nothing was rewritten") {
		t.Errorf("monitor message does not state that nothing was rewritten:\n%s", msg)
	}

	single := maintenanceMonitorMessage([]maintenanceTarget{{name: "om", commits: 7}}, 5)
	if !strings.Contains(single, "om") || !strings.Contains(single, "7") {
		t.Errorf("single-target message missing name or count:\n%s", single)
	}
}

func TestTailLines(t *testing.T) {
	tests := []struct {
		name   string
		output string
		n      int
		want   []string
	}{
		{"empty output", "", 5, nil},
		{"whitespace only", "\n\n  \n", 5, nil},
		{"fewer lines than n", "a\nb", 5, []string{"a", "b"}},
		{"exactly n", "a\nb", 2, []string{"a", "b"}},
		{"more lines than n keeps the tail", "a\nb\nc\nd", 2, []string{"c", "d"}},
		{"trailing newline is not a line", "a\nb\n", 5, []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tailLines([]byte(tt.output), tt.n)
			if len(got) != len(tt.want) {
				t.Fatalf("tailLines(%q, %d) = %q, want %q", tt.output, tt.n, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("tailLines(%q, %d) = %q, want %q", tt.output, tt.n, got, tt.want)
				}
			}
		})
	}
}
