package daemon

import (
	"context"
	"errors"

	"github.com/steveyegge/gastown/internal/attention"
)

// errClusterDriftNoAnswer is the unreadable item's reason when the setting is
// on and no compare is wired: a clear would claim the cluster was read and in
// step, and nothing read it (gt-b6j1k5).
var errClusterDriftNoAnswer = errors.New("no drift compare is wired into the daemon, so the cluster was not read")

// collectClusterDrift turns the drift compare's answer into its items: the
// drift item for a difference and the unreadable item for a read that failed
// or was never wired. It answers nothing while the patrol is off, and calls
// the answer on every heartbeat; the 15 minute cadence is the cache bead's
// (gt-0cdmgq). The error it returns is always nil, so a failed answer applies
// the unreadable item at once instead of keeping the previous drift item, and
// no timer retries (gt-b6j1k5).
func (s *attentionSources) collectClusterDrift(ctx context.Context) ([]attention.Item, error) {
	if s.clusterDriftEnabled == nil || !s.clusterDriftEnabled() {
		return nil, nil
	}
	if s.clusterDriftAnswer == nil {
		return clusterDriftItems(clusterDriftResult{}, errClusterDriftNoAnswer), nil
	}
	res, err := s.clusterDriftAnswer(ctx)
	return clusterDriftItems(res, err), nil
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
