package daemon

import (
	"context"
	"time"
)

// tierSweepRunnerProgressBudget is the bound for the sweep runner's progress
// exec, so a hung exec cannot hold the sweep past its stage budget (gt-aoafom).
// It is a named constant, not a config key. No caller yet: the exec wiring
// comes in a later bead.
const tierSweepRunnerProgressBudget = 30 * time.Second

// tierSweepRunnerStatusBudget is the bound for the status read that
// tierSweepRunnerCollect makes, so a hung status exec cannot hold the wait past
// its stage budget (gt-3g35lf). A status read that times out is the
// errTierSweepStatusUnread the wait goes on with. It is a named constant, not a
// config key.
const tierSweepRunnerStatusBudget = 30 * time.Second

// tierSweepRunnerCollectBudget is the bound for the Collect steps that copy a
// run's result and logs out of its output claim (gt-3g35lf). It is a chosen
// bound, not a measured one. A copy that times out is the copy failure the
// collect returns. It is a named constant, not a config key.
const tierSweepRunnerCollectBudget = 10 * time.Minute

// tierSweepRunnerStepContext bounds one runner step by budget. A parent with no
// deadline gets one no later than now plus budget; a parent whose deadline is
// sooner keeps it, and a canceled parent stays canceled. The caller owns the
// returned cancel func.
func tierSweepRunnerStepContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, budget)
}
