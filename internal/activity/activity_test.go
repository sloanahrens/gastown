package activity

import (
	"testing"
	"time"
)

// testEpoch is the fixed "now" the tests measure activity ages from.
var testEpoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// TestCalculateUsesWallClock checks the exported wrapper measures from
// time.Now: a stamp a day old is red whatever the host's load.
func TestCalculateUsesWallClock(t *testing.T) {
	t.Parallel()
	last := time.Now().Add(-24 * time.Hour)
	info := Calculate(last)
	if info.ColorClass != ColorRed || !info.LastActivity.Equal(last) || info.Duration < 24*time.Hour {
		t.Errorf("Calculate(now-24h) = %+v, want red with duration >= 24h", info)
	}
}

func TestCalculateActivity_Green(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		age      time.Duration
		wantAge  string
		wantColor string
	}{
		{"just now", 0, "<1m", ColorGreen},
		{"30 seconds", 30 * time.Second, "<1m", ColorGreen},
		{"1 minute", 1 * time.Minute, "1m", ColorGreen},
		{"2 minutes", 2 * time.Minute, "2m", ColorGreen},
		{"3 minutes", 3 * time.Minute, "3m", ColorGreen},
		{"4m59s", 299 * time.Second, "4m", ColorGreen},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := calculateAt(testEpoch.Add(-tt.age), testEpoch)

			if info.FormattedAge != tt.wantAge {
				t.Errorf("FormattedAge = %q, want %q", info.FormattedAge, tt.wantAge)
			}
			if info.ColorClass != tt.wantColor {
				t.Errorf("ColorClass = %q, want %q", info.ColorClass, tt.wantColor)
			}
		})
	}
}

func TestCalculateActivity_Yellow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		age      time.Duration
		wantAge  string
		wantColor string
	}{
		{"5 minutes", 5 * time.Minute, "5m", ColorYellow},
		{"6 minutes", 6 * time.Minute, "6m", ColorYellow},
		{"8 minutes", 8 * time.Minute, "8m", ColorYellow},
		{"9m59s", 599 * time.Second, "9m", ColorYellow},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := calculateAt(testEpoch.Add(-tt.age), testEpoch)

			if info.FormattedAge != tt.wantAge {
				t.Errorf("FormattedAge = %q, want %q", info.FormattedAge, tt.wantAge)
			}
			if info.ColorClass != tt.wantColor {
				t.Errorf("ColorClass = %q, want %q", info.ColorClass, tt.wantColor)
			}
		})
	}
}

func TestCalculateActivity_Red(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		age      time.Duration
		wantAge  string
		wantColor string
	}{
		{"10 minutes", 10 * time.Minute, "10m", ColorRed},
		{"15 minutes", 15 * time.Minute, "15m", ColorRed},
		{"30 minutes", 30 * time.Minute, "30m", ColorRed},
		{"1 hour", 1 * time.Hour, "1h", ColorRed},
		{"2 hours", 2 * time.Hour, "2h", ColorRed},
		{"1 day", 24 * time.Hour, "1d", ColorRed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := calculateAt(testEpoch.Add(-tt.age), testEpoch)

			if info.FormattedAge != tt.wantAge {
				t.Errorf("FormattedAge = %q, want %q", info.FormattedAge, tt.wantAge)
			}
			if info.ColorClass != tt.wantColor {
				t.Errorf("ColorClass = %q, want %q", info.ColorClass, tt.wantColor)
			}
		})
	}
}

func TestCalculateActivity_ZeroTime(t *testing.T) {
	t.Parallel()
	// Zero time should return unknown state
	info := Calculate(time.Time{})

	if info.ColorClass != ColorUnknown {
		t.Errorf("ColorClass = %q, want %q for zero time", info.ColorClass, ColorUnknown)
	}
	if info.FormattedAge != "unknown" {
		t.Errorf("FormattedAge = %q, want %q for zero time", info.FormattedAge, "unknown")
	}
}

func TestCalculateActivity_FutureTime(t *testing.T) {
	t.Parallel()
	// Future time (clock skew) should be treated as "just now"
	info := calculateAt(testEpoch.Add(5*time.Second), testEpoch)

	if info.ColorClass != ColorGreen || info.Duration != 0 || info.FormattedAge != "<1m" {
		t.Errorf("info = %+v, want green, zero duration, <1m for future time", info)
	}
}

func TestInfo_IsActive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		color    string
		isActive bool
	}{
		{ColorGreen, true},
		{ColorYellow, false},
		{ColorRed, false},
		{ColorUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.color, func(t *testing.T) {
			info := Info{ColorClass: tt.color}
			if info.IsActive() != tt.isActive {
				t.Errorf("IsActive() = %v, want %v for color %q", info.IsActive(), tt.isActive, tt.color)
			}
		})
	}
}

func TestInfo_IsStale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		color   string
		isStale bool
	}{
		{ColorGreen, false},
		{ColorYellow, true},
		{ColorRed, false},
		{ColorUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.color, func(t *testing.T) {
			info := Info{ColorClass: tt.color}
			if info.IsStale() != tt.isStale {
				t.Errorf("IsStale() = %v, want %v for color %q", info.IsStale(), tt.isStale, tt.color)
			}
		})
	}
}

func TestInfo_IsStuck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		color   string
		isStuck bool
	}{
		{ColorGreen, false},
		{ColorYellow, false},
		{ColorRed, true},
		{ColorUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.color, func(t *testing.T) {
			info := Info{ColorClass: tt.color}
			if info.IsStuck() != tt.isStuck {
				t.Errorf("IsStuck() = %v, want %v for color %q", info.IsStuck(), tt.isStuck, tt.color)
			}
		})
	}
}
