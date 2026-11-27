package done

import (
	"errors"
	"fmt"
)

// raiseSizeEscalation hands the size refusal about to be returned to the run's
// escalation route, and prints one line saying whether it was raised (gt-3odsvc).
func raiseSizeEscalation(r *doneRun, refusal error) {
	msg := "no escalation was raised: this run has no escalation route; raise it with gt escalate"
	if r.raiseSizeRefusal != nil {
		msg = fmt.Sprintf("escalated to the overseer: the size refusal on %s is raised", r.issueID)
		if err := r.raiseSizeRefusal(r.issueID, refusal.Error()); err != nil {
			msg = fmt.Sprintf("no escalation was raised: %v; raise it with gt escalate", err)
		}
	}
	fmt.Fprintln(r.stdout(), msg)
}

// endRunOnSubmitFailure ends a run whose submit failed. A size refusal ends the
// turn as ESCALATED, so the escalation raised at the refusal holds the seat on
// its bead; a failure to record that exit is joined to the refusal, never
// replaces it (gt-fkhjrv). Any other error goes to failRun, as before.
func endRunOnSubmitFailure(r *doneRun, cleanupStatus string, err error, failRun func(error) error) error {
	var refusal sizeRefusalError
	if !errors.As(err, &refusal) {
		return failRun(err)
	}
	r.cleanupStatus = cleanupStatus
	if reportErr := reportDone(r, ExitEscalated); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return err
}
