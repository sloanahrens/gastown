package config

import (
	"fmt"
	"time"
)

// The tier_sweep duration keys, spelled as the operator edits them in
// settings/config.json.
const (
	TierSweepIntervalKey          = "daemon.patrols.tier_sweep.interval"
	TierSweepRestartStaleAfterKey = "daemon.patrols.tier_sweep.restart_stale_after"
)

// ValidateDaemonPatrolConfig runs every check the daemon's schema applies to a
// daemon patrol document: the type and version, the overrun guard's
// scheduled_stop_after, gate_allowance and split_added_lines, the story points
// change budget's lint_budget_max_lines, the container names, the polecat image
// source, the container_off names the step audit does not classify, the
// tier_sweep runner and its interval and restart_stale_after, the reconciler's
// interval, grace and four duty limits, the disk maintenance cadences and
// percent, the update
// patrol's window, min_interval and max_runtime, and every scheduled_slings
// entry.
// The writer paths and the command that edits settings/config.json call it
// before a document is saved, so neither can save one the other refuses
// (gt-yjzepk.4).
func ValidateDaemonPatrolConfig(c *DaemonPatrolConfig) error {
	if c == nil {
		return nil
	}
	if err := validateDaemonPatrolConfig(c); err != nil {
		return err
	}
	if err := c.ValidateContainerNames(); err != nil {
		return err
	}
	if err := c.ValidatePolecatImageSource(); err != nil {
		return err
	}
	for _, name := range c.ContainerOff() {
		if !DaemonStepKnown(name) {
			return fmt.Errorf("%s: in daemon.container_off but unknown to the audit table (a typo, or a step to classify)", name)
		}
	}
	if c.Patrols == nil {
		return nil
	}
	if ts := c.Patrols.TierSweep; ts != nil {
		// The sweep reads both durations with time.ParseDuration and silently
		// keeps its default for one that does not parse or is not positive
		// (internal/daemon/tier_sweep.go), so the write refuses those values.
		if err := validatePositiveDuration(TierSweepIntervalKey, ts.IntervalStr); err != nil {
			return err
		}
		if err := validatePositiveDuration(TierSweepRestartStaleAfterKey, ts.RestartStaleAfterStr); err != nil {
			return err
		}
		if ts.Runner != nil {
			if err := ts.Runner.validate(); err != nil {
				return fmt.Errorf("daemon.patrols.tier_sweep.runner.%w", err)
			}
		}
	}
	if r := c.Patrols.Reconciler; r != nil {
		// The patrol reads its interval, grace and each duty's limit with these
		// parsers, which refuse a value outside their bounds rather than
		// clamping it. The write, load and boot paths all run this check, so
		// each refuses the same values. The parse errors name
		// patrols.reconciler.<key>; daemon. prefixes it to the path the operator
		// edits, as the update block does.
		if _, err := r.ParseInterval(); err != nil {
			return fmt.Errorf("daemon.%w", err)
		}
		if _, err := r.ParseGrace(); err != nil {
			return fmt.Errorf("daemon.%w", err)
		}
		for _, duty := range []string{
			ReconcilerDutyBackup,
			ReconcilerDutyArchive,
			ReconcilerDutyGC,
			ReconcilerDutyTownState,
		} {
			if _, err := r.ParseLimit(duty); err != nil {
				return fmt.Errorf("daemon.%w", err)
			}
		}
	}
	if dm := c.Patrols.DiskMaintenance; dm != nil {
		if err := dm.validate(); err != nil {
			return err
		}
	}
	if u := c.Patrols.Update; u != nil {
		// Settings is the parse the daemon's trigger runs, refusing the window
		// and both durations. It names the patrols.update.<key> path; the
		// operator edits daemon.patrols.update.<key> in settings/config.json.
		if _, err := u.Settings(); err != nil {
			return fmt.Errorf("daemon.%w", err)
		}
	}
	if ss := c.Patrols.ScheduledSlings; ss != nil {
		for _, e := range ss.Entries {
			if err := ValidateScheduledSlingEntry(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePositiveDuration refuses a non-empty setting that time.ParseDuration
// does not read as a positive duration, naming key. Only exactly "" means the
// default applies: the sweep parses any other value, so a blank string is
// refused rather than read as empty.
func validatePositiveDuration(key, setting string) error {
	if setting == "" {
		return nil
	}
	d, err := time.ParseDuration(setting)
	if err != nil {
		return fmt.Errorf("%s = %q: want a Go duration such as 1h: %w", key, setting, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s = %q: want a duration greater than zero", key, setting)
	}
	return nil
}
