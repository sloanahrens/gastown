package config

import (
	"strings"
	"testing"
	"time"
)

func TestUpdateConfigDefaults(t *testing.T) {
	t.Parallel()

	defaults := UpdateSettings{WindowHour: 3, MinInterval: 3 * time.Hour, MaxRuntime: 2 * time.Hour}
	tests := []struct {
		name string
		c    *UpdateConfig
		want UpdateSettings
	}{
		{"nil config", nil, defaults},
		{"empty config", &UpdateConfig{}, defaults},
		{
			name: "explicit values",
			c:    &UpdateConfig{Enabled: true, Window: "04:30", MinInterval: "6h", MaxRuntime: "1h30m"},
			want: UpdateSettings{Enabled: true, WindowHour: 4, WindowMinute: 30, MinInterval: 6 * time.Hour, MaxRuntime: 90 * time.Minute},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.c.Settings()
			if err != nil {
				t.Fatalf("Settings() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Settings() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUpdateConfigRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		c       *UpdateConfig
		wantKey string
	}{
		{"window hour out of range", &UpdateConfig{Window: "25:00"}, UpdateWindowKey},
		{"window not HH:MM", &UpdateConfig{Window: "3pm"}, UpdateWindowKey},
		{"window signed hour", &UpdateConfig{Window: "+3:00"}, UpdateWindowKey},
		{"window late minute", &UpdateConfig{Window: "03:60"}, UpdateWindowKey},
		{"min_interval below floor", &UpdateConfig{MinInterval: "10m"}, UpdateMinIntervalKey},
		{"min_interval above ceiling", &UpdateConfig{MinInterval: "48h"}, UpdateMinIntervalKey},
		{"min_interval unparsable", &UpdateConfig{MinInterval: "soon"}, UpdateMinIntervalKey},
		{"max_runtime below floor", &UpdateConfig{MaxRuntime: "5m"}, UpdateMaxRuntimeKey},
		{"max_runtime above ceiling", &UpdateConfig{MaxRuntime: "24h"}, UpdateMaxRuntimeKey},
		{"max_runtime unparsable", &UpdateConfig{MaxRuntime: "2 hours"}, UpdateMaxRuntimeKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.c.Settings()
			if err == nil {
				t.Fatalf("Settings() accepted %+v, want an error naming %s", tt.c, tt.wantKey)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("Settings() error = %q, want it to name %s", err, tt.wantKey)
			}
		})
	}
}

func TestUpdateConfigBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    *UpdateConfig
		want UpdateSettings
	}{
		{"earliest window", &UpdateConfig{Window: "00:00"}, UpdateSettings{WindowHour: 0, WindowMinute: 0, MinInterval: 3 * time.Hour, MaxRuntime: 2 * time.Hour}},
		{"latest window", &UpdateConfig{Window: "23:59"}, UpdateSettings{WindowHour: 23, WindowMinute: 59, MinInterval: 3 * time.Hour, MaxRuntime: 2 * time.Hour}},
		{"shortest durations", &UpdateConfig{MinInterval: "30m", MaxRuntime: "10m"}, UpdateSettings{WindowHour: 3, MinInterval: 30 * time.Minute, MaxRuntime: 10 * time.Minute}},
		{"longest durations", &UpdateConfig{MinInterval: "24h", MaxRuntime: "12h"}, UpdateSettings{WindowHour: 3, MinInterval: 24 * time.Hour, MaxRuntime: 12 * time.Hour}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.c.Settings()
			if err != nil {
				t.Fatalf("Settings() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Settings() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
