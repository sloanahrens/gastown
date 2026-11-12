package config

import (
	"fmt"
	"sort"
	"time"
)

// DefaultSandboxEndedHoldSeconds is how long a finished seat's sandbox stays up
// after the agent's session ends (gt-yck7hz). Thirty minutes is three times the
// 10m patrol_scan tick the town runs today; see ValidateSandboxEndedHolds.
const DefaultSandboxEndedHoldSeconds = 1800

// sandboxEndedHoldIntervals is how many patrol_scan intervals a sandbox's hold
// must cover. A seat that ends just after a tick waits up to one interval plus
// the record's grace before the deleter sees it, so a shorter hold lets the
// gateway delete the sandbox first and the transcripts are lost.
const sandboxEndedHoldIntervals = 3

// defaultPatrolScanInterval is the interval the patrol_scan tick runs at when
// its block sets none. The daemon's patrolScanInterval returns PatrolScanInterval,
// so this is the one default.
const defaultPatrolScanInterval = 2 * time.Minute

// EndedHold returns how long the profile's sandbox stays up after the agent's
// session ends: EndedHoldSeconds, or DefaultSandboxEndedHoldSeconds when unset.
// An explicit zero is returned as zero, so ValidateSandboxEndedHolds refuses it.
func (p SandboxProfile) EndedHold() int {
	if p.EndedHoldSeconds == nil {
		return DefaultSandboxEndedHoldSeconds
	}
	return *p.EndedHoldSeconds
}

// PatrolScanInterval returns the interval the patrol_scan tick runs at: the
// block's interval when it parses to a positive duration, else the default. It
// reads the value the way the daemon's patrolScanInterval does, so the hold is
// checked against the cadence the daemon actually runs.
func PatrolScanInterval(daemon *DaemonPatrolConfig) time.Duration {
	if daemon == nil || daemon.Patrols == nil || daemon.Patrols.PatrolScan == nil {
		return defaultPatrolScanInterval
	}
	if s := daemon.Patrols.PatrolScan.IntervalStr; s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return defaultPatrolScanInterval
}

// ValidateSandboxEndedHolds refuses the first sandbox profile, in name order,
// whose hold is below sandboxEndedHoldIntervals patrol_scan intervals. The hold
// checked is the one the profile runs with, so an unset value is checked as the
// default. The settings file does not carry the patrol interval, so the town
// loader calls this with both files.
func ValidateSandboxEndedHolds(ts *TownSettings, daemon *DaemonPatrolConfig) error {
	if ts == nil {
		return nil
	}
	names := make([]string, 0, len(ts.Sandboxes))
	for name := range ts.Sandboxes {
		names = append(names, name)
	}
	sort.Strings(names)
	interval := PatrolScanInterval(daemon)
	minimum := sandboxEndedHoldIntervals * interval
	for _, name := range names {
		hold := ts.Sandboxes[name].EndedHold()
		if time.Duration(hold)*time.Second < minimum {
			return fmt.Errorf("sandboxes.%s.ended_hold_seconds %d: want at least %g, which is %d times patrols.patrol_scan.interval %s",
				name, hold, minimum.Seconds(), sandboxEndedHoldIntervals, interval)
		}
	}
	return nil
}
