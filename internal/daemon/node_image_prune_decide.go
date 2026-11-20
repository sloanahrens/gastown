package daemon

// The node image prune's decision: whether the prune is due for the update run
// that just ended, and whether this pass may remove images or only report them.
// It reads the update state, the active update Jobs and the run the prune last
// ran for, and asks nodeImagePruneDue. Every read the daemon cannot make refuses
// with its reason. The Job list is read first and a failure returns before the
// gate is asked: the gate takes a nil or empty list as no Job active, so an error
// dropped into it would let the prune run during a roll (gt-pxb6jq).

import (
	"errors"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// nodeImagePruneDecision says whether the control node's image prune is due and
// whether to apply it. It is ok only when an update run ended ok, no update Job
// is active and that run's id is not yet recorded as pruned; apply is true only
// when the disk-maintenance mode is apply.
func (d *Daemon) nodeImagePruneDecision() (runID string, apply, ok bool, why string) {
	jobs, err := d.activeUpdateJobs()
	if err != nil {
		return "", false, false, "update Job list could not be read: " + err.Error()
	}
	store := update.Store{Dir: update.Dir(d.config.TownRoot)}
	st, err := store.Read()
	if err != nil && !errors.Is(err, update.ErrNoState) {
		return "", false, false, "the update state could not be read: " + err.Error()
	}
	last, err := readNodeImagePruneLast(d.config.TownRoot)
	if err != nil {
		return "", false, false, err.Error()
	}
	runID, ok, why = nodeImagePruneDue(st, last, jobs)
	if !ok {
		return runID, false, false, why
	}
	mode, err := diskMaintenanceMode(d.patrol())
	if err != nil {
		return runID, false, false, err.Error()
	}
	return runID, mode == agentconfig.DiskMaintenanceModeApply, true, why
}
