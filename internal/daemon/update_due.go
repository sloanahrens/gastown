package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// updateWindowLength is how long the nightly update window stays open after
// its configured HH:MM start.
const updateWindowLength = time.Hour

// UpdateFacts is what the due check knows about the town. The caller gathers
// it; updateDue reads nothing itself.
type UpdateFacts struct {
	// Standby is the standby reason, empty when the town is running.
	Standby string
	// Running reports a gt update run in progress (lock held, or its
	// heartbeat fresh).
	Running bool
	// InstallPending reports an install marker that has not been cleared.
	InstallPending bool
	// MainMoved reports main differs from the installed build.
	MainMoved bool
	// Waiting counts landings waiting to deploy.
	Waiting int
	// LastStarted is when the last run started; zero when none.
	LastStarted time.Time
	// LastEnded is when the last run ended; zero when none.
	LastEnded time.Time
}

// updateDue reports whether an update is due at now and why. It is pure: the
// caller passes the clock, the settings and the facts.
func updateDue(now time.Time, s config.UpdateSettings, f UpdateFacts) (due bool, reason string) {
	if !s.Enabled {
		return false, "update disabled: patrols.update.enabled is false"
	}
	if f.Standby != "" {
		return false, "town in standby: " + f.Standby
	}
	if f.Running {
		return false, "an update run is in progress"
	}
	if f.InstallPending {
		return false, "an install marker is pending"
	}

	// The nightly rule: one run per day, and only inside the hour that opens
	// at the window start. A start late in the day opens a window that runs
	// past midnight, so when now is before today's start the open one is the
	// window yesterday's start left behind. LastStarted before that start
	// means no run has started since it opened, so a zero LastStarted
	// qualifies too.
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), s.WindowHour, s.WindowMinute, 0, 0, now.Location())
	if now.Before(windowStart) {
		windowStart = windowStart.AddDate(0, 0, -1)
	}
	if now.Before(windowStart.Add(updateWindowLength)) && f.LastStarted.Before(windowStart) {
		return true, "nightly window is open and no run has started since it opened"
	}

	// The catch-up rule: main moved and something has been waiting long
	// enough that the last run's end, if any, is MinInterval behind.
	if f.MainMoved && f.Waiting >= 1 && (f.LastEnded.IsZero() || now.Sub(f.LastEnded) >= s.MinInterval) {
		return true, "main moved and a landing is waiting to deploy"
	}

	return false, "nothing due"
}
