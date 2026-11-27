package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// clusterDriftStartDelay is how long after the daemon starts the drift patrol
// waits before its first run: a roll starts the new daemon while the gateway
// and dashboard rollouts may still be settling (gt-myyr6e). It is a constant,
// not a setting.
const clusterDriftStartDelay = 3 * time.Minute

// clusterDriftStarting reports whether now is still inside clusterDriftStartDelay
// after the daemon started. A daemon with no recorded start is not starting.
func (d *Daemon) clusterDriftStarting(now time.Time) bool {
	started := d.clusterDriftCache.started
	if started.IsZero() {
		return false
	}
	return now.Sub(started) < clusterDriftStartDelay
}

// errClusterDriftNoAnswer is the unreadable item's reason when the setting is
// on and no compare is wired: a clear would claim the cluster was read and in
// step, and nothing read it (gt-b6j1k5).
var errClusterDriftNoAnswer = errors.New("no drift compare is wired into the daemon, so the cluster was not read")

// collectClusterDrift turns the drift compare's answer into its items: the
// drift item for a difference and the unreadable item for a read that failed
// or was never wired. It answers nothing while the patrol is off, and calls
// the answer on every heartbeat; the 15 minute cadence is the cache bead's
// (gt-0cdmgq). The error it returns is always nil and no timer retries: a
// failed answer applies the unreadable item at once and keeps the drift item
// from prev, the last run that read the cluster and found a difference, so a
// failed read never clears an open difference (gt-b6j1k5, gt-9jbtl6).
func (s *attentionSources) collectClusterDrift(ctx context.Context) ([]attention.Item, error) {
	if s.clusterDriftEnabled == nil || !s.clusterDriftEnabled() {
		return nil, nil
	}
	if s.clusterDriftAnswer == nil {
		return clusterDriftKeepingTheDifference(s.prev, clusterDriftItems(clusterDriftResult{}, errClusterDriftNoAnswer)), nil
	}
	res, err := s.clusterDriftAnswer(ctx)
	return clusterDriftKeepingTheDifference(s.prev, clusterDriftItems(res, err)), nil
}

// clusterDriftKeepingTheDifference returns an unreadable run's item beside the
// drift item the last readable run with a difference raised, held in prev until
// a run reads the cluster again: a clean run returns neither and clears both, a
// run with a difference replaces the kept one. The drift item is never built
// from a failed read, so a failure cannot clear an open difference
// (gt-9jbtl6).
func clusterDriftKeepingTheDifference(prev attention.State, items []attention.Item) []attention.Item {
	if len(items) != 1 || items[0].Kind != attention.KindClusterDriftUnreadable {
		return items
	}
	return append(prevOfKind(prev, attention.KindClusterDrift), items...)
}

// clusterDriftRun is the compare the cache runs: the injected one, or the
// unreadable answer a nil compare stands for, so a setting that is on with no
// compare wired raises the unreadable item rather than a false clear
// (gt-menvlj).
func (d *Daemon) clusterDriftRun() func(context.Context) (clusterDriftResult, error) {
	if d.clusterDriftCompare != nil {
		return d.clusterDriftCompare
	}
	return func(context.Context) (clusterDriftResult, error) {
		return clusterDriftResult{}, errClusterDriftNoAnswer
	}
}
