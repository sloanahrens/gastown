package manifestdata

import "time"

// The daemon's shutdown budgets, named once. internal/daemon's SIGTERM drain and
// the pod's terminationGracePeriodSeconds both size from these values, so the
// drain and the grace period cannot drift apart (gt-99ys4n).

// LandingStepDrainBound is how long a SIGTERM waits for the landing step and the
// slings in flight before the daemon shuts down without them.
const LandingStepDrainBound = 2 * time.Minute

// DoltStopBudget is the daemon's own graceful-shutdown ceiling: the Dolt SQL
// server's graceful-stop wait before it is killed.
const DoltStopBudget = 30 * time.Second

// HeartbeatSignalGrace is how long a shutdown signal waits for a heartbeat step
// that does not stop on the canceled daemon context.
const HeartbeatSignalGrace = 5 * time.Second

// ShutdownDrainSum is the longest a SIGTERM can take to stop the daemon: the
// landing-step drain bound, then the heartbeat grace, then the Dolt stop. The
// three run one after another, so their sum is the drain.
const ShutdownDrainSum = LandingStepDrainBound + HeartbeatSignalGrace + DoltStopBudget
