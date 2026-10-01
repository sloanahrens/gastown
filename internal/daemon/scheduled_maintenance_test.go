package daemon

import (
	"testing"
	"time"
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
