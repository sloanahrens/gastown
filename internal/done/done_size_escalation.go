package done

import "fmt"

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
