package daemon

import "github.com/steveyegge/gastown/internal/update"

// nodeImagePruneDue says whether the control node's image prune may run now, and
// for which run: only an ok run that ended, with an id not yet pruned, and never
// while an update Job is active. A refusal names the reason.
func nodeImagePruneDue(st update.State, lastPrunedRun string, activeJobs []string) (runID string, ok bool, why string) {
	switch {
	case len(activeJobs) > 0:
		return st.RunID, false, "update Job active"
	case st.Outcome == "":
		return st.RunID, false, "no run recorded"
	case st.Outcome != update.OutcomeOK:
		return st.RunID, false, "outcome " + string(st.Outcome) + ", not ok"
	case st.EndedAt.IsZero():
		return st.RunID, false, "ok run has no end time"
	case st.RunID == "":
		return st.RunID, false, "ok run has no run id"
	case st.RunID == lastPrunedRun:
		return st.RunID, false, "already pruned"
	}
	return st.RunID, true, "ok run ended, no update Job active"
}
