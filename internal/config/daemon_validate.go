package config

import "fmt"

// ValidateDaemonPatrolConfig runs every check the daemon's schema applies to a
// daemon patrol document: the type and version, the overrun guard's
// scheduled_stop_after, the container names, the container_off names the step
// audit does not classify, the tier_sweep runner, the disk maintenance cadences
// and percent, and every scheduled_slings entry. The writer paths and the
// command that edits settings/config.json call it before a document is saved,
// so neither can save one the other refuses (gt-yjzepk.4).
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
	for _, name := range c.ContainerOff() {
		if !DaemonStepKnown(name) {
			return fmt.Errorf("%s: in daemon.container_off but unknown to the audit table (a typo, or a step to classify)", name)
		}
	}
	if c.Patrols == nil {
		return nil
	}
	if ts := c.Patrols.TierSweep; ts != nil && ts.Runner != nil {
		if err := ts.Runner.validate(); err != nil {
			return fmt.Errorf("daemon.patrols.tier_sweep.runner.%w", err)
		}
	}
	if dm := c.Patrols.DiskMaintenance; dm != nil {
		if err := dm.validate(); err != nil {
			return err
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
