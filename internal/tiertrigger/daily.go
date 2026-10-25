package tiertrigger

import "time"

// The daily container run: the town's container packages run once a day at
// DailyHour:DailyMinute in DailyZone, whether or not their paths changed.
const (
	DailyHour   = 3
	DailyMinute = 30
	DailyZone   = "America/Chicago"
)

// DailyZoneLocation loads the zone DailyZone names.
func DailyZoneLocation() (*time.Location, error) {
	return time.LoadLocation(DailyZone)
}

// DailyRunDue reports whether the daily container run is due at now given
// lastRun, the start of the previous one: due once today's
// DailyHour:DailyMinute in loc has passed with no run since it. A zero lastRun
// (never run) is due as soon as the window opens, and a lastRun after now (a
// clock set back) is due immediately, so a missing stamp or a backwards clock
// costs one extra run rather than a wedged schedule. loc is the zone the window
// is read in and must not be nil.
func DailyRunDue(lastRun, now time.Time, loc *time.Location) bool {
	if lastRun.After(now) {
		return true
	}
	n := now.In(loc)
	window := dailyWindow(n, loc)
	if n.Before(window) {
		return false
	}
	return lastRun.Before(window)
}

// dailyWindow returns today's DailyHour:DailyMinute in loc, from a now already
// converted to loc.
func dailyWindow(n time.Time, loc *time.Location) time.Time {
	y, m, d := n.Date()
	return time.Date(y, m, d, DailyHour, DailyMinute, 0, 0, loc)
}
