package rehearsal

import (
	"strings"
	"testing"
	"time"
)

func TestRehearsalSweepLine(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 11, 27, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		name        string
		line        string
		start, end  time.Time
		ok, wantErr bool
	}{
		{"a readable swept line gives its run", "2026/11/27 10:30:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s", closed.Add(-time.Minute), closed, true, false},
		{"another rig's swept line is not this rig's run", "2026/11/27 10:30:00 tier_sweep: other: swept 1a2b3c4d (shell GREEN) in 1m0s", time.Time{}, time.Time{}, false, false},
		{"an aborted cycle is not a run", "2026/11/27 10:30:00 tier_sweep: gastown: sweep aborted 1a2b3c4d (no stage ran)", time.Time{}, time.Time{}, false, false},
		{"a stampless swept line is not a run", "tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s", time.Time{}, time.Time{}, false, false},
		{"a swept line at start is not this window's run", "2026/11/27 10:00:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s", time.Time{}, time.Time{}, false, false},
		{"a swept line before start is not this window's run", "2026/11/27 09:00:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in 1m0s", time.Time{}, time.Time{}, false, false},
		{"a swept line with no wall is an error", "2026/11/27 10:30:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN)", time.Time{}, time.Time{}, false, true},
		{"a swept line with an unparseable wall is an error", "2026/11/27 10:30:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in soon", time.Time{}, time.Time{}, false, true},
		{"a swept line with a negative wall is an error", "2026/11/27 10:30:00 tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in -1m0s", time.Time{}, time.Time{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotStart, gotEnd, ok, err := sweptLine(tt.line, "gastown", start)
			if tt.wantErr {
				if err == nil || ok || !strings.Contains(err.Error(), tt.line) {
					t.Fatalf("sweptLine(%q) = ok %v, err %v; want ok false and an error naming the line", tt.line, ok, err)
				}
				return
			}
			if err != nil || ok != tt.ok || !gotStart.Equal(tt.start) || !gotEnd.Equal(tt.end) {
				t.Fatalf("sweptLine(%q) = [%v, %v], ok %v, err %v; want [%v, %v], ok %v", tt.line, gotStart, gotEnd, ok, err, tt.start, tt.end, tt.ok)
			}
		})
	}
}

func TestRehearsalSweepRan(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 27, 10, 30, 0, 0, time.UTC)
	const line = "tier_sweep: gastown: swept 1a2b3c4d (shell GREEN) in "
	tests := []struct {
		name, log string
		want      bool
		bad       string
	}{
		{"a run inside the window overlaps it", "2026/11/27 10:10:00 " + line + "5m0s\n", true, ""},
		{"a run that began before start still overlaps", "2026/11/27 10:05:00 " + line + "10m0s\n", true, ""},
		{"a run ending at start does not overlap", "2026/11/27 10:00:00 " + line + "5m0s\n", false, ""},
		{"a run beginning at end does not overlap", "2026/11/27 10:35:00 " + line + "5m0s\n", false, ""},
		{"a refused line is false with its error", "2026/11/27 10:10:00 " + line + "soon\n", false, "soon"},
		{"a refused line beside an overlapping run is still false with its error", "2026/11/27 10:10:00 " + line + "5m0s\n2026/11/27 10:12:00 " + line + "soon\n", false, "soon"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sweepRan([]byte(tt.log), "gastown", start, end)
			if tt.bad != "" {
				if err == nil || got || !strings.Contains(err.Error(), tt.bad) {
					t.Fatalf("sweepRan(%q) = %v, %v; want false and an error naming %q", tt.log, got, err, tt.bad)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("sweepRan(%q) = %v, %v; want %v with no error", tt.log, got, err, tt.want)
			}
		})
	}
}
