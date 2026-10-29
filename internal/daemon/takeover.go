package daemon

import (
	"fmt"
	"time"
)

// TakeoverWindow is how old a recorded heartbeat must be before a start may read
// it as evidence the recorded owner is gone. It is more than twice the default
// heartbeat interval, so one missed beat is never read as a stop.
const TakeoverWindow = 12 * time.Minute

// OwnerProbe reports whether an owner is running. known is false when the
// owner's state could not be read, and CheckTakeover refuses on unknown rather
// than treat it as stopped, because a Docker error is not proof of an exit.
type OwnerProbe func(Owner) (running bool, known bool)

// TakeoverInput is everything CheckTakeover judges. The caller reads state.json,
// looks for other signs of a prior daemon, and supplies the clock and the probe,
// so the guard does no I/O of its own.
type TakeoverInput struct {
	// State is the recorded daemon state; nil means state.json is missing.
	State *State
	// PriorRun is true when another sign of a prior daemon remains on the town
	// root (daemon/daemon.lock or daemon/daemon.log), which outlives a deleted
	// state file.
	PriorRun bool
	// Candidates are the owners this town could have (launchd and container),
	// probed when the state names no owner. An empty list refuses a takeover:
	// with nothing to probe, the old owner cannot be shown stopped.
	Candidates []Owner
	// Now is the caller's clock.
	Now time.Time
	// Self is the owner of the daemon about to start.
	Self Owner
	// Takeover lets a start pass a recorded owner the probe shows not running;
	// it never passes one that is running or unreadable.
	Takeover bool
	// Probe reads an owner's liveness, and every rule that judges an owner needs
	// it; a nil probe refuses rather than allow.
	Probe OwnerProbe
}

// CheckTakeover decides whether a daemon may start on a town root another owner
// may still hold: a nil error allows the start, and every error refuses and names
// what it read. The rules apply in this order:
//
//  1. No state file and no other sign of a prior daemon: allow.
//  2. No state file, or a legacy state with no owner: refuse; with Takeover,
//     allow once the probe shows every candidate other than Self not running,
//     and only when a candidate other than Self was named at all.
//  3. The recorded owner is Self: allow, so an owner restarts itself.
//  4. A recorded clean stop: allow only when the probe shows the recorded owner
//     not running, whatever the heartbeat age. This is the cutover's handoff.
//  5. A heartbeat under TakeoverWindow old: refuse; with Takeover, allow only
//     when the probe shows the recorded owner not running.
//  6. A stale heartbeat: allow only when the probe shows the recorded owner not
//     running. Running or unknown refuses, because a stuck daemon also stops
//     writing its heartbeat, and its lock is not visible across a container
//     boundary.
func CheckTakeover(in TakeoverInput) error {
	if in.State == nil && !in.PriorRun {
		return nil
	}
	// An ownerless state was written by a binary older than the owner field, and
	// is handled like a missing file with prior-run evidence (the plan, A7).
	if in.State == nil || in.State.Owner == nil {
		return checkLegacyStart(in)
	}
	owner := *in.State.Owner
	if sameOwner(owner, in.Self) {
		return nil
	}
	if !in.State.Running {
		return refuseUnlessStopped(in, owner,
			fmt.Sprintf("state.json records a clean stop by %s", ownerName(owner)))
	}
	heartbeat := fmt.Sprintf("%s and its heartbeat was %s", ownerName(owner),
		heartbeatPhrase(in.Now, in.State.LastHeartbeat))
	if in.Now.Sub(in.State.LastHeartbeat) < TakeoverWindow {
		heartbeat += fmt.Sprintf(", under the %s takeover window", TakeoverWindow)
		if !in.Takeover {
			return fmt.Errorf("%s; refusing to start %s beside it", heartbeat, ownerName(in.Self))
		}
	}
	return refuseUnlessStopped(in, owner, heartbeat)
}

// checkLegacyStart refuses a start where no usable owner was recorded. The two
// ways in are a missing state file with a leftover lock or log, and a state file
// written before owners existed; both leave the old owner unnameable, so only a
// takeover that proves every candidate other than the starter stopped may pass.
func checkLegacyStart(in TakeoverInput) error {
	found := "state.json is missing and another sign of a prior daemon remains"
	if in.State != nil {
		found = "state.json has no owner (a state written by an older binary)"
	}
	if !in.Takeover {
		return fmt.Errorf("%s; refusing to start %s without takeover", found, ownerName(in.Self))
	}
	if in.Probe == nil {
		return fmt.Errorf("%s; takeover refuses because no owner probe was supplied to show the old owner stopped", found)
	}
	if len(in.Candidates) == 0 {
		return fmt.Errorf("%s; takeover refuses because no owner kinds were named to probe", found)
	}
	// A probe run from inside the starting daemon reports its own owner running or
	// unreadable, so probing that owner would refuse every legacy takeover.
	probeable := make([]Owner, 0, len(in.Candidates))
	for _, candidate := range in.Candidates {
		if !sameOwner(candidate, in.Self) {
			probeable = append(probeable, candidate)
		}
	}
	if len(probeable) == 0 {
		return fmt.Errorf("%s; takeover refuses because every owner named is the starting daemon's own", found)
	}
	for _, candidate := range probeable {
		running, known := in.Probe(candidate)
		switch {
		case !known:
			return fmt.Errorf("%s; takeover refuses because whether %s is running is unknown", found, ownerName(candidate))
		case running:
			return fmt.Errorf("%s; takeover refuses because %s is still running", found, ownerName(candidate))
		}
	}
	return nil
}

// refuseUnlessStopped allows the start when the probe proves owner is gone, and
// otherwise refuses, reading found as the clause that states what was recorded.
func refuseUnlessStopped(in TakeoverInput, owner Owner, found string) error {
	stopped, why := ownerStopped(in, owner)
	if stopped {
		return nil
	}
	return fmt.Errorf("%s, but %s; refusing to start %s", found, why, ownerName(in.Self))
}

// ownerStopped reports whether the probe proves the owner is not running. A
// missing probe or an unreadable owner is not proof, and each returns the clause
// a refusal states.
func ownerStopped(in TakeoverInput, owner Owner) (bool, string) {
	if in.Probe == nil {
		return false, fmt.Sprintf("whether %s is still running is unknown (no probe was supplied)", ownerName(owner))
	}
	running, known := in.Probe(owner)
	switch {
	case !known:
		return false, fmt.Sprintf("whether %s is still running is unknown", ownerName(owner))
	case running:
		return false, fmt.Sprintf("%s is still running", ownerName(owner))
	}
	return true, ""
}

// heartbeatPhrase states a recorded heartbeat's age for a refusal, in minutes
// with the exact duration beside it. A heartbeat dated after the caller's clock
// reads as fresh (its age is under the window), so a clock that moved backwards
// refuses rather than count as proof the owner stopped.
func heartbeatPhrase(now, last time.Time) string {
	switch {
	case last.IsZero():
		return "never recorded"
	case last.After(now):
		return fmt.Sprintf("recorded %s ahead of the clock", last.Sub(now).Round(time.Second))
	}
	age := now.Sub(last)
	return fmt.Sprintf("%s old (%d minutes)", age.Round(time.Second), int(age.Minutes()))
}

// sameOwner reports whether a and b name the same supervisor. An owner with no
// kind and no id names nothing, so it matches nothing, which keeps a zero-value
// Self from passing an ownerless record off as a restart of itself.
func sameOwner(a, b Owner) bool {
	if !namesOwner(a) || !namesOwner(b) {
		return false
	}
	return a == b
}

// namesOwner reports whether an owner carries a kind or an id to be identified by.
func namesOwner(o Owner) bool {
	return o.Kind != "" || o.ID != ""
}

// ownerName names an owner the way every refusal does: its kind and its id.
func ownerName(owner Owner) string {
	if owner.Kind == "" && owner.ID == "" {
		return "an owner with no kind or id"
	}
	return fmt.Sprintf("%s owner %q", owner.Kind, owner.ID)
}
