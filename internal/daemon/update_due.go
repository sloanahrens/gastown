package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
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
	// SourceMissing reports the town has no gt source checkout: main cannot be
	// read for a target and a run cannot install, so nothing may launch.
	SourceMissing bool
	// MainMoved reports main differs from the installed build.
	MainMoved bool
	// Waiting counts landings waiting to deploy.
	Waiting int
	// LastStarted is when the last run started; zero when none.
	LastStarted time.Time
	// LastEnded is when the last run ended; zero when none.
	LastEnded time.Time
	// LastOutcome is how the last run ended; empty when none has.
	LastOutcome update.Outcome
	// LastTarget is the main sha the last run set out to install.
	LastTarget string
	// MainSHA is main's current sha, the target a new run would set out to
	// install; empty when it could not be read.
	MainSHA string
}

// updateDue reports whether an update is due at now and why. It is pure: the
// caller passes the clock, the settings and the facts.
func updateDue(now time.Time, s config.UpdateSettings, f UpdateFacts) (due bool, reason string) {
	if !s.Enabled {
		return false, "update disabled: patrols.update.enabled is false"
	}
	// Without a checkout there is no main to target and no build to install,
	// so nothing makes a run due there; name that rather than let a standby or
	// a running state answer for it.
	if f.SourceMissing {
		return false, "the town has no gt source checkout: gt update cannot run here"
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

	// A run that failed or aborted on main's current target would fail the same
	// way again: it drains the town and fails at the same step. Hold it until
	// main moves to a new target or a run succeeds. A failure on an older
	// target holds nothing. An unknown main sha cannot show the target moved,
	// so it holds too.
	if f.LastOutcome == update.OutcomeFailed || f.LastOutcome == update.OutcomeAborted {
		if f.MainSHA == "" {
			return false, "last run failed and main's sha is unknown; run gt update by hand"
		}
		if f.LastTarget == f.MainSHA {
			return false, "last run failed on this target; run gt update by hand"
		}
	}

	// The nightly rule: one run per day, and only inside the hour that opens
	// at the window start. A start late in the day opens a window that runs
	// past midnight, so when now is before today's start the open one is the
	// window yesterday's start left behind. LastStarted before that start
	// means no run has started since it opened, so a zero LastStarted
	// qualifies too. The hold above has already returned for a failed or
	// aborted run on main's target, and the source check for a town that
	// cannot update, so neither opens a window here.
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
