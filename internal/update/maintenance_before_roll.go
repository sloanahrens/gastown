package update

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrMaintenanceDeferred is what Start returns when it declines to run the cycle.
var ErrMaintenanceDeferred = errors.New("maintenance deferred")

// MaintenanceBeforeRoll's outcome words; deferred and failed are the daemon's.
const (
	MaintenanceDone     = "done"
	MaintenanceTimedOut = "timed out"
	MaintenanceDeferred = maintenanceDeferred
	MaintenanceFailed   = maintenanceFailed
)

// MaintenanceSeams is the maintenance cycle as the pre-roll step sees it: Start
// returns with the cycle running or finished and ErrMaintenanceDeferred
// declines it; Wait waits one poll step and advances Now.
type MaintenanceSeams interface {
	Live() bool
	Due(ctx context.Context) (bool, error)
	Start(ctx context.Context) error
	Running() bool
	Wait(ctx context.Context)
	Now() time.Time
}

// MaintenanceOutcome is how the step ended: Outcome is one of the Maintenance
// words above and Detail is why, for the daemon's attention item. The roll
// proceeds on every outcome.
type MaintenanceOutcome struct {
	Outcome string
	Detail  string
}

// MaintenanceBeforeRoll runs the maintenance cycle due before a roll, inside
// bound, starts nothing when maintenance is not live or nothing is due, and
// never returns an error.
func MaintenanceBeforeRoll(ctx context.Context, seams MaintenanceSeams, bound time.Duration) MaintenanceOutcome {
	if !seams.Live() {
		return MaintenanceOutcome{Outcome: MaintenanceDone, Detail: "maintenance is not live"}
	}
	due, err := seams.Due(ctx)
	if err != nil {
		return MaintenanceOutcome{Outcome: MaintenanceFailed, Detail: fmt.Sprintf("read due maintenance: %v", err)}
	}
	if !due {
		return MaintenanceOutcome{Outcome: MaintenanceDone, Detail: "nothing due"}
	}
	start := seams.Now() // a slow Start delays the roll too
	if err := seams.Start(ctx); err != nil {
		if errors.Is(err, ErrMaintenanceDeferred) {
			return MaintenanceOutcome{Outcome: MaintenanceDeferred, Detail: "the daemon declined"}
		}
		return MaintenanceOutcome{Outcome: MaintenanceFailed, Detail: fmt.Sprintf("start maintenance: %v", err)}
	}
	for {
		// A canceled context ends the wait, so a Wait that returns at once on
		// cancellation cannot spin; Running is read first so a cycle finishing
		// on the bound counts as done.
		if err := ctx.Err(); err != nil {
			return MaintenanceOutcome{Outcome: MaintenanceFailed, Detail: fmt.Sprintf("wait for maintenance: %v", err)}
		}
		if !seams.Running() {
			return MaintenanceOutcome{Outcome: MaintenanceDone, Detail: "maintenance finished"}
		}
		if seams.Now().Sub(start) >= bound {
			return MaintenanceOutcome{Outcome: MaintenanceTimedOut, Detail: fmt.Sprintf("still running after %v", bound)}
		}
		seams.Wait(ctx)
	}
}
