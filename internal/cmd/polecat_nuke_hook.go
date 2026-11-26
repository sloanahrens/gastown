package cmd

import (
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

// nukeHookDisposition is the nuke gate's reading of a seat's hook_bead: the
// classifyHookBeadRef disposition, except that a handed-back hook (handedBackHook)
// on a paused seat answers Safe. A failed lookup is never overridden, so the
// classifier's blocker stands.
func nukeHookDisposition(bd issueShower, hookBead, agentState string) polecat.HookBeadDisposition {
	if bd == nil {
		_, disposition := classifyHookBeadRef(nil, hookBead)
		return disposition
	}
	rec := &showRecorder{inner: bd}
	issue, disposition := classifyHookBeadRef(rec, hookBead)
	if rec.err != nil || !handedBackHook(agentState, issue) {
		return disposition
	}
	return polecat.HookBeadDisposition{Safe: true}
}

// showRecorder keeps the error of the lookup it forwards. classifyHookBeadRef
// returns the resolved issue beside the disposition and drops the error, so a
// caller that must not clear on a failed read needs it here.
type showRecorder struct {
	inner issueShower
	err   error
}

func (r *showRecorder) Show(issueID string) (*beads.Issue, error) {
	issue, err := r.inner.Show(issueID)
	r.err = err
	return issue, err
}
