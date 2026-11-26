package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// reconcilerInterval returns how often the reconciler patrol runs. It is the
// Reconciler block's interval when the block sets a valid one; a missing config,
// a missing block, or an interval ParseInterval refuses reads the config
// default, so a bad setting never stops the patrol.
func reconcilerInterval(pc *config.DaemonPatrolConfig) time.Duration {
	if pc == nil || pc.Patrols == nil {
		return config.DefaultReconcilerInterval
	}
	d, err := pc.Patrols.Reconciler.ParseInterval()
	if err != nil {
		return config.DefaultReconcilerInterval
	}
	return d
}
