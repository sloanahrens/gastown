package tiertrigger

import (
	"testing"
	"time"

	_ "time/tzdata" // the rows read America/Chicago, so they must not depend on the host's zoneinfo
)

// at is a time on the daily window's clock: the town zone, whatever the host's
// zoneinfo says. It takes the test because a zone that will not load is that
// test's failure, not a crash of the whole binary.
func at(tb testing.TB, y int, mo time.Month, d, h, m, s int) time.Time {
	tb.Helper()
	loc, err := DailyZoneLocation()
	if err != nil {
		tb.Fatalf("tiertrigger: %v", err)
	}
	return time.Date(y, mo, d, h, m, s, 0, loc)
}

// utc is the same instant spelled in UTC, to prove the boundary is read in the
// town zone rather than in the caller's.
func utc(y int, mo time.Month, d, h, m, s int) time.Time {
	return time.Date(y, mo, d, h, m, s, 0, time.UTC)
}

func TestDailyRunDue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		lastRun time.Time
		now     time.Time
		want    bool
	}{
		{"no recorded run is due once the window opens", time.Time{}, at(t, 2026, 10, 25, 3, 30, 0), true},
		{"not due before the window opens even with no recorded run", time.Time{}, at(t, 2026, 10, 25, 3, 29, 59), false},
		{"due at 03:30 the day after the last run", at(t, 2026, 10, 24, 3, 30, 0), at(t, 2026, 10, 25, 3, 30, 0), true},
		{"not due a second before 03:30", at(t, 2026, 10, 24, 3, 30, 0), at(t, 2026, 10, 25, 3, 29, 59), false},
		{"not due twice in one day", at(t, 2026, 10, 25, 3, 30, 0), at(t, 2026, 10, 25, 23, 59, 0), false},
		{"a run exactly at 03:30 consumes that day's window", at(t, 2026, 10, 25, 3, 30, 0), at(t, 2026, 10, 25, 3, 30, 0), false},
		{"due after a missed day", at(t, 2026, 10, 23, 3, 31, 0), at(t, 2026, 10, 25, 3, 30, 0), true},
		{"a run before the window does not count for that day", at(t, 2026, 10, 25, 3, 0, 0), at(t, 2026, 10, 25, 4, 0, 0), true},
		{"a clock set back is due", at(t, 2026, 10, 25, 5, 0, 0), at(t, 2026, 10, 25, 3, 0, 0), true},
		{"the window is 03:30 in the town zone, not UTC", utc(2026, 10, 25, 8, 0, 0), utc(2026, 10, 25, 9, 0, 0), true},
		{"due at 03:30 CDT on the day daylight saving starts", at(t, 2026, 3, 7, 3, 31, 0), at(t, 2026, 3, 8, 3, 30, 0), true},
		{"due at 03:30 CST on the day daylight saving ends", at(t, 2026, 10, 31, 3, 31, 0), at(t, 2026, 11, 1, 3, 30, 0), true},
	}
	loc, err := DailyZoneLocation()
	if err != nil {
		t.Fatalf("DailyZoneLocation: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DailyRunDue(tc.lastRun, tc.now, loc); got != tc.want {
				t.Fatalf("DailyRunDue(last %s, now %s) = %v, want %v", tc.lastRun, tc.now, got, tc.want)
			}
		})
	}
}

func TestDailyZoneLocation(t *testing.T) {
	t.Parallel()
	loc, err := DailyZoneLocation()
	if err != nil {
		t.Fatalf("DailyZoneLocation: %v", err)
	}
	if got := loc.String(); got != DailyZone {
		t.Fatalf("DailyZoneLocation() = %q, want %q", got, DailyZone)
	}
}
